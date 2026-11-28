package agentiface

import (
	"context"
	"errors"

	"github.com/oagentkit/agent/iface"
	"github.com/yi-nology/goagent-kernel/contract"
	"github.com/yi-nology/goagent-runtime/agentloop"
)

// Agentloop 把会话制入口 agentloop.Loop.Run 适配为 iface.Agent
// （三入口适配之二；agentrun 在 agent 仓、contract 边界见 contract.go）。
type Agentloop struct {
	loop *agentloop.Loop
}

var _ iface.Agent = (*Agentloop)(nil)

// NewAgentloop 包装一个已装配的 Loop。
func NewAgentloop(loop *agentloop.Loop) *Agentloop {
	return &Agentloop{loop: loop}
}

// Loop 原生句柄：窄接口不承载的丰富面（RunID/Rounds/Uncertain/PendingCallID
// 与 Approve 续跑）从这里取——iface 的自我约束是「装配与丰富面外置」。
func (a *Agentloop) Loop() *agentloop.Loop { return a.loop }

// Run 实现 iface.Agent。映射规则（与 agent#174 ADR-001 §一对照表逐列对应）：
//
//   - 参数：in.SessionID → 会话锚点（**必填**——会话制入口；一次性形态用
//     agentrun 适配器）；in.Query → user 消息。
//   - 返回：loop 的 Result.Status 本就是 contract 词表值（loop 内部直接用
//     contract 常量），原样透传；Final 同传。loop 错误路径也返回非 nil 的
//     res（带状态），无状态时按 ctx 错分类兜底。
//   - 事件：**显式拒绝** OnEvent——事件面是 kernel eventbus
//     （ADR-0010：Event 共享面 = eventbus），静默丢事件等于撒谎。
//   - 取消/中断：ctx 取消 → loop 收场 → 快照可恢复（W3 #40 判据不动）；
//     挂起（waiting_approval）经原生 Loop().Approve 续跑。
func (a *Agentloop) Run(ctx context.Context, in iface.Input) (iface.Result, error) {
	if a.loop == nil {
		return iface.Result{}, errors.New("agentiface: Agentloop.loop 未装配")
	}
	if in.OnEvent != nil {
		return iface.Result{}, errors.New(
			"agentiface: 事件面是 kernel eventbus（ADR-0010 Event 共享面）——" +
				"本适配器不经 OnEvent 转发；静默丢事件等于对消费方撒谎，请直接订阅 eventbus")
	}
	if in.SessionID == "" {
		return iface.Result{}, errors.New(
			"agentiface: agentloop 是会话制入口——Input.SessionID 必填（一次性形态用 agentrun 适配器）")
	}
	res, err := a.loop.Run(ctx, in.SessionID, in.Query)
	if res == nil {
		if err == nil {
			err = errors.New("agentiface: loop 返回空结果且无错误")
		}
		return iface.Result{Status: statusForErr(err)}, err
	}
	status := res.Status
	switch {
	case status == "" && err == nil:
		return iface.Result{Status: contract.StatusFailed},
			errors.New("agentiface: loop 成功返回但无状态")
	case status == "":
		// 错误路径上 loop 未填状态 ⇒ 按 ctx 错分类兜底（词表不空转）
		status = statusForErr(err)
	case err != nil && !contract.IsTerminalStatus(status):
		// 错误路径上 loop 留的是**过程态**（实测：模型失败时 res.Status=running）
		// ⇒ Result 不能把过程态当结论交付，按 ctx 错分类收口到终态；
		// loop 已自己收口的终态（如 timed_out）尊重不覆盖。
		status = statusForErr(err)
	}
	return iface.Result{Status: status, Final: res.Final}, err
}

// err → contract 词表状态（runtime 侧直接引常量；agent 侧字面量与此对齐，
// 对齐链的两半分别钉在本仓 contract_test 与 agent/agentrun/iface_test）。
func statusForErr(err error) string {
	switch {
	case err == nil:
		return contract.StatusFailed // 无错却无状态：不是成功，按失败暴露
	case errors.Is(err, context.DeadlineExceeded):
		return contract.StatusTimedOut
	case errors.Is(err, context.Canceled):
		return contract.StatusCancelled
	default:
		return contract.StatusFailed
	}
}
