package pipeline_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/yi-nology/goagent-runtime/chat"
	"github.com/yi-nology/goagent-runtime/pipeline"
	"github.com/yi-nology/goagent-runtime/storage"
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

var errBoom = errorString("boom")

type errorString string

func (e errorString) Error() string { return string(e) }
