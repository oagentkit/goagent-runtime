package agentloop_test

import (
	"context"
	"testing"
	"time"

	"github.com/yi-nology/goagent-kernel/contract"

	"github.com/yi-nology/goagent-runtime/agentloop"
	"github.com/yi-nology/goagent-runtime/chat"
	"github.com/yi-nology/goagent-runtime/pipeline"
	"github.com/yi-nology/goagent-runtime/storage"
)

// TestToolEventPairing 工具事件配对语义回归钉（批669 事件面补全）：
// EventToolCall = 执行**前**的请求事件（参数完整、副作用未施加），
// EventToolResult = 执行后的收口事件（outcome+content）——两侧 call_id 严格
// 配对、发射序=请求→收口（卡创建面先于收口面）。
func TestToolEventPairing(t *testing.T) {
	mem := storage.NewMem()
	var calls int
	pipe, err := pipeline.New(mem.Effects(), time.Minute, []pipeline.Tool{{
		Name: "calc",
		Safe: true,
		Handler: func(_ context.Context, _ []byte) (string, error) {
			calls++
			return "42", nil
		},
	}})
	if err != nil {
		t.Fatalf("建管线失败: %v", err)
	}
	m := &fakeModel{errAt: -1, script: []chat.ChatResponse{
		{ToolCalls: []chat.ToolCall{{ID: "call-1", Name: "calc", Arguments: `{"x":1}`}}},
		{Content: "终答"},
	}}
	em := &captureEmitter{}
	l, err := agentloop.New(agentloop.Config{
		Model: m, Tools: pipe, Store: mem.Checkpoints(),
		Emitter: em,
	})
	if err != nil {
		t.Fatalf("建循环失败: %v", err)
	}
	res, err := l.Run(context.Background(), "sess-pair", "开始")
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	if res.Status != contract.StatusSucceeded {
		t.Fatalf("应 succeeded，got %s", res.Status)
	}
	if calls != 1 {
		t.Fatalf("工具应执行 1 次，got %d", calls)
	}

	calls1 := em.payloadsOf(agentloop.EventToolCall)
	if len(calls1) != 1 {
		t.Fatalf("应恰好 1 条 tool_call，got %d", len(calls1))
	}
	p := calls1[0]
	if p["call_id"] != "call-1" || p["tool"] != "calc" || p["args"] != `{"x":1}` {
		t.Fatalf("tool_call 载荷应为 call_id+tool+args，got %v", p)
	}
	results := em.payloadsOf(agentloop.EventToolResult)
	if len(results) != 1 {
		t.Fatalf("应恰好 1 条 tool_result，got %d", len(results))
	}
	rp := results[0]
	if rp["call_id"] != "call-1" || rp["tool"] != "calc" ||
		rp["outcome"] != "applied" || rp["content"] != "42" {
		t.Fatalf("tool_result 载荷应为 call_id+tool+outcome+content，got %v", rp)
	}

	// 发射序：tool_call 必须先于同 call_id 的 tool_result。
	em.mu.Lock()
	var seq []string
	for _, e := range em.events {
		if e.typ == agentloop.EventToolCall || e.typ == agentloop.EventToolResult {
			seq = append(seq, e.typ)
		}
	}
	em.mu.Unlock()
	if len(seq) != 2 || seq[0] != agentloop.EventToolCall || seq[1] != agentloop.EventToolResult {
		t.Fatalf("工具事件序应为 请求→收口，got %v", seq)
	}
}
