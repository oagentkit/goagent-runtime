// loop_delta_joint_test.go —— 流式旁路在**联合提交形态**下的语义
// （issue #11 的遗留缺口 + issue #9 的「一轮四事件」口径）。
//
// 判据三条：
//  1. Joint 形态下 OnDelta **要挂载**（此前只在 Emitter 非 nil 时挂 ⇒
//     standalone/cluster 里流式增量静默消失）；
//  2. 流式增量**只广播不落事件行**（观测面 ≠ 状态面：事件行 = 状态面
//     的一行，把瞬时碎片塞进去会污染状态面，也会打破「一轮四事件」）；
//  3. 状态面事件照旧随快照单事务落库（联合提交本体不被破坏）。
package agentloop_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/yi-nology/goagent-kernel/checkpoint"
	"github.com/yi-nology/goagent-kernel/eventbus"

	"github.com/yi-nology/goagent-runtime/agentloop"
	"github.com/yi-nology/goagent-runtime/chat"
	"github.com/yi-nology/goagent-runtime/pipeline"
	"github.com/yi-nology/goagent-runtime/storage"
)

// recordingJoint 记录联合提交收到的快照与事件（判据②的取证面）。
type recordingJoint struct {
	mu     sync.Mutex
	events []eventbus.Event
}

func (r *recordingJoint) CommitWithEvents(_ context.Context, _ checkpoint.Snapshot, events []eventbus.Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, events...)
	return nil
}

func (r *recordingJoint) types() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.events))
	for _, e := range r.events {
		out = append(out, e.Type)
	}
	return out
}

// drainBus 取走总线里已投递的事件（非阻塞：广播是同步投缓冲的）。
func drainBus(ch <-chan eventbus.Event) []eventbus.Event {
	var out []eventbus.Event
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, ev)
		default:
			return out
		}
	}
}

// scriptedModel 脚本化模型：第一轮请求工具、第二轮给终答（⇒ 两轮 ⇒
// 状态面「一轮四事件」：2×round_start + tool_call + session_state）。
type scriptedModel struct {
	mu     sync.Mutex
	calls  int
	deltas []chat.Delta
}

func (s *scriptedModel) Chat(_ context.Context, req chat.ChatRequest) (chat.ChatResponse, error) {
	s.mu.Lock()
	i := s.calls
	s.calls++
	ds := s.deltas
	s.mu.Unlock()
	if req.OnDelta != nil {
		for _, d := range ds {
			req.OnDelta(d)
		}
	}
	if i == 0 {
		return chat.ChatResponse{ToolCalls: []chat.ToolCall{{ID: "call-1", Name: "calc", Arguments: "{}"}}}, nil
	}
	return chat.ChatResponse{Content: "逐片"}, nil
}

// TestContentDeltaJointBroadcastsNotPersisted Joint 形态：流式增量即时
// 广播（总线上可见）且**不进事件行**（联合提交收到的事件里没有它）；
// 状态面事件照旧随快照落库。
func TestContentDeltaJointBroadcastsNotPersisted(t *testing.T) {
	ctx := context.Background()
	m := &scriptedModel{
		deltas: []chat.Delta{
			{Kind: chat.DeltaKindText, Text: "逐"},
			{Kind: chat.DeltaKindText, Text: "片"},
		},
	}
	mem := storage.NewMem()
	pipe, err := pipeline.New(mem.Effects(), time.Minute, []pipeline.Tool{
		{Name: "calc", Safe: true, Handler: func(_ context.Context, _ []byte) (string, error) {
			return "ok", nil
		}},
	})
	if err != nil {
		t.Fatalf("建管线失败: %v", err)
	}
	bus := eventbus.New()
	ch, unsub := bus.Subscribe(ctx)
	defer unsub()
	joint := &recordingJoint{}
	l, err := agentloop.New(agentloop.Config{
		Model: m, Tools: pipe, Store: mem.Checkpoints(),
		Joint: joint, Bus: bus, System: "test",
	})
	if err != nil {
		t.Fatalf("建循环失败: %v", err)
	}
	if _, err := l.Run(ctx, "sess-joint-delta", "说点什么"); err != nil {
		t.Fatalf("Run 失败: %v", err)
	}

	// ① 总线上能看到逐片增量（两轮各 2 片 ⇒ 4 条；即时广播，
	//    不等回合结束才成批）。
	deltas := 0
	for _, ev := range drainBus(ch) {
		if ev.Type == agentloop.EventContentDelta {
			deltas++
		}
	}
	if deltas != 4 {
		t.Fatalf("总线上应逐片广播 4 条 content_delta（两轮×2 片），got %d", deltas)
	}

	// ② 联合提交收到的事件里**不含** content_delta（观测面不进状态面）。
	for _, typ := range joint.types() {
		if typ == agentloop.EventContentDelta {
			t.Fatal("流式增量不得进事件行（会污染状态面并打破「一轮四事件」判据）")
		}
	}

	// ③ 状态面事件照旧落库，且**条数不受增量影响**（两轮 ⇒ 一轮四事件：
	//    2×round_start + tool_call + session_state）——这是 issue #9 的口径。
	types := joint.types()
	if len(types) != 4 {
		t.Fatalf("一轮应落 4 条状态面事件，got %d (%v)", len(types), types)
	}
	if types[3] != agentloop.EventSessionState {
		t.Fatalf("末条应为终态事件，got %s (%v)", types[3], types)
	}
}

// TestContentDeltaPayloadCarriesFragments 载荷带结构化片段：工具调用片段
// 与计量原样透传（观测面要原样，不做归并）。
func TestContentDeltaPayloadCarriesFragments(t *testing.T) {
	ctx := context.Background()
	m := &streamModel{
		deltas: []chat.Delta{
			{Kind: chat.DeltaKindToolCall, ToolCall: &chat.ToolCallFragment{
				Index: 0, ID: "call-1", Name: "calc", Arguments: `{"a":1}`}},
			{Kind: chat.DeltaKindUsage, Usage: &chat.Usage{PromptTokens: 3, CompletionTokens: 4, TotalTokens: 7}},
		},
		response: chat.ChatResponse{Content: "好"},
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
	if _, err := l.Run(ctx, "sess-frag", "走"); err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	got := emu.payloadsOf(agentloop.EventContentDelta)
	if len(got) != 2 {
		t.Fatalf("应发 2 条 content_delta，got %d", len(got))
	}
	tc, ok := got[0]["tool_call"].(map[string]any)
	if !ok {
		t.Fatalf("载荷应带 tool_call 结构：got %+v", got[0])
	}
	if tc["index"] != 0 || tc["id"] != "call-1" || tc["name"] != "calc" {
		t.Fatalf("tool_call 载荷原样透传：got %+v", tc)
	}
	us, ok := got[1]["usage"].(map[string]any)
	if !ok {
		t.Fatalf("载荷应带 usage 结构：got %+v", got[1])
	}
	if us["total_tokens"] != 7 {
		t.Fatalf("usage 载荷原样透传：got %+v", us)
	}
}
