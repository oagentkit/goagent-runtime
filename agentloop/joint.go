// 联合提交接缝（runtime issue #2 主体）。
//
// 病灶与结构消解见 sqliteadapter/joint.go 同名注释：两脸同库时事件行与
// 快照行同生共死。本包只声明**消费方侧窄接口**（宿主存储满足即接入），
// 不 import 任何具体存储——依赖方向纪律与 chat.Model 同款。
package agentloop

import (
	"context"

	"github.com/oagentkit/goagent-kernel/checkpoint"
	"github.com/oagentkit/goagent-kernel/eventbus"
)

// JointCommitter 快照+事件单事务提交面。Config.Joint 非 nil 时：
// emit 改为累计（只构造不发送）、commit 改为联合提交（快照 CAS + 事件行
// 单事务），提交成功后统一广播（广播失败只计 Result.EmitFailed——数据
// 已原子，广播失败=观测延迟不是数据丢失）。nil = 两步旧路径。
type JointCommitter interface {
	CommitWithEvents(ctx context.Context, snap checkpoint.Snapshot, events []eventbus.Event) error
}
