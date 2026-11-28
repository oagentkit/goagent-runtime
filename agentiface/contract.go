package agentiface

import (
	"context"
	"errors"
	"fmt"

	"github.com/oagentkit/agent/iface"
	"github.com/yi-nology/goagent-kernel/contract"
)

// Contract 把「宿主经 contract 边界驱动的编排」适配为 iface.Agent
// （三入口适配之三）。
//
// 实测勘误（agent#174 ADR-001 §一）：contract 包**没有** Run/Step 方法——
// 它是编排边界接口（RunContext/StepSink/Tools/Clock）+ 状态词表，本身不运行。
// 所以本适配器的形态是：宿主注入读面 RC（领域结构已适配进去）与执行体 Exec，
// 适配器只负责「把 contract 状态词表读成 Result、把未收口的执行补到终态」。
type Contract struct {
	// RC 宿主编排读面：Exec 推进后从它读 Status（词表归 contract）。
	RC contract.RunContext
	// Exec 宿主执行体：跑一次编排（按 contract 纪律推进状态、产出终答）。
	// 起手态（created→running）由 Exec 负责；适配器只兜**收口**。
	Exec func(ctx context.Context, in iface.Input) (final string, err error)
}

var _ iface.Agent = (*Contract)(nil)

// Run 实现 iface.Agent。
//
//   - 参数：in.Query → Exec；in.SessionID 透传给 Exec（RC 已绑定会话，
//     本适配器不解释）；OnEvent 显式拒绝（理由见包文档）。
//   - 返回：RC.Status 原样进 Result.Status；Exec 未把状态推到终态时，
//     按 ctx 错分类补 failed/timed_out/cancelled（宿主状态机拒绝补态则
//     如实上抛，不吞）。
//   - 取消/中断：ctx 错分类 + 词表 cancelled/timed_out 取值；更深的
//     中断语义（interrupted 等）由宿主状态机自己管，本适配器不发明。
func (c *Contract) Run(ctx context.Context, in iface.Input) (iface.Result, error) {
	if c.RC == nil {
		return iface.Result{}, errors.New("agentiface: Contract.RC 未装配")
	}
	if c.Exec == nil {
		return iface.Result{}, errors.New("agentiface: Contract.Exec 未装配")
	}
	if in.OnEvent != nil {
		return iface.Result{}, errors.New(
			"agentiface: 事件面是 kernel eventbus（ADR-0010 Event 共享面）——" +
				"本适配器不经 OnEvent 转发；静默丢事件等于对消费方撒谎，请直接订阅 eventbus")
	}

	final, err := c.Exec(ctx, in)
	status := c.RC.Status()

	// 收口：执行体没把状态推到终态 ⇒ 按结果补（终态已到则尊重宿主状态机）
	if !contract.IsTerminalStatus(status) {
		want := contract.StatusSucceeded
		if err != nil {
			want = statusForErr(err)
		}
		if serr := c.RC.SetStatus(want); serr != nil {
			if err != nil {
				return iface.Result{Status: c.RC.Status()},
					fmt.Errorf("执行体: %w（且补终态 %s 被宿主状态机拒绝: %v）", err, want, serr)
			}
			return iface.Result{Status: c.RC.Status()},
				fmt.Errorf("补终态 %s 被宿主状态机拒绝: %v", want, serr)
		}
		status = want
	}
	if err != nil {
		return iface.Result{Status: status}, err
	}
	return iface.Result{Status: status, Final: final}, nil
}
