// mountpolicy.go —— 挂载白名单（issue #187）：隔离承诺的 bind 侧兑现判据。
//
// Docker 实现唯一兑现的出站隔离是 network=none（见 docker.go 的 Egress 自报），
// 而一个 bind 就能把它整个绕开。判据与形状在这里，闸门在 Docker.Exec
// （checkMounts，先于任何引擎调用）。
package containerbackend

import (
	"fmt"
	"path"
	"strings"

	"github.com/oekit/pkg/errors"

	"github.com/oagentkit/goagent-kernel/backend"
)

// MountPolicy 挂载白名单（issue #187）。
//
// # 为什么必须有它
//
// 本实现唯一兑现的出站隔离是 network=none，而一个 bind 就能把它整个绕开：
// 挂 /var/run/docker.sock 等于把宿主容器控制权交给作业（断不断网毫无意义），
// 挂 / 、/etc、$HOME/.ssh、$HOME/.aws 等于直接交出凭据。backend.Job.Mounts
// 的来源若不做判定，「隔离档位声明」与「实际执行」就是脱节的。
//
// # 判据（不满足 ⇒ 创建阶段拒绝，绝不降级——fail-closed）
//
//   - Source 命中 AllowedSources 之一（目录形态 = 该目录整棵子树）；
//   - 读写挂载必须显式 AllowWritable——缺省一切挂载只读（可写挂载是暴露面）；
//   - 零值策略（未装配）= 空白名单 = 拒绝一切挂载；
//   - 形态判据：bind 字符串由本实现拼成 `Source:Target[:ro]`，两端出现「:」
//     会被引擎当 bind 选项解析（Target 塞 ":rw" 等于改写读写档），拒绝；
//     相对来源会被引擎当命名卷解释（绕开宿主路径控制），拒绝。
//
// # 已知边界（如实声明，不假装覆盖）
//
// 本判据是 Clean 后的字符串前缀匹配：宿主路径里的符号链接不在判据内
// （/workspace/link → /etc 这类形态要在运行时按真实路径解析才可判），
// 那是容器引擎 / Provider 职责面（上游 oagentkit/guardx#193 的原语位）。
type MountPolicy struct {
	// AllowedSources 允许的宿主侧来源路径（绝对路径；目录形态含整棵子树）。
	AllowedSources []string
	// AllowWritable 是否放开读写挂载（缺省 false = 只接受 ReadOnly 声明）。
	AllowWritable bool
}

// check 判一次挂载声明是否落在策略内。
func (p MountPolicy) check(m backend.Mount) error {
	if strings.Contains(m.Source, ":") || strings.Contains(m.Target, ":") {
		return fmt.Errorf("挂载 %q→%q 含「:」——会被引擎当 bind 选项解析", m.Source, m.Target)
	}
	src := path.Clean(m.Source)
	if !path.IsAbs(src) {
		return fmt.Errorf("挂载来源 %q 不是绝对路径（相对形态会被引擎当命名卷解释）", m.Source)
	}
	if !path.IsAbs(path.Clean(m.Target)) {
		return fmt.Errorf("挂载目标 %q 不是绝对路径", m.Target)
	}
	if !m.ReadOnly && !p.AllowWritable {
		return fmt.Errorf("挂载 %q→%q 是读写挂载而策略未放开（缺省只读）", m.Source, m.Target)
	}
	for _, a := range p.AllowedSources {
		allow := path.Clean(a)
		if !path.IsAbs(allow) {
			continue // 白名单项自身不合法：跳过，不放它当通配
		}
		if src == allow || strings.HasPrefix(src, allow+"/") {
			return nil
		}
	}
	return fmt.Errorf("挂载来源 %q 不在白名单内（允许：%v）", m.Source, p.AllowedSources)
}

// checkMounts 挂载判据闸：任何一条不满足即在创建阶段拒绝（先于引擎调用）。
func (d *Docker) checkMounts(j backend.Job) error {
	for _, m := range j.Mounts {
		if err := d.mounts.check(m); err != nil {
			return errors.Wrap(ErrMountDenied, err.Error())
		}
	}
	return nil
}
