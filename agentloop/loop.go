// Package agentloop —— 可恢复的模型↔工具**回合循环**。
//
// # 它把哪些机制件串成一条链
//
//	chat.Model（供应商中立）⇄ 本循环 ⇄ pipeline（副作用施加）
//	每回合/每工具后 checkpoint 落快照（中断后从存储恢复，不重放副作用）
//	回合前 roundguard 判额度（预算用尽按策略强制收尾或停止）
//	全程经 Emitter 出事件（embedded 广播 / standalone 落库）
//
// # 恢复语义（本包的要点）
//
// 会话历史整体序列化进 checkpoint 快照（Fields["history"]）。进程死在
// 任意两个 commit 之间时，恢复点要么包含完整的「assistant 工具请求 +
// tool 结果回填」，要么两者都不包含——因为回填与提交在同一段顺序代码里。
// 恢复时若读到**不完整**的历史（assistant 请求了工具但结果缺失），视为
// 存储被外部篡改，fail-closed 报错，不猜。
//
// # 幂等键与 toolCallID
//
// 模型响应里的每个工具调用，ID 由编排侧（NewID）在入历史**之前**铸造：
// 它是 pipeline 副作用幂等键的原材料，恢复重放时同一调用必须同键——
// 否则闸把它当成另一个动作，重复施加的防护就漏了。
package agentloop

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"sync/atomic"
	"time"

	"github.com/oekit/pkg/errors"

	"github.com/yi-nology/goagent-kernel/checkpoint"
	"github.com/yi-nology/goagent-kernel/contract"
	"github.com/yi-nology/goagent-kernel/eventbus"
	"github.com/yi-nology/goagent-kernel/roundguard"

	"github.com/yi-nology/goagent-runtime/chat"
	"github.com/yi-nology/goagent-runtime/pipeline"
)

// fieldHistory 快照 Fields 里会话历史的键。
const fieldHistory = "history"

// defaultForceFinalPrompt 强制收尾提示词缺省文案（宿主未注入 ForceFinalPrompt
// 时回落到这里；文案逐字保留=兼容基线）。
const defaultForceFinalPrompt = "预算已用尽：请基于已有信息立即给出最终回答，不要再调用工具。"

// 事件类型词表（eventbus typ 字段取值）。
//
// 工具事件配对语义：EventToolCall 是**执行前**的请求事件（参数完整、副作用
// 未施加），EventToolResult 是执行后的收口事件（outcome + 结果内容）——两侧
// 用 call_id 配对。宿主（bianque 批669 面2）把这对事件映射到 agentrun 同名
// 事件词表，SSE 工具卡即获得与既有路径相同的「卡创建（含参数）→ 卡收口
// （含结果）」形态；执行前后的发射点在同一顺序代码段内，崩溃窗口的两端
// 各自自洽（与「回填与提交同段」同一纪律）。
const (
	EventRoundStart   = "agent.round_start"
	EventContentDelta = "agent.content_delta"
	EventToolCall     = "agent.tool_call"
	EventToolResult   = "agent.tool_result"
	EventRoundEnd     = "agent.round_end"
	EventSessionState = "agent.session_status"
)

// 工具结果 outcome 取值（EventToolResult payload 的 outcome 键）：执行面
// 复用 pipeline.Kind 词表（applied/replayed/uncertain/refused/failed），
// 审批闸拒绝为 "approval_denied"（副作用未施加的专用收口值）。

// Emitter 事件发射面。eventbus.DurablePublisher 与本包的 busEmitter
// 都满足它；nil = 不出事件。
type Emitter interface {
	Publish(ctx context.Context, sessionID, caseID string, stepID *uint64, typ, level string, payload map[string]any) error
}

// Config 装配一个循环。Model/Store/Tools 缺一即装配期编程错误。
type Config struct {
	// Model 供应商中立的模型调用面（必填）。
	Model chat.Model
	// Tools 工具施加管线（必填）。
	Tools *pipeline.Pipeline
	// Store 进度快照存储（必填；恢复点与历史走这里）。
	Store checkpoint.StoreCAS
	// Emitter 事件发射面；nil 时用 Bus（可再 nil = 完全不出事件）。
	Emitter Emitter
	// Bus embedded 形态的默认事件面（Emitter 为 nil 时生效）。
	Bus *eventbus.Bus
	// Budget 回合/工具额度。零值 = 全部不限。
	Budget roundguard.Budget
	// System 可选系统提示（新会话的第一条消息）。
	System string
	// NewID 工具调用 ID 铸造器；nil = 默认（crypto/rand + 进程内回退）。
	NewID func() string
	// ForceFinalPrompt 预算用尽时注入的强制收尾提示词，宿主注入（i18n/调口径
	// 的接缝）；空值 = 缺省文案（issue #3：提示词是领域概念，不硬编码在 loop 层）。
	ForceFinalPrompt string
	// OnEmitError 事件发射失败的宿主回调；nil = 只计数不回调。发射失败从此
	// 不再静默吞（issue #2 可观测面：孤儿事件/终态事件丢失从此可见）。
	OnEmitError func(err error)
	// ApprovalGate 工具执行前的人工审批闸（issue #4：仅对声明了
	// pipeline.Tool.NeedsApproval 的工具生效）。三态决策：
	// ApprovalApprove ⇒ 直通执行；ApprovalDeny ⇒ 副作用不施加、拒绝以工具
	// 观察回喂模型（可自行改道）；ApprovalSuspend ⇒ 提交 awaiting 快照后
	// Run 返回（PendingCallID 携带挂起调用），宿主离线收集决策后经
	// Approve(callID, approved) 落决策续跑。err ≠ nil ⇒ fail-closed 本轮
	// 失败上抛（闸失败=无法证明获准）。同步等待由宿主在闸内实现，ctx 超时
	// 仍归循环管；nil = 无审批面。
	ApprovalGate func(ctx context.Context, sessionID string, call chat.ToolCall) (Approval, error)
	// Joint 快照+事件单事务提交面（issue #2 主体：事件行与快照行同生共死
	// ——孤儿事件/终态事件丢失的崩溃窗口结构闭合）。非 nil 时 emit 改为
	// 累计、commit 改为联合提交（提交成功后统一广播，广播失败只计数）；
	// nil = 两步旧路径（现状语义，向后兼容）。
	Joint JointCommitter
}

// Result 一次 Run 的结果。
type Result struct {
	// Status contract 状态词表（succeeded / failed / timed_out）。
	Status string
	// Final 终答文本（Status=succeeded 时有值）。
	Final string
	// Rounds 实际回合数（模型调用次数）。
	Rounds int
	// ToolCalls 实际工具调用次数。
	ToolCalls int
	// Uncertain 结果未知的 toolCallID（须先对账，禁止盲目重试）。
	Uncertain []string
	// EmitFailed 事件发射失败次数（checkpoint 已提交但事件未落库 = 孤儿事件
	// / 终态事件丢失的可观测面；恢复重跑须自行对账）。
	EmitFailed int
	// PendingCallID 挂起等审批的调用 ID（Status=waiting_approval 时有值；
	// Approve 落决策的锚点）。
	PendingCallID string
}

// Loop 可恢复的回合循环。
//
// 同一 Loop 的 Run 不得并发调用：快照序号（seq）是进程内单调游标，
// 并发 Run 同一 sessionID 须由调用方串行化（或各用各的 Loop 共享存储）。
type Loop struct {
	cfg       Config
	gate      *checkpoint.SeqGate
	committer *checkpoint.Committer
	seq       uint64
	fallback  atomic.Uint64
	// pendings Joint 模式的事件累计面（emit 构造后不发送，随下一次快照
	// 提交原子落库）。同一 Loop 的 Run/Approve 不得并发（快照序号是进程内
	// 单调游标）⇒ 本字段顺序使用安全。
	pendings []eventbus.Event
}

// New 装配循环（装配期校验，fail fast）。
func New(cfg Config) (*Loop, error) {
	if cfg.Model == nil {
		return nil, errors.New("循环缺模型面（nil Model = 没有回合的另一端）")
	}
	if cfg.Tools == nil {
		return nil, errors.New("循环缺工具管线（nil Tools = 模型请求工具无人拦）")
	}
	if cfg.Store == nil {
		return nil, errors.New("循环缺进度快照存储（nil Store = 中断不可恢复）")
	}
	if err := cfg.Budget.Validate(); err != nil {
		return nil, errors.Wrap(err, "回合预算非法（负额度不许静默降级为不限）")
	}
	if cfg.Emitter == nil && cfg.Bus != nil {
		cfg.Emitter = busEmitter{bus: cfg.Bus}
	}
	l := &Loop{cfg: cfg, gate: checkpoint.NewSeqGate()}
	l.committer = checkpoint.NewCommitter(l.gate, cfg.Store)
	return l, nil
}

// Run 执行（或续跑）一个会话回合链。
//
// sessionID 是快照 RunID：同一 sessionID 的多次 Run 共享历史——进程重启后
// 用同一 ID 再 Run 即恢复。user 是本次的用户输入（追加到历史尾部）。
func (l *Loop) Run(ctx context.Context, sessionID, user string) (*Result, error) {
	// 回灌序号基线：进程内单调闸重启后是空的，不回灌则陈旧快照能盖掉新进度。
	if err := l.committer.SeedFromStore(ctx, sessionID); err != nil {
		return nil, errors.Wrap(err, "回灌快照序号基线失败", "session", sessionID)
	}
	snap, messages, done, err := l.loadHistory(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	// 挂起中的会话 Run 短路：awaiting 状态下模型协议上不接受「assistant
	// 工具请求悬而未决」的历史继续对话——先走 Approve 落决策再续跑。
	if snap != nil && snap.Status == StatusAwaitingApproval {
		return &Result{Status: StatusAwaitingApproval, PendingCallID: pendingCallID(messages)}, nil
	}
	messages = append(messages, chat.Message{Role: chat.RoleUser, Content: user})
	used := roundguard.Used{Tools: map[string]int{}}
	res := &Result{Status: contract.StatusRunning}
	return l.drive(ctx, sessionID, messages, done, used, res)
}

// drive 主回合驱动循环：预算闸 → 模型调用 → 审批闸 → 工具施加 → 快照提交。
// Run 与 Approve 共享这一条路径（单源，禁复制漂移）。
func (l *Loop) drive(ctx context.Context, sessionID string, messages []chat.Message, done []string, used roundguard.Used, res *Result) (*Result, error) {
	forced := false // 收尾轮标记：预算用尽后给模型的最后一次机会

	for {
		if !forced {
			v := roundguard.CheckRounds(l.cfg.Budget, used)
			if !v.Allowed {
				if l.cfg.Budget.EffectivePolicy() == roundguard.PolicyForceFinal {
					forced = true
					messages = append(messages, chat.Message{
						Role:    chat.RoleUser,
						Content: l.forceFinalPrompt(),
					})
				} else {
					res.Status = contract.StatusTimedOut
					break
				}
			}
		}

		req := chat.ChatRequest{
			Messages: append([]chat.Message(nil), messages...),
			Tools:    l.cfg.Tools.Specs(),
		}
		// 流式旁路：模型支持时逐片回调 → content_delta 事件。
		//
		// 挂载条件：**有观测面**（Emitter 非 nil）**或**有联合提交面（Joint
		// 非 nil）——后者此前不挂 ⇒ 事件随快照落库的形态（standalone / cluster）
		// 里流式增量静默消失（issue #11 的遗留缺口）。
		//
		// 载荷：kind/text 原样透传（保序）；带工具调用片段或计量片段时一并
		// 带上结构化字段——观测面要**原样**（不归并：归并是响应面的事，
		// 规则见 chat.DeltaAggregator）。
		if l.cfg.Emitter != nil || l.cfg.Joint != nil {
			req.OnDelta = func(d chat.Delta) {
				l.emitDelta(ctx, res, sessionID, deltaPayload(d))
			}
		}
		resp, err := l.cfg.Model.Chat(ctx, req)
		if err != nil {
			// ctx 取消/超时上抛：快照已持久化，下次同 sessionID 再 Run 即恢复。
			return res, errors.Wrap(err, "模型调用失败", "session", sessionID)
		}
		used.Rounds++
		res.Rounds = used.Rounds

		// ID 铸造先于入历史：assistant 消息里的 ID 与 pipeline 幂等键同源。
		calls := make([]chat.ToolCall, len(resp.ToolCalls))
		copy(calls, resp.ToolCalls)
		for i := range calls {
			if calls[i].ID == "" {
				calls[i].ID = l.newID()
			}
		}
		messages = append(messages, chat.Message{
			Role:      chat.RoleAssistant,
			Content:   resp.Content,
			ToolCalls: calls,
		})
		l.emit(ctx, res, sessionID, EventRoundStart, map[string]any{
			"round": used.Rounds, "tool_calls": len(calls),
		})

		if len(calls) == 0 {
			res.Status = contract.StatusSucceeded
			res.Final = resp.Content
			break
		}
		if forced {
			// 收尾轮仍请求工具：额度口径到此为止，不再放行。
			res.Status = contract.StatusTimedOut
			break
		}

		stopped := false
		for _, call := range calls {
			if v := roundguard.CheckToolCalls(l.cfg.Budget, used); !v.Allowed {
				res.Status = contract.StatusTimedOut
				stopped = true
				break
			}
			if v := roundguard.CheckTool(l.cfg.Budget, used, call.Name); !v.Allowed {
				res.Status = contract.StatusTimedOut
				stopped = true
				break
			}
			// 执行前人工审批闸（issue #4）：只闸声明了 NeedsApproval 的工具。
			if l.cfg.ApprovalGate != nil && l.cfg.Tools.NeedsApproval(call.Name) {
				decision, err := l.cfg.ApprovalGate(ctx, sessionID, call)
				if err != nil {
					// fail-closed：闸失败=无法证明获准，副作用不施加。
					return res, errors.Wrap(err, "审批闸失败", "session", sessionID, "tool", call.Name)
				}
				switch decision {
				case ApprovalApprove:
					// 批准：直通执行。
				case ApprovalDeny:
					// 拒绝以工具观察回喂：模型可自行改道；副作用未施加。
					messages = append(messages, chat.Message{
						Role:       chat.RoleTool,
						Content:    denyObservation,
						ToolCallID: call.ID,
					})
					done = append(done, call.ID)
					l.emit(ctx, res, sessionID, EventToolResult, map[string]any{
						"call_id": call.ID, "tool": call.Name,
						"outcome": "approval_denied", "content": denyObservation,
					})
					continue
				default:
					// 挂起（ApprovalSuspend；未知决策值同义——保守挂起）：
					// 提交 awaiting 快照后返回，宿主稍后经 Approve 落决策。
					res.Status = StatusAwaitingApproval
					res.PendingCallID = call.ID
					if err := l.commit(ctx, res, sessionID, messages, done, res.Status); err != nil {
						return res, err
					}
					l.emit(ctx, res, sessionID, EventSessionState, map[string]any{"status": res.Status})
					return res, nil
				}
			}
			// 执行前请求事件：参数完整、副作用未施加（观测面在卡创建时即拿到
			// 参数；与收口事件以 call_id 配对）。
			l.emit(ctx, res, sessionID, EventToolCall, map[string]any{
				"call_id": call.ID, "tool": call.Name, "args": call.Arguments,
			})
			// 执行。
			out := l.cfg.Tools.Execute(ctx, sessionID, call)
			used.ToolCalls++
			used.Tools[call.Name]++
			res.ToolCalls = used.ToolCalls
			messages = append(messages, chat.Message{
				Role:       chat.RoleTool,
				Content:    out.Content,
				ToolCallID: call.ID,
			})
			done = append(done, call.ID)
			l.emit(ctx, res, sessionID, EventToolResult, map[string]any{
				"call_id": call.ID, "tool": call.Name,
				"outcome": string(out.Kind), "content": out.Content,
				// 结果字节数（审计轨迹面：宿主落 tool 轨迹日志只记长度不记内容；
				// #16 语义收编进批669 的收口事件形态）——审批拒绝等无结果的
				// 调用轮不带本键。
				"bytes": len(out.Content),
			})
			if out.Kind == pipeline.KindUncertain {
				res.Uncertain = append(res.Uncertain, call.ID)
			}
			// 回填与提交在同一段顺序代码里：崩溃窗口的两端各自自洽。
			if err := l.commit(ctx, res, sessionID, messages, done, contract.StatusRunning); err != nil {
				return res, err
			}
		}
		if stopped {
			break
		}
	}

	// 终态事件先于终态提交构造（Joint 模式下随快照原子落库；两步模式下
	// 崩溃窗口从「终态事件丢失」换为「可对账的重复」——重复好于丢）。
	l.emit(ctx, res, sessionID, EventSessionState, map[string]any{"status": res.Status})
	if err := l.commit(ctx, res, sessionID, messages, done, res.Status); err != nil {
		return res, err
	}
	return res, nil
}

// loadHistory 读恢复点：历史 + 已完成工具调用。首次运行（ErrNotFound）
// 返回初始历史（仅系统提示）。awaiting 状态的快照放行「最后一条 assistant
// 消息存在未答复调用」（挂起形态），更早消息的未答复仍是篡改。
func (l *Loop) loadHistory(ctx context.Context, sessionID string) (*checkpoint.Snapshot, []chat.Message, []string, error) {
	snap, err := l.cfg.Store.Load(ctx, sessionID)
	if err != nil {
		if errors.Is(err, checkpoint.ErrNotFound) {
			msgs := []chat.Message{}
			if l.cfg.System != "" {
				msgs = append(msgs, chat.Message{Role: chat.RoleSystem, Content: l.cfg.System})
			}
			return nil, msgs, nil, nil
		}
		return nil, nil, nil, errors.Wrap(err, "读取会话快照失败", "session", sessionID)
	}
	allowPending := snap.Status == StatusAwaitingApproval
	if err := verifyHistory(snap.Fields[fieldHistory], allowPending); err != nil {
		return nil, nil, nil, errors.Wrap(err, "会话快照历史不完整", "session", sessionID)
	}
	var messages []chat.Message
	if raw := snap.Fields[fieldHistory]; raw != "" {
		if err := json.Unmarshal([]byte(raw), &messages); err != nil {
			return nil, nil, nil, errors.Wrap(err, "会话快照历史反序列化失败", "session", sessionID)
		}
	}
	// 序号游标回灌：本次提交的序号必须从已持久化的最大值续起。
	l.seq = snap.Seq
	return &snap, messages, append([]string(nil), snap.Done...), nil
}

// commit 落一次进度快照（序号严格递增），历史整体序列化进 Fields。
// 序号由调用侧单调游标管理（SeqGate 只判不分配），恢复时经 loadHistory
// 回灌——不回灌则本次 Seq=1 会被「已持久化序号 >= 本次」拒收。
//
// Joint 模式分派（issue #2 主体）：事件行随快照单事务落库（同生共死——
// 联合提交失败整批回滚）；提交成功后统一广播（进程内广播 fire-and-forget
// 无失败语义，数据已原子）。旧路径=先落快照，事件由 emit 即时发（向后兼容）。
//
// ★ **联合提交失败时本批事件作废，不保留待下轮重试**：重试的形态是
// 「重跑该回合」，新一轮会重新产生事件；把上一批挂在 pendings 里等下一轮
// 提交，等于把两轮的事件混进同一笔快照。失败即整批作废（数据面已回滚，
// 事件面随之为空——同生共死）。
func (l *Loop) commit(ctx context.Context, res *Result, sessionID string, messages []chat.Message, done []string, status string) error {
	raw, err := json.Marshal(messages)
	if err != nil {
		return errors.Wrap(err, "会话历史序列化失败", "session", sessionID)
	}
	l.seq++
	snap := checkpoint.Snapshot{
		RunID:  sessionID,
		Seq:    l.seq,
		Status: status,
		Done:   append([]string(nil), done...),
		Fields: map[string]string{fieldHistory: string(raw)},
	}
	if l.cfg.Joint != nil {
		events := l.pendings
		l.pendings = nil
		if err := l.cfg.Joint.CommitWithEvents(ctx, snap, events); err != nil {
			return errors.Wrap(err, "联合提交失败", "session", sessionID)
		}
		if l.cfg.Bus != nil {
			for _, ev := range events {
				l.cfg.Bus.Broadcast(ev)
			}
		}
		return nil
	}
	if err := l.committer.Commit(ctx, snap); err != nil {
		return errors.Wrap(err, "进度快照落库失败", "session", sessionID)
	}
	return nil
}

// verifyHistory 历史完整性校验：assistant 的每个工具调用必须都有对应的
// tool 结果回填。不满足 = 存储里的历史不完整（崩溃窗口不会产出这种形态，
// 出现即外部篡改/实现漂移），fail-closed。
//
// allowPending（挂起形态放行面）：仅允许**最后一条** assistant 消息存在
// 未答复调用——这正是挂起提交的形态；更早消息的未答复调用仍是篡改。
func verifyHistory(raw string, allowPending bool) error {
	if raw == "" {
		return nil
	}
	var messages []chat.Message
	if err := json.Unmarshal([]byte(raw), &messages); err != nil {
		return err
	}
	answered := map[string]bool{}
	for _, m := range messages {
		if m.Role == chat.RoleTool {
			answered[m.ToolCallID] = true
		}
	}
	lastAssistant := -1
	for i, m := range messages {
		if m.Role == chat.RoleAssistant {
			lastAssistant = i
		}
	}
	for i, m := range messages {
		if m.Role != chat.RoleAssistant {
			continue
		}
		pendingOK := allowPending && i == lastAssistant
		for _, c := range m.ToolCalls {
			if !answered[c.ID] && !pendingOK {
				return errors.Newf("assistant 的工具调用缺少结果回填: call_id=%s tool=%s", c.ID, c.Name)
			}
		}
	}
	return nil
}

// newID 铸造工具调用 ID。默认 crypto/rand；极端失败回退计数器+纳秒时间。
func (l *Loop) newID() string {
	if l.cfg.NewID != nil {
		return l.cfg.NewID()
	}
	var b [12]byte
	if _, err := rand.Read(b[:]); err == nil {
		return "call-" + hex.EncodeToString(b[:])
	}
	return "call-" + time.Now().Format("20060102150405.000000000") + "-" +
		hex.EncodeToString([]byte{byte(l.fallback.Add(1))})
}

// forceFinalPrompt 强制收尾提示词：宿主注入优先，空值回落缺省文案。
func (l *Loop) forceFinalPrompt() string {
	if l.cfg.ForceFinalPrompt != "" {
		return l.cfg.ForceFinalPrompt
	}
	return defaultForceFinalPrompt
}

// emit 出事件。Joint 模式（Config.Joint 非 nil）= 只累计不发送——事件行
// 与快照行同生共死（issue #2 主体），随下一次 commit 单事务落库后统一广播。
// 非 Joint：即时发（Emitter nil+Bus=内置纯广播；Emitter=持久化+广播）。
// 发射失败不再静默吞（issue #2 可观测面）：计数 Result.EmitFailed +
// OnEmitError 回调。
//
// ★ **流式增量不走这条路径**（见 emitDelta）：增量是观测面不是状态面。
func (l *Loop) emit(ctx context.Context, res *Result, sessionID, typ string, payload map[string]any) {
	if l.cfg.Emitter == nil && l.cfg.Joint == nil {
		return
	}
	if l.cfg.Joint != nil {
		l.pendings = append(l.pendings, eventbus.Event{
			SessionID: sessionID, Type: typ, Level: "info",
			Payload: payload, OccurredAt: time.Now(),
		})
		return
	}
	if err := l.cfg.Emitter.Publish(ctx, sessionID, "", nil, typ, "info", payload); err != nil {
		res.EmitFailed++
		if l.cfg.OnEmitError != nil {
			l.cfg.OnEmitError(err)
		}
	}
}

// deltaPayload 把一片流式增量摊成事件载荷：kind/text 原样，带工具调用
// 片段或计量片段时一并带上结构化字段。
func deltaPayload(d chat.Delta) map[string]any {
	payload := map[string]any{"kind": d.Kind, "text": d.Text}
	if d.ToolCall != nil {
		payload["tool_call"] = map[string]any{
			"index":     d.ToolCall.Index,
			"id":        d.ToolCall.ID,
			"name":      d.ToolCall.Name,
			"arguments": d.ToolCall.Arguments,
		}
	}
	if d.Usage != nil {
		payload["usage"] = map[string]any{
			"prompt_tokens":     d.Usage.PromptTokens,
			"completion_tokens": d.Usage.CompletionTokens,
			"total_tokens":      d.Usage.TotalTokens,
		}
	}
	return payload
}

// emitDelta 流式增量：**只广播，不落事件行**。
//
// 为什么与其它事件分路：流式增量是**观测面**（瞬时碎片），不是状态面。
// Joint 形态下事件面就是落库面（事件行 = 状态面的一行）——把碎片塞进去
// 会污染状态面，也会打破「一轮四事件」那条判据（issue #9 的口径）。
// 故 Joint 形态走总线**即时广播**（逐片体验），不进 pendings。
//
// 非 Joint 形态：与其它事件同路径（Emitter），宿主既有采集方式不变。
//
// 背压：总线广播是**非阻塞**的（订阅方缓冲满则丢弃并计数）⇒ 宿主 SSE 写慢
// 不会反压模型流；片段因此可能丢——观测面允许丢，状态面不允许。
func (l *Loop) emitDelta(ctx context.Context, res *Result, sessionID string, payload map[string]any) {
	if l.cfg.Joint != nil {
		if l.cfg.Bus != nil {
			l.cfg.Bus.Broadcast(eventbus.Event{
				SessionID: sessionID, Type: EventContentDelta,
				Level: "info", Payload: payload, OccurredAt: time.Now(),
			})
		}
		return
	}
	if l.cfg.Emitter == nil {
		return
	}
	if err := l.cfg.Emitter.Publish(ctx, sessionID, "", nil, EventContentDelta, "info", payload); err != nil {
		res.EmitFailed++
		if l.cfg.OnEmitError != nil {
			l.cfg.OnEmitError(err)
		}
	}
}

// busEmitter 把 eventbus.Broadcast 包成 Emitter（embedded 形态默认）。
type busEmitter struct{ bus *eventbus.Bus }

func (e busEmitter) Publish(_ context.Context, sessionID, caseID string, stepID *uint64, typ, level string, payload map[string]any) error {
	e.bus.Broadcast(eventbus.Event{
		SessionID: sessionID, CaseID: caseID, StepID: stepID,
		Type: typ, Level: level, Payload: payload, OccurredAt: time.Now(),
	})
	return nil
}
