// journal.go —— 工具调用账本（invocation）的接线面。
//
// # 它补的是哪一段
//
// 快照面保证「对话历史不撕裂」，**不**保证「这次工具调用是谁」。进程死在
// 「模型已发出调用、结果还没回填」那一段时，恢复点的历史里**没有**那次调用
// ⇒ 模型**重新生成**一次 ⇒ 调用 ID 变了 ⇒ 幂等键跟着变 ⇒ 闸认不出是同一次
// 动作 ⇒ 同一业务动作再施加一遍。
//
// 账本补的就是这一段：**执行外部工具之前**把不可变身份（callID + 工具名 +
// 参数）落到持久账本，恢复时按账本续跑，**不再问模型要那次调用**。
//
// # 分工（判据在内核，本包只按判据走）
//
//	账本（invocation）    判「这次调用是谁、参数是什么、走到哪一步了」
//	副作用闸（sideeffect）判「这个逻辑动作是不是已经施加过」
//
// 合成才闭合崩溃窗口：账本保**身份**（不重新生成 ⇒ 键不变），闸保**至多
// 一次**（键不变 ⇒ 重放取回既有结果）。只有闸会重复施加；只有账本会盲目重做。
//
// # 本包不判「恢复时该怎么办」
//
// 那是内核的 invocation.Recovery（纯判据）。本包只做四件事：执行前落
// planned；按执行结果推进（executing → applied / uncertain / failed）；结果
// 回填进历史并提交后收口（applied → result_recorded）；恢复时按账本续跑。
//
// 「远端到底生效没有」只有宿主知道 ⇒ 宿主可注入 Reconcile 钩子裁决；没注入
// 时本包**不猜**：结果未知的账目按「必须先对账」回喂模型并记进
// Result.Uncertain（与闸判 uncertain 的既有语义同款）。
//
// # 未装账本时行为逐字不变
//
// Journal 为 nil ⇒ 本文件全部是**空操作**，快照节奏与旧版一致。账本是**可选
// 接缝**，不是装配前提。
package agentloop

import (
	"context"

	"github.com/oekit/pkg/errors"

	"github.com/oagentkit/goagent-kernel/contract"
	"github.com/oagentkit/goagent-kernel/invocation"

	"github.com/oagentkit/goagent-runtime/chat"
	"github.com/oagentkit/goagent-runtime/pipeline"
)

// journalPlan 落一条待执行调用（身份一经落账不可变）。
//
// 幂等：同身份重复落账 = 无操作（恢复重放同一计划不得报错）；身份不一致 ⇒
// invocation.ErrIdentityConflict（那是**另一个动作**）。
func (l *Loop) journalPlan(ctx context.Context, sessionID string, call chat.ToolCall) error {
	if l.cfg.Journal == nil {
		return nil
	}
	e := invocation.Entry{
		SessionID:  sessionID,
		CallID:     call.ID,
		Tool:       call.Name,
		ArgsDigest: invocation.Digest([]byte(call.Arguments)),
		Args:       []byte(call.Arguments),
		State:      invocation.StatePlanned,
	}
	if err := l.cfg.Journal.Plan(ctx, e); err != nil {
		return errors.Wrap(err, "工具调用落账失败", "session", sessionID, "call_id", call.ID)
	}
	return nil
}

// journalAdvance 推进账目（薄封装：调用点只关心「推进到哪」）。
func (l *Loop) journalAdvance(ctx context.Context, sessionID, callID string, to invocation.State, u invocation.Update) error {
	if l.cfg.Journal == nil {
		return nil
	}
	if _, err := l.cfg.Journal.Advance(ctx, sessionID, callID, to, u); err != nil {
		return errors.Wrap(err, "账目推进失败",
			"session", sessionID, "call_id", callID, "to", string(to))
	}
	return nil
}

// journalOutcome 把一次执行的结果落成账目状态。
//
//   - applied / replayed → applied：副作用已施加，结果在账上；
//   - uncertain → uncertain：结果未知 ⇒ 须先对账，**禁止盲目重试**；
//   - 其余（refused / failed / contended）→ failed：**确定未施加**——闸没轮上、
//     执行体报错、工具未注册，三者都不是「不知道」，是「确定没做」。
//
// ★ failed 与 rejected 是**两个决策来源**：rejected 是**审批闸**的判决，failed
//
//	是**执行面**的判决。宿主接线时暴露过这个缺口——词表原先端点没有「获批但
//	跑失败」这一档，硬塞进 rejected 会让账目读者分不清「没获批」与「获批但
//	失败」（两者补偿口径完全不同）。
func (l *Loop) journalOutcome(ctx context.Context, sessionID, callID string, out pipeline.Outcome) error {
	switch out.Kind {
	case pipeline.KindApplied, pipeline.KindReplayed:
		return l.journalAdvance(ctx, sessionID, callID, invocation.StateApplied,
			invocation.Update{Result: []byte(out.Content)})
	case pipeline.KindUncertain:
		return l.journalAdvance(ctx, sessionID, callID, invocation.StateUncertain,
			invocation.Update{Reason: out.Content})
	default:
		return l.journalAdvance(ctx, sessionID, callID, invocation.StateFailed,
			invocation.Update{Reason: out.Content})
	}
}

// journalFinish 结果已回填进历史 ⇒ 账目收口（applied → result_recorded，终态）。
//
// 只有 applied 那条收口：failed 已是终态（确定未施加，不再重做）；uncertain
// 还要等裁决（uncertain → applied / executing 是内核的合法转移）。
func (l *Loop) journalFinish(ctx context.Context, sessionID, callID string) error {
	if l.cfg.Journal == nil {
		return nil
	}
	e, err := l.cfg.Journal.Get(ctx, sessionID, callID)
	if err != nil {
		return errors.Wrap(err, "读账目失败", "session", sessionID, "call_id", callID)
	}
	if e.State != invocation.StateApplied {
		return nil
	}
	return l.journalAdvance(ctx, sessionID, callID, invocation.StateResultRecorded, invocation.Update{})
}

// journalLegitimate 账本背书的**合法未完成调用**集合（callID 集合）。
//
// 这是 verifyHistory 的放行面：历史里留着未答复的工具调用只有两种可能——
// ① 崩溃窗口的合法产物（账本说它还没收口）；② 存储被外部篡改（账本里根本没
// 有它）。前者放行、后者 fail-closed，判据就在「账本认不认」。
//
// 未装账本时返回空集合 ⇒ verifyHistory 旧行为逐字不变。
func (l *Loop) journalLegitimate(ctx context.Context, sessionID string) (map[string]bool, error) {
	if l.cfg.Journal == nil {
		return nil, nil
	}
	list, err := l.cfg.Journal.List(ctx, sessionID)
	if err != nil {
		return nil, errors.Wrap(err, "读账本失败", "session", sessionID)
	}
	out := map[string]bool{}
	for _, e := range list {
		if invocation.Unfinished(e.State) {
			out[e.CallID] = true
		}
	}
	return out, nil
}

// journalEntry 取回账目；账上没有就**补落**（身份在历史里 ⇒ 参数齐全）。
//
// 「补落」不是放行：账本缺条目只说明**落账那一步没走到**，调用身份仍完整记
// 在历史里（callID + 工具名 + 参数）⇒ 补落后照常走同一套判据。身份不一致 ⇒
// invocation.ErrIdentityConflict（那是另一个动作，不是重试）。
func (l *Loop) journalEntry(ctx context.Context, sessionID string, call chat.ToolCall) (invocation.Entry, error) {
	e, err := l.cfg.Journal.Get(ctx, sessionID, call.ID)
	if err == nil {
		return e, nil
	}
	if !errors.Is(err, invocation.ErrNotFound) {
		return invocation.Entry{}, errors.Wrap(err, "读账目失败",
			"session", sessionID, "call_id", call.ID)
	}
	if err := l.journalPlan(ctx, sessionID, call); err != nil {
		return invocation.Entry{}, err
	}
	e, err = l.cfg.Journal.Get(ctx, sessionID, call.ID)
	if err != nil {
		return invocation.Entry{}, errors.Wrap(err, "补落账目后仍读不到",
			"session", sessionID, "call_id", call.ID)
	}
	return e, nil
}

// resumePending 按账本续跑未完成的调用（恢复面）。
//
// ★ 为什么在**问模型之前**做：历史里有未答复的工具调用时，模型协议不接受把
//
//	它悬着继续对话（下一轮模型看到的是「assistant 要了工具但没有结果」）。
//	先续跑 ⇒ 历史自洽，再让模型接着说。
//
// ★ 为什么**工作清单由账本驱动**（而不是「历史里未答复的调用」）：
//
//	账本领先历史的情形（崩在「落账之后、提交历史之前」——那次调用根本不在
//	历史里）只有账本知道 ⇒ 清单必须从账本出。反过来（历史领先账本）已被
//	落账顺序排除：**先落账、后提交历史**⇒ 凡是进了历史的调用，账上必有。
//
// 处置完全按内核的 invocation.Recovery 判（本包不自推转移表——自推就是第二份
// 真相，内核改了转移表这份不会跟着变）：
//
//	execute   → 执行（账目说还没开始）
//	record    → 结果已在账上 ⇒ **回填**（不重做：重做会把已生效的副作用再施加一遍）
//	reconcile → 对账（executing ⇒ 交给副作用闸裁决；uncertain ⇒ 宿主裁决，没
//	             裁决钩子则不盲重试）
//	none      → 账目已收口（rejected / failed）⇒ 把「未施加」回喂给模型
func (l *Loop) resumePending(ctx context.Context, st *runState) error {
	list, err := l.cfg.Journal.List(ctx, st.sessionID)
	if err != nil {
		return errors.Wrap(err, "读账本失败", "session", st.sessionID)
	}
	var resumed []string
	for _, e := range list {
		if !invocation.Unfinished(e.State) {
			continue
		}
		call := chat.ToolCall{ID: e.CallID, Name: e.Tool, Arguments: string(e.Args)}
		// 这次调用不在历史里（崩在落账之后、提交历史之前）⇒ 补一条 assistant
		// 请求消息：身份与参数全在账上，补出来就是原样。
		if !historyHasCall(st.messages, e.CallID) {
			st.messages = append(st.messages, chat.Message{
				Role:      chat.RoleAssistant,
				ToolCalls: []chat.ToolCall{call},
			})
		}
		if err := l.resumeOne(ctx, st, call, e); err != nil {
			return err
		}
		resumed = append(resumed, call.ID)
	}
	if len(resumed) == 0 {
		return nil
	}
	// 续跑完提交一次（回填与提交同段 ⇒ 崩溃窗口的两端各自自洽）。
	if err := l.commit(ctx, st, contract.StatusRunning); err != nil {
		return err
	}
	// 结果已回填进历史 ⇒ 账目收口（applied → result_recorded，终态）。
	for _, id := range resumed {
		if err := l.journalFinish(ctx, st.sessionID, id); err != nil {
			return err
		}
	}
	return nil
}

// resumeOne 按账本处置一条未收口的账目（判据来自内核 invocation.Recovery）。
func (l *Loop) resumeOne(ctx context.Context, st *runState, call chat.ToolCall, e invocation.Entry) error {
	switch invocation.Recovery(e) {
	case invocation.ActionExecute:
		return l.executeCall(ctx, st, call)
	case invocation.ActionRecord:
		return l.fillCall(ctx, st, call, e)
	case invocation.ActionReconcile:
		return l.reconcileCall(ctx, st, call, e)
	default:
		// 账目说已收口、历史里却没答复——只有一种可能是两边对不上（账本说
		// 结果已回填、历史里没有）⇒ fail-closed，不猜。
		if e.State == invocation.StateResultRecorded {
			return errors.Newf("账目说结果已回填进历史，历史里却没有这次调用（两边对不上）: call_id=%s", call.ID)
		}
		return l.closeCall(ctx, st, call, e, "not_applied")
	}
}

// executeCall 执行一次调用，并把结果落成账目 + 回填进历史。
//
// 账本推进三步（判据在内核，本包只按判据走）：
//
//  1. 执行前 → executing：**崩溃窗口的合法形态**——进程死在这一步时，远端做
//     没做不知道，但账目本身合法（不是历史损坏）⇒ 恢复时据此对账；
//  2. 执行后 → applied（结果在账上）/ uncertain（结果未知）/ failed（确定未
//     施加）；
//  3. 落 applied 的那条在**结果回填进历史并提交之后** → result_recorded。
//
// 「能不能重做」不由本函数判：副作用只施加一次由**闸**保证（键不变 ⇒ 重放
// 取回既有结果，不重复施加）。
func (l *Loop) executeCall(ctx context.Context, st *runState, call chat.ToolCall) error {
	if err := l.journalAdvance(ctx, st.sessionID, call.ID, invocation.StateExecuting, invocation.Update{}); err != nil {
		return err
	}
	out := l.cfg.Tools.Execute(ctx, st.sessionID, call)
	st.used.ToolCalls++
	st.used.Tools[call.Name]++
	st.res.ToolCalls = st.used.ToolCalls
	if err := l.journalOutcome(ctx, st.sessionID, call.ID, out); err != nil {
		return err
	}
	st.messages = append(st.messages, chat.Message{
		Role:       chat.RoleTool,
		Content:    out.Content,
		ToolCallID: call.ID,
	})
	st.done = append(st.done, call.ID)
	l.emit(ctx, st, EventToolResult, map[string]any{
		"call_id": call.ID, "tool": call.Name,
		"outcome": string(out.Kind), "content": out.Content,
		"bytes": len(out.Content),
	})
	if out.Kind == pipeline.KindUncertain {
		st.res.Uncertain = append(st.res.Uncertain, call.ID)
	}
	return nil
}

// fillCall 把账上的结果回填进历史（**不重做**）。
//
// 账目说「已施加、结果在账上，只差写回会话历史」⇒ 恢复时要做的是回填，不是
// 重做。重做会把一次已生效的副作用再施加一遍。
func (l *Loop) fillCall(ctx context.Context, st *runState, call chat.ToolCall, e invocation.Entry) error {
	content := string(e.Result)
	st.messages = append(st.messages, chat.Message{
		Role:       chat.RoleTool,
		Content:    content,
		ToolCallID: call.ID,
	})
	st.done = append(st.done, call.ID)
	l.emit(ctx, st, EventToolResult, map[string]any{
		"call_id": call.ID, "tool": call.Name,
		"outcome": string(pipeline.KindApplied), "content": content,
		"bytes": len(content),
	})
	return nil
}

// closeCall 账目已收口（rejected / failed / uncertain）⇒ 把结论回喂给模型。
//
// 对应「执行面判了结论、但结果还没回填进历史就崩了」：账目说确定没做（或不
// 知道）⇒ 历史该有一条结果回填（模型据此改道），但**不是**重做。
func (l *Loop) closeCall(ctx context.Context, st *runState, call chat.ToolCall, e invocation.Entry, outcome string) error {
	content := e.Reason
	if content == "" {
		content = "这次调用已收口（副作用未施加）。"
	}
	st.messages = append(st.messages, chat.Message{
		Role:       chat.RoleTool,
		Content:    content,
		ToolCallID: call.ID,
	})
	st.done = append(st.done, call.ID)
	l.emit(ctx, st, EventToolResult, map[string]any{
		"call_id": call.ID, "tool": call.Name,
		"outcome": outcome, "content": content,
		"bytes": len(content),
	})
	return nil
}

// reconcileCall 对账（账目说「执行中」或「结果未知」）。
//
// executing：进程死在执行途中 ⇒ 不知道远端做没做 ⇒ **重跑一次交给副作用闸裁
// 决**（闸认出「已生效」就取回既有结果，没生效才真施加）。这不是盲重试：闸
// 就是「至多施加一次」的那道闸，重跑时键不变 ⇒ 闸不会重复施加。
//
// uncertain：闸自己判了「结果未知」⇒ 闸**不授予执行权**（盲重试被挡）。远端
// 到底生效没有只有宿主知道 ⇒ 宿主可注入 Reconcile 钩子裁决；没注入时
// **不猜**：把「必须先对账」回喂给模型并记进 Result.Uncertain。
func (l *Loop) reconcileCall(ctx context.Context, st *runState, call chat.ToolCall, e invocation.Entry) error {
	if e.State == invocation.StateExecuting {
		return l.executeCall(ctx, st, call)
	}
	if l.cfg.Reconcile == nil {
		st.res.Uncertain = append(st.res.Uncertain, call.ID)
		return l.closeCall(ctx, st, call, e, "uncertain")
	}
	to, result, err := l.cfg.Reconcile(ctx, st.sessionID, e)
	if err != nil {
		return errors.Wrap(err, "对账裁决失败", "session", st.sessionID, "call_id", call.ID)
	}
	switch to {
	case invocation.StateApplied:
		// 裁决「确实已生效」⇒ 结果落账并回填历史（不重做）。
		if err := l.journalAdvance(ctx, st.sessionID, call.ID, invocation.StateApplied,
			invocation.Update{Result: result}); err != nil {
			return err
		}
		e.Result = result
		return l.fillCall(ctx, st, call, e)
	case invocation.StateExecuting:
		// 裁决「未生效」⇒ 允许重做（内核词表：uncertain → executing 是合法转移）。
		if err := l.journalAdvance(ctx, st.sessionID, call.ID, invocation.StateExecuting,
			invocation.Update{}); err != nil {
			return err
		}
		return l.executeCall(ctx, st, call)
	default:
		return errors.Newf("对账裁决给出未知的目标状态: %s", to)
	}
}

// historyHasCall 历史里有没有这次调用（已答复或未答复都算「有这条请求」）。
func historyHasCall(messages []chat.Message, callID string) bool {
	for _, m := range messages {
		if m.Role != chat.RoleAssistant {
			continue
		}
		for _, c := range m.ToolCalls {
			if c.ID == callID {
				return true
			}
		}
	}
	return false
}

// unansweredCalls 历史里**未答复**的工具调用（按出现顺序）。
//
// 这是崩溃窗口的产物：assistant 已把调用写进历史，结果还没回填。正常路径下
// 这个集合恒为空（回填与提交在同一段顺序代码里）。
func unansweredCalls(messages []chat.Message) []chat.ToolCall {
	answered := map[string]bool{}
	for _, m := range messages {
		if m.Role == chat.RoleTool {
			answered[m.ToolCallID] = true
		}
	}
	var out []chat.ToolCall
	for _, m := range messages {
		if m.Role != chat.RoleAssistant {
			continue
		}
		for _, c := range m.ToolCalls {
			if !answered[c.ID] {
				out = append(out, c)
			}
		}
	}
	return out
}
