// 审批闸的挂起/恢复形态（issue #4 第二切片）。
//
// 同步闸（第一切片）在闸内等待；本文件提供异步形态：闸返回 ApprovalSuspend
// ⇒ 提交 waiting_approval 快照后 Run 返回，宿主离线收集决策后经 Approve 落决策
// 续跑。挂起提交的快照 = 最后一条 assistant 消息存在未答复调用 + 状态
// waiting_approval（verifyHistory 的放行面只认这一种形态，更早消息的
// 未答复仍是篡改，fail-closed 不变）。状态词面=kernel contract 既有
// StatusWaitingApproval（Open 态），不 runtime 侧自造。
package agentloop

import (
	"context"

	"github.com/oekit/pkg/errors"

	"github.com/oagentkit/goagent-kernel/contract"
	"github.com/oagentkit/goagent-kernel/invocation"

	"github.com/oagentkit/goagent-runtime/chat"
	"github.com/oagentkit/goagent-runtime/pipeline"
)

// StatusAwaitingApproval 会话挂起等审批的快照/结果状态。对齐 kernel
// contract 既有词表（StatusWaitingApproval，Open 态）——同义异名统一，
// 不再 runtime 侧自造词面（挂起=等待人工决策的 Open 态，非终态）。
const StatusAwaitingApproval = contract.StatusWaitingApproval

// Approval 审批闸三态决策。
type Approval string

const (
	// ApprovalApprove 批准：该次调用直通执行。
	ApprovalApprove Approval = "approve"
	// ApprovalDeny 拒绝：副作用不施加，拒绝以工具观察回喂模型（可改道）。
	ApprovalDeny Approval = "deny"
	// ApprovalSuspend 挂起：提交 awaiting 快照后返回；宿主稍后经 Approve
	// 落决策再续跑。未知决策值同义保守挂起（fail-closed 方向）。
	ApprovalSuspend Approval = "suspend"
)

// ApprovalGateFunc 工具执行前的人工审批闸（宿主注入面）。
//
// 三态决策：ApprovalApprove 直通执行；ApprovalDeny 拒绝（副作用不施加，
// 以工具观察回喂模型）；ApprovalSuspend 挂起（提交 awaiting 快照后 Run
// 返回，宿主离线收集决策后经 Approve 落决策续跑）。err ≠ nil ⇒ fail-closed
// 本轮失败上抛（闸失败 = 无法证明获准）。
type ApprovalGateFunc func(ctx context.Context, sessionID string, call chat.ToolCall) (Approval, error)

// denyObservation 拒绝时回喂模型的工具观察文本。
const denyObservation = "工具调用被人工审批拒绝，副作用未施加；请调整方案或改用其他工具。"

// pendingCallID 找第一个未答复的工具调用（挂起形态恒发生在最后一条
// assistant 消息的调用批里——verifyHistory 的放行面保证了这一点）。
func pendingCallID(messages []chat.Message) string {
	answered := map[string]bool{}
	for _, m := range messages {
		if m.Role == chat.RoleTool {
			answered[m.ToolCallID] = true
		}
	}
	for _, m := range messages {
		if m.Role != chat.RoleAssistant {
			continue
		}
		for _, c := range m.ToolCalls {
			if !answered[c.ID] {
				return c.ID
			}
		}
	}
	return ""
}

// toolNameOf 从历史找 callID 对应的工具名（挂起快照的 assistant 消息里
// 必有——找不到是存储被外部篡改的形态，返回空串由调用方 fail-closed）。
func toolNameOf(messages []chat.Message, callID string) string {
	for _, m := range messages {
		if m.Role != chat.RoleAssistant {
			continue
		}
		for _, c := range m.ToolCalls {
			if c.ID == callID {
				return c.Name
			}
		}
	}
	return ""
}

// pendingToolCall 找历史里 callID 对应的**完整**工具调用（含参数）。
//
// 为什么必须连参数一起取回：用户批准的是「带参数的具体动作」（deploy 哪个
// 目标、execute_sql 哪条语句、delete 哪个资源）。只取回 ID 与工具名 ⇒
// 执行体收到**空参数** ⇒ 施加的根本不是获批的那个动作（审批与执行脱钩）。
//
// 取回的是**快照里的原样调用**（快照由存储侧 CAS 保护、历史完整性有
// verifyHistory 把关）⇒ 执行的就是用户看到并批准的那次调用。
func pendingToolCall(messages []chat.Message, callID string) (chat.ToolCall, bool) {
	for _, m := range messages {
		if m.Role != chat.RoleAssistant {
			continue
		}
		for _, c := range m.ToolCalls {
			if c.ID == callID {
				return c, true
			}
		}
	}
	return chat.ToolCall{}, false
}

// Approve 落审批决策并续跑挂起中的会话。
//
// approved=true ⇒ 挂起的调用走标准施加面执行（幂等闸/租约/重试语义与
// drive 内直通路径完全一致）；approved=false ⇒ 拒绝观察回喂模型。两种
// 去向都接主驱动循环续跑到终态。callID 必须精确匹配挂起的调用
// （fail-closed：不匹配拒绝落决策——防宿主拿着陈旧决策误放行新调用）。
func (l *Loop) Approve(ctx context.Context, sessionID string, callID string, approved bool) (*Result, error) {
	if err := l.committer.SeedFromStore(ctx, sessionID); err != nil {
		return nil, errors.Wrap(err, "回灌快照序号基线失败", "session", sessionID)
	}
	snap, messages, done, err := l.loadHistory(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if snap == nil || snap.Status != StatusAwaitingApproval {
		return nil, errors.Newf("会话不在挂起等审批状态（无待答复调用）: %s", sessionID)
	}
	pending := pendingCallID(messages)
	if pending == "" {
		return nil, errors.Newf("挂起快照里找不到待答复调用: %s", sessionID)
	}
	if pending != callID {
		return nil, errors.Newf("callID 不匹配挂起调用: 挂起=%s 传入=%s", pending, callID)
	}

	st := newRunState(sessionID, messages, done, snap)
	res := st.res
	if approved {
		// 恢复**完整**调用（含参数）：获批的是带参数的具体动作，只恢复
		// ID+工具名会让执行体收到空参数（施加的不是获批的动作）。
		call, found := pendingToolCall(messages, pending)
		if !found {
			return nil, errors.Newf("挂起调用在历史里找不到: %s", pending)
		}
		name := call.Name
		// 账本：审批决策已落 ⇒ 记「已获批准」；执行前 → executing
		//（**崩溃窗口的合法形态**：进程死在这一段时远端做没做不知道，
		//  但账目本身合法 ⇒ 恢复时据此对账）。
		if l.cfg.Journal != nil {
			if _, err := l.journalEntry(ctx, sessionID, call); err != nil {
				return nil, err
			}
			if err := l.journalAdvance(ctx, sessionID, call.ID,
				invocation.StateApproved, invocation.Update{}); err != nil {
				return nil, err
			}
			if err := l.journalAdvance(ctx, sessionID, call.ID,
				invocation.StateExecuting, invocation.Update{}); err != nil {
				return nil, err
			}
		}
		out := l.cfg.Tools.Execute(ctx, sessionID, call)
		if err := l.journalOutcome(ctx, sessionID, call.ID, out); err != nil {
			return nil, err
		}
		st.used.ToolCalls++
		st.used.Tools[name]++
		res.ToolCalls = st.used.ToolCalls
		st.messages = append(st.messages, chat.Message{
			Role:       chat.RoleTool,
			Content:    out.Content,
			ToolCallID: pending,
		})
		st.done = append(st.done, pending)
		l.emit(ctx, st, EventToolCall, map[string]any{
			"call_id": pending, "tool": name, "outcome": string(out.Kind),
		})
		if out.Kind == pipeline.KindUncertain {
			res.Uncertain = append(res.Uncertain, pending)
		}
	} else {
		// 账目收口：rejected = **审批闸**判的「确定未施加」（终态）。
		if l.cfg.Journal != nil {
			if _, err := l.journalEntry(ctx, sessionID, chat.ToolCall{ID: pending}); err != nil {
				return nil, err
			}
			if err := l.journalAdvance(ctx, sessionID, pending,
				invocation.StateRejected,
				invocation.Update{Reason: denyObservation}); err != nil {
				return nil, err
			}
		}
		st.messages = append(st.messages, chat.Message{
			Role:       chat.RoleTool,
			Content:    denyObservation,
			ToolCallID: pending,
		})
		st.done = append(st.done, pending)
		l.emit(ctx, st, EventToolCall, map[string]any{
			"call_id": pending, "tool": toolNameOf(messages, pending), "outcome": "approval_denied",
		})
	}

	// 决策与回填同段提交（挂起恢复窗口的两端各自自洽），接主驱动循环续跑。
	if err := l.commit(ctx, st, contract.StatusRunning); err != nil {
		return res, err
	}
	// 结果已回填进历史 ⇒ 账目收口（applied → result_recorded，终态）。
	if err := l.journalFinish(ctx, sessionID, pending); err != nil {
		return res, err
	}
	return l.drive(ctx, st)
}
