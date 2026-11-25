// k8sjobidem.go —— K8s Job 创建的执行幂等（issue #263）。
//
// # 缺口与修法
//
// 修前 `Exec` 每次调用都生成新作业名（`newK8sJobName()`，纳秒随机）：
// 创建请求发出后若响应丢失（超时/连接中断），Worker 看到失败并重试，
// 重试生成**另一个**名字 ⇒ 集群里同时存在两份执行同一业务动作的 Job。
// 「创建失败」在语义上无法区分「没建成」与「建成了但响应丢了」。
//
// 三件机制：
//
//  1. 稳定 ExecutionID：由内核/宿主给定（backend.Job.ExecutionID），绑定到
//     Job 名（确定性派生，k8sStableJobName）与标签（harness.io/execution-id，
//     Job 对象与 Pod 模板都带）——创建因此具备幂等键。
//  2. 对账先于创建：按派生名 GET，已存在（且执行标识相符）⇒ 不发创建请求，
//     直接等既有作业的终态（返回既有作业的结果，不是重跑）；对账查询本身
//     失败 ⇒ 拒绝创建（fail-closed：无法确认同标识作业是否已存在）。
//  3. 创建结果分级：created / conflict / rejected / uncertain 四态——
//     conflict（409）在幂等路径上视作「并发创建者已建成」（对账与创建之间
//     的竞态窗口），等它的终态；rejected（其余 4xx）= 一定没建成，清理已建
//     策略；**uncertain（传输失败 / 5xx）= 可能已建成 ⇒ 不删 Job 不删策略**
//     （删了可能砍掉已建成作业的禁网面与作业本体；孤儿由 TTL 兜底，全拒
//     策略孤儿只会拒绝不会放行，fail-safe）。
//
// 边界（如实声明）：本机制保证「同一执行标识同时只有一份 Job 在跑」；
// 上一份作业**完成后**的重试（TTL 已回收对象）会创建新作业——跨进程的
// 终态结果持久化与业务级去归宿主与 SideEffect 台账，本仓只兑现机制面。
package containerbackend

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/oekit/pkg/errors"

	"github.com/oagentkit/goagent-kernel/backend"
)

// k8sExecutionIDLabel 执行标识标签：幂等键在集群对象上的绑定点（Job 与 Pod
// 模板都带，对账按它认人）。
const k8sExecutionIDLabel = "harness.io/execution-id"

// k8sStableJobName 执行标识 → DNS-1123 label 作业名：确定性派生（同标识同
// 名，跨进程一致），语义前缀保留可读性，散列尾防碰撞。任何形态的标识都
// 落进词表（非 [a-z0-9-] 一律改写，两端 - 剥掉），长度封顶 63。
func k8sStableJobName(executionID string) string {
	prefix := k8sSanitizeDNSLabel(executionID)
	if len(prefix) > 24 {
		prefix = strings.Trim(prefix[:24], "-")
	}
	if prefix == "" {
		prefix = "exec"
	}
	sum := sha256.Sum256([]byte(executionID))
	return fmt.Sprintf("gak-job-%s-%s", prefix, hex.EncodeToString(sum[:])[:12])
}

// k8sSanitizeDNSLabel 把任意字符串改写成 DNS-1123 label 片段（小写字母数字
// 与 -，两端非 -）。
func k8sSanitizeDNSLabel(s string) string {
	var b []byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-':
			b = append(b, c)
		case c >= 'A' && c <= 'Z':
			b = append(b, c-'A'+'a')
		default:
			b = append(b, '-')
		}
	}
	return strings.Trim(string(b), "-")
}

// k8sReconcileJob 对账先于创建：按派生名查询同标识作业。
//
// found=true 表示集群里已有该执行标识的作业（不发创建请求，等它的终态）；
// uid 是该对象的 metadata.uid（清理时作为删除前置条件，issue #276）。
// 查询失败 ⇒ 拒绝创建（无法确认是否已存在，fail-closed）；命中同名对象但
// 执行标识不符 ⇒ 拒绝（可能是别人的对象，绝不对着它等终态）。
func (k *K8sJob) k8sReconcileJob(ctx context.Context, ns, name, executionID string) (found bool, uid string, err error) {
	job, found, err := k.getJob(ctx, ns, name)
	if err != nil {
		return false, "", errors.Wrap(ErrAPICall,
			"对账查询失败：无法确认同标识作业是否已存在，拒绝创建（fail-closed）: "+err.Error())
	}
	if !found {
		return false, "", nil
	}
	if got := job.Metadata.Labels[k8sExecutionIDLabel]; got != executionID {
		return false, "", errors.Wrap(ErrAPICall, fmt.Sprintf(
			"对账命中同名作业 %s/%s 但执行标识不符（got=%q want=%q）——拒绝创建，不对别人的对象等终态",
			ns, name, got, executionID))
	}
	return true, job.Metadata.UID, nil
}

// k8sCreateOutcome 创建请求的落点分级（「没建成」与「建成了但响应丢了」
// 必须分得开——修前这两者共用一个 error）。
type k8sCreateOutcome uint8

const (
	// k8sCreated 建成（2xx）。
	k8sCreated k8sCreateOutcome = iota
	// k8sCreateConflict 同名对象已存在（409）：幂等路径上 = 并发创建者已建成。
	k8sCreateConflict
	// k8sCreateRejected apiserver 明确拒绝（其余 4xx）：一定没建成。
	k8sCreateRejected
	// k8sCreateUncertain 结果不明（传输失败 / 5xx）：可能已建成，响应丢了。
	k8sCreateUncertain
)

// createJob 创建 Job 并回报落点分级与对象 UID。
//
// uid 只在 2xx 且应答可解析时非空：清理要用它做删除前置条件（issue #276）。
// 应答读不出 UID 不影响执行本身（Job 已建成），但会让显式清理退化到 TTL
// ——宁可不删，也不按名字误删后来者。
func (k *K8sJob) createJob(ctx context.Context, ns, name string, j backend.Job) (k8sCreateOutcome, string, error) {
	resp, err := k.call(ctx, http.MethodPost, k8sJobsPath(ns, ""), buildK8sJob(name, ns, j, k), nil)
	if err != nil {
		return k8sCreateUncertain, "", err // 传输失败：请求可能已到达服务端
	}
	if resp.status >= 200 && resp.status < 300 {
		var out k8sJobResponse
		if jerr := json.Unmarshal(resp.body, &out); jerr != nil {
			return k8sCreated, "", nil // 已建成但读不出 UID：清理交给 TTL
		}
		return k8sCreated, out.Metadata.UID, nil
	}
	if resp.status == http.StatusConflict {
		return k8sCreateConflict, "", k8sAPIError("创建 Job", resp)
	}
	if resp.status >= 500 {
		return k8sCreateUncertain, "", k8sAPIError("创建 Job", resp) // 5xx：可能已受理
	}
	return k8sCreateRejected, "", k8sAPIError("创建 Job", resp)
}

// k8sPolicyIsOursDenyAll 判同名既有策略是否就是本作业的全拒策略（重试路径上
// 策略重建遇 409 时复用它；不是 ⇒ 拒绝，绝不在别人的对象上收场）。
//
// 四层都判：对象名、对象标签、Pod 选择器只选中本作业、policyTypes=[Egress]
// 且 egress 为空（全拒）。读回校验（verifyNetworkPolicy）也用同一判据——
// 创建成功后的落库形态必须与声明面逐层一致。
func k8sPolicyIsOursDenyAll(p k8sNetworkPolicy, name string) bool {
	if p.Metadata.Name != name || p.Metadata.Labels[k8sJobLabel] != name {
		return false
	}
	sel := p.Spec.PodSelector
	if len(sel.MatchExpressions) > 0 || len(sel.MatchLabels) != 1 || sel.MatchLabels[k8sJobLabel] != name {
		return false
	}
	return slices.Equal(p.Spec.PolicyTypes, []string{"Egress"}) && len(p.Spec.Egress) == 0
}
