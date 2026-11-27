// Package trace —— 运行时可观测的 **Trace 读模型**（issue #42，W5）。
//
// # 定位：事件是真相，这里只投影
//
// kernel eventbus 给事件面判据，本包把事件流投影成
// Session → Run → Turn → LLM Call / Tool Call 的层级视图，并回答运营侧
// 的八问。**不落库、不解释判据、不改事件面**——层级是读模型，删掉它
// 不损失任何状态。
//
// # 挂载
//
// Collector 实现事件消费形态（Sink() 出 observe.Sink），挂到任意
// Observer 注入面即可：
//
//	c := trace.NewCollector()
//	rt, _ := runtime.NewEmbedded(runtime.Config{Observer: c.Sink(), ...})
//	// cluster：Config.Observer 同理（事件经 PG 面转发到本地总线）
//
// # 八问 → 查询（issue #42）
//
//  1. 一个 Session 有多少次 Run？            → RunCount
//  2. 一次 Run 经历多少个 Turn？             → TurnCount（Turn ≈ 回合 =
//     一次模型调用驱动的轮次，事件面 round_start 计数）
//  3. 哪个 ToolCall 最慢？                   → SlowestToolCall
//     （tool_call → tool_result 按 call_id 配对计时）
//  4. 哪次模型调用失败？                     → Failures（RunID + FailedRound
//     = 失败发生在第几轮模型调用 + 错误文本）
//  5. 哪个 Worker 接管了任务？               → 不在本包：worker 身份在
//     taskboard 任务板上（Owner / 接管代次 Fence），走 cluster 的
//     Board().List 查询（判据面，不进事件投影）
//  6. 任务为什么被暂停？                     → PauseReason（awaiting_approval
//     + 挂起调用 ID）
//  7. 为什么最终进入 failed？                → TerminalFailure（终态 failed
//     的错误文本）
//  8. 消耗了多少 Token？                     → Usage（chat.ChatResponse.Usage
//     随 round 事件出面后聚合；模型未上报 = 零值，不猜）
//
// # 层级与身份
//
// run_id 由 agentloop 每次 Run/Approve 铸造、注进每个事件 payload——
// Trace 按它切分 Run。挂起（awaiting）与续跑（Approve）是两次执行 = 两个
// Run 记录（同一会话下相邻，宿主可按 PendingCallID 关联）。Handoff 节
// 预留在层级里（多 agent 移交事件尚无生产方，出现后按 run_id 同法归位）。
//
// 事件命名法与词表单源的裁决（snake_case 为法 / `agent.*` 冻结不扩张 /
// run 身份走载荷键不新造事件族 / worker 身份留任务板）见 ADR-0011。
package trace

import (
	"sync"
	"time"

	"github.com/yi-nology/goagent-kernel/eventbus"

	"github.com/yi-nology/goagent-runtime/agentloop"
	"github.com/yi-nology/goagent-runtime/chat"
)

// Collector 事件流 → 层级视图的内存投影。
//
// 并发安全（事件回调与查询可能来自不同 goroutine）；查询返回**副本**——
// 拿到之后 Collector 继续演进不影响已取快照。内存驻留型：进程退出即失
// （要长期留存请宿主自行落库，本包不带存储面）。
type Collector struct {
	mu       sync.Mutex
	sessions map[string]*SessionTrace
}

// NewCollector 建投影器。
func NewCollector() *Collector {
	return &Collector{sessions: map[string]*SessionTrace{}}
}

// Sink 出 observe.Sink 形态（挂 Runtime/cluster 的 Observer 注入面）。
func (c *Collector) Sink() func(eventbus.Event) {
	return c.Handle
}

// SessionTrace 一个会话的 Trace（副本形态）。
type SessionTrace struct {
	ID   string
	Runs []RunTrace
}

// RunTrace 一次执行（副本形态）。
type RunTrace struct {
	RunID     string
	StartedAt time.Time
	// EndedAt 终态/挂起事件到达时刻；零值 = 仍开启（进程死在执行中等）。
	EndedAt time.Time
	// Status 终态词表（succeeded / failed / timed_out / waiting_approval）；
	// 空 = 未观测到状态事件。
	Status string
	// Error 失败原因（session_status 的 error 字段；仅 failed 时有值）。
	Error string
	// FailedRound 失败发生在第几轮模型调用（= 已观测轮数 + 1；非失败为 0）
	//——「哪次模型调用失败」的轮次答案。
	FailedRound int
	// PendingCallID 挂起等审批的调用 ID（Status=waiting_approval 时有值）。
	PendingCallID string
	// Turns 回合（按 round_start 到达序）。
	Turns []TurnTrace
	// Usage 本 Run 的 Token 聚合（模型上报了才有；未上报为零值）。
	Usage chat.Usage
}

// TurnTrace 一个回合（副本形态）。
type TurnTrace struct {
	Round     int
	StartedAt time.Time
	// ToolCalls 本回合的工具调用（按 call_id 配对计时）。
	ToolCalls []ToolCallTrace
}

// ToolCallTrace 一次工具调用（副本形态）。
type ToolCallTrace struct {
	CallID string
	Tool   string
	// StartedAt / EndedAt tool_call → tool_result 的到达时刻；EndedAt
	// 零值 = 未收口（执行中进程死掉等）。
	StartedAt time.Time
	EndedAt   time.Time
	// Outcome tool_result 的 outcome（applied / replayed / uncertain /
	// contended / failed / approval_denied …）。
	Outcome string
	// Bytes 结果字节数（审计轨迹面；tool_result 未带时为 0）。
	Bytes int
}

// Handle 消费一条事件（投影规则见包文档）。未知类型忽略——事件面是开放
// 词表，读模型只认自己会投影的那几种。
func (c *Collector) Handle(ev eventbus.Event) {
	if ev.SessionID == "" {
		return
	}
	at := ev.OccurredAt
	if at.IsZero() {
		at = time.Now()
	}
	runID := payloadString(ev.Payload, "run_id")

	c.mu.Lock()
	defer c.mu.Unlock()
	sess := c.sessions[ev.SessionID]
	if sess == nil {
		sess = &SessionTrace{ID: ev.SessionID}
		c.sessions[ev.SessionID] = sess
	}
	run := sess.run(runID, at)

	switch ev.Type {
	case agentloop.EventRoundStart:
		turn := TurnTrace{Round: payloadInt(ev.Payload, "round"), StartedAt: at}
		run.Turns = append(run.Turns, turn)
		if u, ok := payloadUsage(ev.Payload["usage"]); ok {
			run.Usage.PromptTokens += u.PromptTokens
			run.Usage.CompletionTokens += u.CompletionTokens
			run.Usage.TotalTokens += u.TotalTokens
		}
	case agentloop.EventToolCall:
		call := ToolCallTrace{
			CallID:    payloadString(ev.Payload, "call_id"),
			Tool:      payloadString(ev.Payload, "tool"),
			StartedAt: at,
		}
		run.currentTurn().ToolCalls = append(run.currentTurn().ToolCalls, call)
	case agentloop.EventToolResult:
		callID := payloadString(ev.Payload, "call_id")
		if t := run.findCall(callID); t != nil {
			t.EndedAt = at
			t.Outcome = payloadString(ev.Payload, "outcome")
			t.Bytes = payloadInt(ev.Payload, "bytes")
		}
	case agentloop.EventSessionState:
		run.Status = payloadString(ev.Payload, "status")
		run.Error = payloadString(ev.Payload, "error")
		run.PendingCallID = payloadString(ev.Payload, "pending_call_id")
		run.EndedAt = at
		if run.Status == "failed" && run.FailedRound == 0 {
			run.FailedRound = len(run.Turns) + 1
		}
	}
}

// runID 索引下取（或建）Run 记录。到达序即执行序（同会话执行不并发——
// 快照序号闸保证；跨副本同会话由 taskboard 单点执行保证）。
func (s *SessionTrace) run(runID string, at time.Time) *RunTrace {
	for i := range s.Runs {
		if s.Runs[i].RunID == runID {
			return &s.Runs[i]
		}
	}
	s.Runs = append(s.Runs, RunTrace{RunID: runID, StartedAt: at})
	return &s.Runs[len(s.Runs)-1]
}

// currentTurn 最近回合（round_start 必先于 tool_call 到达；防御性兜底：
// 没有回合时补一个匿名回合，不让调用丢出层级）。
func (r *RunTrace) currentTurn() *TurnTrace {
	if len(r.Turns) == 0 {
		r.Turns = append(r.Turns, TurnTrace{})
	}
	return &r.Turns[len(r.Turns)-1]
}

// findCall 在本 Run 全部回合里按 call_id 找未收口的调用（tool_result 追
// tool_call；找不到 = 事件乱序/外来事件，忽略）。
func (r *RunTrace) findCall(callID string) *ToolCallTrace {
	for ti := range r.Turns {
		for ci := range r.Turns[ti].ToolCalls {
			if r.Turns[ti].ToolCalls[ci].CallID == callID {
				return &r.Turns[ti].ToolCalls[ci]
			}
		}
	}
	return nil
}

// ---- 八问查询（返回副本；见包文档的问答映射）----

// RunCount 问 1：会话有多少次 Run（含未观测到终态的开启中执行）。
func (c *Collector) RunCount(sessionID string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if s := c.sessions[sessionID]; s != nil {
		return len(s.Runs)
	}
	return 0
}

// TurnCount 问 2：某次 Run 经历多少个 Turn（round_start 计数）。
func (c *Collector) TurnCount(sessionID, runID string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if s := c.sessions[sessionID]; s != nil {
		for i := range s.Runs {
			if s.Runs[i].RunID == runID {
				return len(s.Runs[i].Turns)
			}
		}
	}
	return 0
}

// SlowestToolCall 问 3：会话内耗时最长且已收口的工具调用。
// 未收口的调用（执行中进程死掉）没有耗时，不参与排名。
func (c *Collector) SlowestToolCall(sessionID string) (ToolCallTrace, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.sessions[sessionID]
	if s == nil {
		return ToolCallTrace{}, false
	}
	var (
		slowest ToolCallTrace
		found   bool
	)
	for _, run := range s.Runs {
		for _, turn := range run.Turns {
			for _, call := range turn.ToolCalls {
				if call.EndedAt.IsZero() || call.StartedAt.IsZero() {
					continue
				}
				if !found || call.EndedAt.Sub(call.StartedAt) > slowest.EndedAt.Sub(slowest.StartedAt) {
					slowest = call
					found = true
				}
			}
		}
	}
	return slowest, found
}

// Failures 问 4：失败的执行清单（RunID + 失败轮次 + 错误文本）。
// 只含状态事件标 failed 的执行；错误文本缺失（外来事件）也列出。
func (c *Collector) Failures(sessionID string) []RunTrace {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []RunTrace
	if s := c.sessions[sessionID]; s != nil {
		for _, run := range s.Runs {
			if run.Status == "failed" {
				out = append(out, run.clone())
			}
		}
	}
	return out
}

// PauseReason 问 6：会话为什么被暂停——返回最近一次挂起的调用 ID
// （Status=waiting_approval 的执行）；ok=false = 没有挂起记录。
func (c *Collector) PauseReason(sessionID string) (pendingCallID string, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if s := c.sessions[sessionID]; s != nil {
		for i := len(s.Runs) - 1; i >= 0; i-- {
			if s.Runs[i].Status == "waiting_approval" {
				return s.Runs[i].PendingCallID, true
			}
		}
	}
	return "", false
}

// TerminalFailure 问 7：会话最终 failed 的原因（最近一次 failed 执行的
// 错误文本）；ok=false = 没有 failed 记录。
func (c *Collector) TerminalFailure(sessionID string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if s := c.sessions[sessionID]; s != nil {
		for i := len(s.Runs) - 1; i >= 0; i-- {
			if s.Runs[i].Status == "failed" {
				return s.Runs[i].Error, true
			}
		}
	}
	return "", false
}

// Usage 问 8：会话 Token 聚合（全部 Run 求和；模型未上报的轮次不计——
// 零值是「没数据」不是「没消耗」，不猜）。
func (c *Collector) Usage(sessionID string) chat.Usage {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out chat.Usage
	if s := c.sessions[sessionID]; s != nil {
		for _, run := range s.Runs {
			out.PromptTokens += run.Usage.PromptTokens
			out.CompletionTokens += run.Usage.CompletionTokens
			out.TotalTokens += run.Usage.TotalTokens
		}
	}
	return out
}

// Session 取整个会话的层级快照（深副本）。
func (c *Collector) Session(sessionID string) (SessionTrace, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.sessions[sessionID]
	if s == nil {
		return SessionTrace{}, false
	}
	out := SessionTrace{ID: s.ID, Runs: make([]RunTrace, len(s.Runs))}
	for i := range s.Runs {
		out.Runs[i] = s.Runs[i].clone()
	}
	return out, true
}

// clone 深副本（查询返回值语义：拿到之后与 Collector 的演进脱钩）。
func (r RunTrace) clone() RunTrace {
	out := r
	if r.Turns != nil {
		out.Turns = make([]TurnTrace, len(r.Turns))
		for i := range r.Turns {
			out.Turns[i] = r.Turns[i]
			if r.Turns[i].ToolCalls != nil {
				out.Turns[i].ToolCalls = append([]ToolCallTrace(nil), r.Turns[i].ToolCalls...)
			}
		}
	}
	return out
}

// ---- payload 取值（事件载荷两种形态：进程内直发是原生类型，经 PG/SQLite
// 事件面往返后 JSON 解码成 map/float64——两种都要判得到）----

func payloadString(p map[string]any, key string) string {
	if p == nil {
		return ""
	}
	switch v := p[key].(type) {
	case string:
		return v
	}
	return ""
}

func payloadInt(p map[string]any, key string) int {
	if p == nil {
		return 0
	}
	switch v := p[key].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64: // JSON 解码形态
		return int(v)
	}
	return 0
}

// payloadUsage 计量取值：原生 *chat.Usage / chat.Usage（进程内直发）与
// map[string]any（JSON 往返后）双形态。
func payloadUsage(v any) (chat.Usage, bool) {
	switch u := v.(type) {
	case *chat.Usage:
		if u != nil {
			return *u, true
		}
	case chat.Usage:
		return u, true
	case map[string]any:
		return chat.Usage{
			PromptTokens:     intFrom(u["prompt_tokens"]),
			CompletionTokens: intFrom(u["completion_tokens"]),
			TotalTokens:      intFrom(u["total_tokens"]),
		}, true
	}
	return chat.Usage{}, false
}

func intFrom(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	}
	return 0
}
