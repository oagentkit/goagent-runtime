package trace_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/yi-nology/goagent-kernel/eventbus"

	"github.com/yi-nology/goagent-runtime/agentloop"
	"github.com/yi-nology/goagent-runtime/chat"
	"github.com/yi-nology/goagent-runtime/pipeline"
	"github.com/yi-nology/goagent-runtime/runtime"
	"github.com/yi-nology/goagent-runtime/trace"
)

// scriptModel 脚本化模型：按调用序出剧本；errAt >= 0 时该次注入错误
// （-1 = 不注入——本仓约定，零值 0 会在首次调用就报错）。
type scriptModel struct {
	mu     sync.Mutex
	calls  int
	errAt  int
	script []chat.ChatResponse
}

func (m *scriptModel) Chat(_ context.Context, _ chat.ChatRequest) (chat.ChatResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	i := m.calls
	m.calls++
	if m.errAt >= 0 && i == m.errAt {
		return chat.ChatResponse{}, errInjected
	}
	if i < len(m.script) {
		return m.script[i], nil
	}
	return chat.ChatResponse{Content: "（剧本耗尽）"}, nil
}

var errInjected = errString("注入的模型错误")

type errString string

func (e errString) Error() string { return string(e) }

// waitCond 轮询等条件成立（事件转发是异步的，不能断言即时到达）。
func waitCond(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("等%s超时", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestTraceAnswersOperationalQuestions 端到端：真 runtime 挂 Collector，
// 逐条回答 issue #42 的运营八问（问 5 走任务板，不在本包——见 cluster 用例）。
func TestTraceAnswersOperationalQuestions(t *testing.T) {
	ctx := context.Background()
	col := trace.NewCollector()

	// 双工具异速：c1 快、c2 慢 ⇒ 最慢工具调用应是 c2。
	slow := make(chan struct{})
	model := &scriptModel{errAt: -1, script: []chat.ChatResponse{
		// Run 1：一轮双工具 + 终答轮（两轮都带计量）。
		{ToolCalls: []chat.ToolCall{
			{ID: "call-fast", Name: "fast", Arguments: "{}"},
			{ID: "call-slow", Name: "slow", Arguments: "{}"},
		}, Usage: &chat.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}},
		{Content: "完成", Usage: &chat.Usage{PromptTokens: 7, CompletionTokens: 3, TotalTokens: 10}},
	}}
	tools := []pipeline.Tool{
		{Name: "fast", Handler: func(context.Context, []byte) (string, error) { return "ok", nil }},
		{Name: "slow", Handler: func(context.Context, []byte) (string, error) {
			select {
			case <-slow:
			case <-time.After(150 * time.Millisecond):
			}
			return "ok", nil
		}},
	}
	rt, err := runtime.NewEmbedded(runtime.Config{
		Model:    model,
		Tools:    tools,
		Observer: col.Sink(),
	})
	if err != nil {
		t.Fatalf("装配失败: %v", err)
	}
	defer func() { _ = rt.Close() }()

	s1, err := rt.NewSession(ctx, runtime.SessionOptions{ID: "sess-q"})
	if err != nil {
		t.Fatalf("NewSession 失败: %v", err)
	}
	res1, err := s1.Run(ctx, "跑")
	if err != nil {
		t.Fatalf("Run1 失败: %v", err)
	}
	close(slow) // 放行（其实150ms 自然放行，这里兜底不悬挂）

	// 第二次执行：同会话第二个 Run。
	res2, err := s1.Run(ctx, "再来")
	if err != nil {
		t.Fatalf("Run2 失败: %v", err)
	}

	// 问 1：会话有多少次 Run = 2；run_id 进了事件面（否则切不出两个 Run）。
	waitCond(t, "RunCount=2", func() bool { return col.RunCount("sess-q") == 2 })
	if res1.RunID == "" || res2.RunID == "" || res1.RunID == res2.RunID {
		t.Fatalf("每次执行应有独立 RunID: %q / %q", res1.RunID, res2.RunID)
	}

	// 问 2：Run1 经历 2 个 Turn（两轮模型调用）；Run2 只有终答轮 = 1。
	if got := col.TurnCount("sess-q", res1.RunID); got != 2 {
		t.Fatalf("Run1 Turn 数应 2，got %d", got)
	}
	if got := col.TurnCount("sess-q", res2.RunID); got != 1 {
		t.Fatalf("Run2 Turn 数应 1，got %d", got)
	}

	// 问 3：最慢工具调用 = call-slow（150ms 档）。
	slowest, ok := col.SlowestToolCall("sess-q")
	if !ok {
		t.Fatal("应观测到已收口的工具调用")
	}
	if slowest.CallID != "call-slow" || slowest.Outcome == "" || slowest.EndedAt.IsZero() {
		t.Fatalf("最慢应是 call-slow 且已收口，got %+v", slowest)
	}

	// 问 8：Token 聚合 = 两轮上报之和（Run1）+ Run2（剧本耗尽未上报 = 0）。
	waitCond(t, "usage 聚合", func() bool { return col.Usage("sess-q").TotalTokens == 25 })
	u := col.Usage("sess-q")
	if u.PromptTokens != 17 || u.CompletionTokens != 8 || u.TotalTokens != 25 {
		t.Fatalf("Usage 聚合错: %+v", u)
	}
}

// TestTraceFailureAndPause 问 4/6/7：模型失败的轮次与原因、挂起的暂停原因。
func TestTraceFailureAndPause(t *testing.T) {
	ctx := context.Background()
	col := trace.NewCollector()

	// 会话 A：第 2 次模型调用失败（第 1 轮正常跑完工具 ⇒ 失败在第 2 轮）。
	mFail := &scriptModel{errAt: 1, script: []chat.ChatResponse{
		{ToolCalls: []chat.ToolCall{{ID: "c1", Name: "noop", Arguments: "{}"}}},
	}}
	rt, err := runtime.NewEmbedded(runtime.Config{
		Model:    mFail,
		Tools:    []pipeline.Tool{{Name: "noop", Handler: func(context.Context, []byte) (string, error) { return "ok", nil }}},
		Observer: col.Sink(),
	})
	if err != nil {
		t.Fatalf("装配失败: %v", err)
	}
	defer func() { _ = rt.Close() }()

	sA, _ := rt.NewSession(ctx, runtime.SessionOptions{ID: "sess-fail"})
	if _, err := sA.Run(ctx, "跑"); err == nil {
		t.Fatal("注入的模型错误应上抛")
	}
	waitCond(t, "失败投影", func() bool { return len(col.Failures("sess-fail")) == 1 })
	fails := col.Failures("sess-fail")
	if fails[0].FailedRound != 2 {
		t.Fatalf("失败应发生在第 2 轮模型调用，got %d", fails[0].FailedRound)
	}
	if reason, ok := col.TerminalFailure("sess-fail"); !ok || reason == "" {
		t.Fatalf("应答得出最终 failed 原因，got ok=%v reason=%q", ok, reason)
	}

	// 会话 B：审批挂起 ⇒ 暂停原因 = 挂起调用 ID。
	mSusp := &scriptModel{errAt: -1, script: []chat.ChatResponse{
		{ToolCalls: []chat.ToolCall{{ID: "call-need-appr", Name: "danger", Arguments: "{}"}}},
	}}
	rt2, err := runtime.NewEmbedded(runtime.Config{
		Model: mSusp,
		Tools: []pipeline.Tool{{
			Name: "danger", Safe: true, NeedsApproval: true,
			Handler: func(context.Context, []byte) (string, error) { return "ok", nil },
		}},
		ApprovalGate: func(context.Context, string, chat.ToolCall) (agentloop.Approval, error) {
			return agentloop.ApprovalSuspend, nil
		},
		Observer: col.Sink(),
	})
	if err != nil {
		t.Fatalf("装配失败: %v", err)
	}
	defer func() { _ = rt2.Close() }()

	sB, _ := rt2.NewSession(ctx, runtime.SessionOptions{ID: "sess-pause"})
	res, err := sB.Run(ctx, "跑")
	if err != nil {
		t.Fatalf("挂起 Run 失败: %v", err)
	}
	if res.Status != agentloop.StatusAwaitingApproval {
		t.Fatalf("应挂起，got %s", res.Status)
	}
	waitCond(t, "挂起投影", func() bool { _, ok := col.PauseReason("sess-pause"); return ok })
	pending, ok := col.PauseReason("sess-pause")
	if !ok || pending != "call-need-appr" {
		t.Fatalf("暂停原因应是挂起调用 ID，got ok=%v pending=%q", ok, pending)
	}
}

// TestTraceJSONPayloadShape 事件经 PG/SQLite 事件面往返后 payload 退化成
// JSON 形态（map/float64）——投影器两种形态都要判得到（本用例直接喂
// JSON 往返后的载荷钉住读侧）。
func TestTraceJSONPayloadShape(t *testing.T) {
	col := trace.NewCollector()
	jsonize := func(p map[string]any) map[string]any {
		raw, err := json.Marshal(p)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var out map[string]any
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		return out
	}
	at := time.Now()
	feed := func(typ string, p map[string]any) {
		col.Handle(eventbus.Event{SessionID: "sess-json", Type: typ, Payload: jsonize(p), OccurredAt: at})
	}
	feed(agentloop.EventRoundStart, map[string]any{
		"run_id": "run-1", "round": 1, "tool_calls": 1,
		"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
	})
	feed(agentloop.EventToolCall, map[string]any{
		"run_id": "run-1", "call_id": "c1", "tool": "t",
	})
	feed(agentloop.EventToolResult, map[string]any{
		"run_id": "run-1", "call_id": "c1", "tool": "t",
		"outcome": "applied", "bytes": 42,
	})
	feed(agentloop.EventSessionState, map[string]any{
		"run_id": "run-1", "status": "succeeded",
	})

	if got := col.TurnCount("sess-json", "run-1"); got != 1 {
		t.Fatalf("JSON 形态下 Turn 计数应 1，got %d", got)
	}
	slowest, ok := col.SlowestToolCall("sess-json")
	if !ok || slowest.CallID != "c1" || slowest.Outcome != "applied" || slowest.Bytes != 42 {
		t.Fatalf("JSON 形态工具配对错: %+v ok=%v", slowest, ok)
	}
	if u := col.Usage("sess-json"); u.TotalTokens != 15 {
		t.Fatalf("JSON 形态 usage 聚合应 15，got %d", u.TotalTokens)
	}
	snap, ok := col.Session("sess-json")
	if !ok || len(snap.Runs) != 1 || snap.Runs[0].EndedAt.IsZero() {
		t.Fatalf("终态事件应推进 EndedAt，got %+v ok=%v", snap, ok)
	}
}
