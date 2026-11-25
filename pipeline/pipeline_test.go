package pipeline_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/oagentkit/goagent-runtime/chat"
	"github.com/oagentkit/goagent-runtime/pipeline"
	"github.com/oagentkit/goagent-runtime/storage"
)

// countingHandler 计数的工具执行体（重放不得重复施加的证据）。
type countingHandler struct {
	mu    sync.Mutex
	calls int
}

func (h *countingHandler) run(_ context.Context, _ []byte) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls++
	return "ok", nil
}

func (h *countingHandler) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.calls
}

func newPipe(t *testing.T, h *countingHandler, safe bool) *pipeline.Pipeline {
	t.Helper()
	p, err := pipeline.New(storage.NewMem().Effects(), time.Minute, []pipeline.Tool{{
		Name:    "calc",
		Safe:    safe,
		Handler: h.run,
	}})
	if err != nil {
		t.Fatalf("建管线失败: %v", err)
	}
	return p
}

func TestAppliedExecutesOnce(t *testing.T) {
	h := &countingHandler{}
	p := newPipe(t, h, true)

	out := p.Execute(context.Background(), "s1", chat.ToolCall{ID: "call-1", Name: "calc"})
	if out.Kind != pipeline.KindApplied {
		t.Fatalf("首调应为 applied，got %s（%s）", out.Kind, out.Content)
	}
	if out.Content != "ok" {
		t.Fatalf("结果内容应为 ok，got %q", out.Content)
	}
	if h.count() != 1 {
		t.Fatalf("执行体应被调 1 次，got %d", h.count())
	}
}

// TestReplayedSameIDDoesNotReapply 本包的核心判据：同一 toolCallID 重放
// （恢复场景）直接取回既有结果，**执行体不得再次调用**。
func TestReplayedSameIDDoesNotReapply(t *testing.T) {
	h := &countingHandler{}
	p := newPipe(t, h, true)

	p.Execute(context.Background(), "s1", chat.ToolCall{ID: "call-1", Name: "calc"})
	out := p.Execute(context.Background(), "s1", chat.ToolCall{ID: "call-1", Name: "calc"})

	if out.Kind != pipeline.KindReplayed {
		t.Fatalf("重放应为 replayed，got %s（%s）", out.Kind, out.Content)
	}
	if out.Content != "ok" {
		t.Fatalf("重放应取回既有结果 ok，got %q", out.Content)
	}
	if h.count() != 1 {
		t.Fatalf("重放不得重复施加：执行体应仍只被调 1 次，got %d", h.count())
	}
}

// TestDifferentIDIsAnotherAction 不同 toolCallID = 另一个逻辑动作：
// 闸不拦（同一 resource 串行下先后施加），但计数必须涨——幂等键的
// 边界就是 toolCallID 本身。
func TestDifferentIDIsAnotherAction(t *testing.T) {
	h := &countingHandler{}
	p := newPipe(t, h, true)

	p.Execute(context.Background(), "s1", chat.ToolCall{ID: "call-1", Name: "calc"})
	out := p.Execute(context.Background(), "s1", chat.ToolCall{ID: "call-2", Name: "calc"})
	if out.Kind != pipeline.KindApplied {
		t.Fatalf("新 ID 应为 applied，got %s", out.Kind)
	}
	if h.count() != 2 {
		t.Fatalf("新动作应再施加一次，got %d", h.count())
	}
}

func TestUnknownToolRefused(t *testing.T) {
	h := &countingHandler{}
	p := newPipe(t, h, true)

	out := p.Execute(context.Background(), "s1", chat.ToolCall{ID: "call-1", Name: "nope"})
	if out.Kind != pipeline.KindRefused {
		t.Fatalf("未注册工具应 refused，got %s", out.Kind)
	}
	if h.count() != 0 {
		t.Fatalf("未注册工具不得触发执行体，got %d", h.count())
	}
}

func TestHandlerErrorIsFailedNotUncertain(t *testing.T) {
	p, err := pipeline.New(storage.NewMem().Effects(), time.Minute, []pipeline.Tool{{
		Name: "flaky",
		Safe: true,
		Handler: func(_ context.Context, _ []byte) (string, error) {
			return "", errBoom
		},
	}})
	if err != nil {
		t.Fatalf("建管线失败: %v", err)
	}

	out := p.Execute(context.Background(), "s1", chat.ToolCall{ID: "call-1", Name: "flaky"})
	if out.Kind != pipeline.KindFailed {
		t.Fatalf("执行体明确报错应为 failed（未施加、结果可弃），got %s", out.Kind)
	}
}

func TestNilStoreRejected(t *testing.T) {
	if _, err := pipeline.New(nil, 0, nil); err == nil {
		t.Fatal("nil 副作用存储必须装配期报错（闸不存在 = 重复施加无人拦）")
	}
}

func TestDuplicateNameRejected(t *testing.T) {
	h := &countingHandler{}
	_, err := pipeline.New(storage.NewMem().Effects(), time.Minute, []pipeline.Tool{
		{Name: "calc", Handler: h.run},
		{Name: "calc", Handler: h.run},
	})
	if err == nil {
		t.Fatal("重名工具必须装配期报错")
	}
}

// TestContendedIsExplicitOutcomeNotPanic 资源被他人**活持有**时，闸判的是
// 「可重试的拒绝」（kernel sideeffect：OutcomeContended + **nil error**）——
// 它是正常判据之一，不是异常。
//
// 装配层必须把它落成明确结论（可重试的暂不可执行），而不是对 nil error
// 调 Error()（空指针 Panic）。多副本下资源竞争是常态，不是罕见路径。
func TestContendedIsExplicitOutcomeNotPanic(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	p, err := pipeline.New(storage.NewMem().Effects(), time.Minute, []pipeline.Tool{{
		Name: "calc",
		Handler: func(_ context.Context, _ []byte) (string, error) {
			close(started)
			<-release
			return "ok", nil
		},
	}})
	if err != nil {
		t.Fatalf("建管线失败: %v", err)
	}

	firstDone := make(chan pipeline.Outcome, 1)
	go func() {
		firstDone <- p.Execute(context.Background(), "s1", chat.ToolCall{ID: "call-1", Name: "calc"})
	}()
	<-started // 第一个执行体已持住资源闸（租约未过期）

	// 第二个执行撞上活持有 ⇒ 闸判 Contended（无 error）。
	var out pipeline.Outcome
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("资源竞争不得 Panic（Contended 是正常判据，不是异常）: %v", r)
			}
		}()
		out = p.Execute(context.Background(), "s1", chat.ToolCall{ID: "call-2", Name: "calc"})
	}()

	if out.Kind != pipeline.KindContended {
		t.Fatalf("资源被活持有应为 contended（本次未施加），got %s（%s）", out.Kind, out.Content)
	}
	if out.Content == "" {
		t.Fatal("contended 必须带明确结论文本（回喂模型时要说明未施加）")
	}

	close(release)
	<-firstDone
}

var errBoom = errorString("boom")

type errorString string

func (e errorString) Error() string { return string(e) }

// TestBusinessIdempotencyKeyAcrossSessionsIsReplayed 业务幂等键的跨会话语义：
// 同一业务动作换个会话重试 ⇒ 闸判 **replayed**（取回既有结果），而不是
// 「同 Key 异 Resource」的动作身份冲突（ErrActionConflict ⇒ 判 failed）。
//
// 危害形态：宿主声明了业务幂等键（跨副本接管重跑靠它不重复施加），跨会话
// 重试却被拒 ⇒ 重跑失败 ⇒ 业务动作要么没做成，要么被重做一遍。
func TestBusinessIdempotencyKeyAcrossSessionsIsReplayed(t *testing.T) {
	h := &countingHandler{}
	p, err := pipeline.New(storage.NewMem().Effects(), time.Minute, []pipeline.Tool{{
		Name: "deploy", Safe: true, Handler: h.run,
		IdempotencyKey: func(context.Context, []byte) (string, error) {
			return "order-42", nil
		},
	}})
	if err != nil {
		t.Fatalf("建管线失败: %v", err)
	}

	first := p.Execute(context.Background(), "sess-A", chat.ToolCall{ID: "call-1", Name: "deploy", Arguments: `{"target":"prod"}`})
	if first.Kind != pipeline.KindApplied {
		t.Fatalf("首调应为 applied，got %s（%s）", first.Kind, first.Content)
	}

	second := p.Execute(context.Background(), "sess-B", chat.ToolCall{ID: "call-2", Name: "deploy", Arguments: `{"target":"prod"}`})
	if second.Kind != pipeline.KindReplayed {
		t.Fatalf("同一次业务动作跨会话重试应为 replayed，got %s（%s）", second.Kind, second.Content)
	}
	if h.count() != 1 {
		t.Fatalf("重放不得重复施加：执行体应仍只被调 1 次，got %d", h.count())
	}
}
