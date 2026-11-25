// loop_delta_test.go —— 流式旁路（chat.OnDelta → content_delta 事件）：
// 模型逐片回调原样入事件（保序、kind/text 不变）、终答语义与非流式一致；
// 无观测面（Emitter nil）不挂回调（零开销旁路）。
package agentloop_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/oagentkit/goagent-kernel/contract"

	"github.com/oagentkit/goagent-runtime/agentloop"
	"github.com/oagentkit/goagent-runtime/chat"
	"github.com/oagentkit/goagent-runtime/pipeline"
	"github.com/oagentkit/goagent-runtime/storage"
)

// streamModel 支持流式的脚本模型：Chat 时若 OnDelta 非 nil 先逐片回调，
// 再返回固定终答；记录回调挂载状态供断言。
type streamModel struct {
	mu         sync.Mutex
	deltas     []chat.Delta
	response   chat.ChatResponse
	sawOnDelta bool
}

func (s *streamModel) Chat(_ context.Context, req chat.ChatRequest) (chat.ChatResponse, error) {
	s.mu.Lock()
	s.sawOnDelta = req.OnDelta != nil
	deltas := s.deltas
	s.mu.Unlock()
	if req.OnDelta != nil {
		for _, d := range deltas {
			req.OnDelta(d)
		}
	}
	return s.response, nil
}

func (s *streamModel) onDeltaSeen() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sawOnDelta
}

// captureEmitter 收集发布事件（typ+payload 逐条，保序）。
type captureEmitter struct {
	mu     sync.Mutex
	events []capturedEvent
}

type capturedEvent struct {
	typ     string
	payload map[string]any
}

func (c *captureEmitter) Publish(_ context.Context, _, _ string, _ *uint64, typ, _ string, payload map[string]any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, capturedEvent{typ: typ, payload: payload})
	return nil
}

// payloadsOf 取指定类型的全部事件载荷（保序）。
func (c *captureEmitter) payloadsOf(typ string) []map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []map[string]any
	for _, e := range c.events {
		if e.typ == typ {
			out = append(out, e.payload)
		}
	}
	return out
}

// TestContentDeltaEvents 流式旁路主用例：模型经 OnDelta 逐片回调 → loop
// 逐片发 content_delta（kind/text 原样、保序）；终答与非流式语义一致。
func TestContentDeltaEvents(t *testing.T) {
	m := &streamModel{
		deltas: []chat.Delta{
			{Kind: chat.DeltaKindText, Text: "你"},
			{Kind: chat.DeltaKindText, Text: "好"},
			{Kind: chat.DeltaKindReasoning, Text: "先想一下"},
		},
		response: chat.ChatResponse{Content: "你好"},
	}
	mem := storage.NewMem()
	pipe, err := pipeline.New(mem.Effects(), time.Minute, nil)
	if err != nil {
		t.Fatalf("建管线失败: %v", err)
	}
	emu := &captureEmitter{}
	l, err := agentloop.New(agentloop.Config{
		Model: m, Tools: pipe, Store: mem.Checkpoints(),
		Emitter: emu, System: "test",
	})
	if err != nil {
		t.Fatalf("建循环失败: %v", err)
	}
	res, err := l.Run(context.Background(), "sess-delta", "打个招呼")
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	if res.Status != contract.StatusSucceeded || res.Final != "你好" {
		t.Fatalf("终答语义应不变: status=%s final=%q", res.Status, res.Final)
	}
	if !m.onDeltaSeen() {
		t.Fatal("有观测面时 OnDelta 应挂载")
	}
	got := emu.payloadsOf(agentloop.EventContentDelta)
	want := []struct{ kind, text string }{
		{chat.DeltaKindText, "你"},
		{chat.DeltaKindText, "好"},
		{chat.DeltaKindReasoning, "先想一下"},
	}
	if len(got) != len(want) {
		t.Fatalf("content_delta 条数: got %d want %d (%+v)", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i]["kind"] != w.kind || got[i]["text"] != w.text {
			t.Fatalf("第 %d 片: got kind=%v text=%v want %s/%s",
				i, got[i]["kind"], got[i]["text"], w.kind, w.text)
		}
	}
}

// TestContentDeltaNoEmitter 无观测面：Emitter nil 不挂 OnDelta（零开销
// 旁路），模型整段返回照常。
func TestContentDeltaNoEmitter(t *testing.T) {
	m := &streamModel{
		deltas:   []chat.Delta{{Kind: chat.DeltaKindText, Text: "x"}},
		response: chat.ChatResponse{Content: "整段"},
	}
	mem := storage.NewMem()
	pipe, err := pipeline.New(mem.Effects(), time.Minute, nil)
	if err != nil {
		t.Fatalf("建管线失败: %v", err)
	}
	l, err := agentloop.New(agentloop.Config{
		Model: m, Tools: pipe, Store: mem.Checkpoints(),
	})
	if err != nil {
		t.Fatalf("建循环失败: %v", err)
	}
	res, err := l.Run(context.Background(), "sess-nodelta", "hi")
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	if res.Final != "整段" {
		t.Fatalf("终答: got %q", res.Final)
	}
	if m.onDeltaSeen() {
		t.Fatal("无观测面时 OnDelta 不应挂载")
	}
}

// TestToolCallPayloadCarriesBytes 执行轮 tool_call 事件带结果字节数（宿主
// 审计轨迹面：只记长度不记内容）；counter handler 返回 "ok" → bytes=2。
func TestToolCallPayloadCarriesBytes(t *testing.T) {
	m := &fakeModel{errAt: -1, script: []chat.ChatResponse{
		{ToolCalls: []chat.ToolCall{{ID: "c1", Name: "calc", Arguments: "{}"}}},
		{Content: "done"},
	}}
	c := &counter{}
	mem := storage.NewMem()
	pipe, err := pipeline.New(mem.Effects(), time.Minute, []pipeline.Tool{{
		Name: "calc", Safe: true, Handler: c.handler,
	}})
	if err != nil {
		t.Fatalf("建管线失败: %v", err)
	}
	emu := &captureEmitter{}
	l, err := agentloop.New(agentloop.Config{
		Model: m, Tools: pipe, Store: mem.Checkpoints(), Emitter: emu,
	})
	if err != nil {
		t.Fatalf("建循环失败: %v", err)
	}
	if _, err := l.Run(context.Background(), "sess-bytes", "算一下"); err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	// bytes 键在批669 事件面补全后落在收口事件（tool_result）上——执行轮
	// tool_call 只有请求参数（执行未始，无结果可计）。
	results := emu.payloadsOf(agentloop.EventToolResult)
	if len(results) != 1 {
		t.Fatalf("应恰一条收口 tool_result: %+v", results)
	}
	p := results[0]
	if p["tool"] != "calc" || p["bytes"] != 2 {
		t.Fatalf("payload 应含 tool=calc/bytes=2: %+v", p)
	}
}
