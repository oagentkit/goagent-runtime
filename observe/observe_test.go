package observe_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/yi-nology/goagent-kernel/eventbus"

	"github.com/yi-nology/goagent-runtime/observe"
)

// TestAttachForwards 订阅转发：事件经 bus 到 sink。
func TestAttachForwards(t *testing.T) {
	bus := eventbus.New()
	var mu sync.Mutex
	var got []eventbus.Event
	detach := observe.Attach(context.Background(), bus, func(ev eventbus.Event) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, ev)
	})

	bus.Publish("sess-1", "", nil, "agent.round_start", "info", map[string]any{"round": 1})

	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(got) == 1
	})
	if got[0].Type != "agent.round_start" {
		t.Fatalf("事件类型错，got %s", got[0].Type)
	}

	detach()
	bus.Publish("sess-1", "", nil, "agent.round_end", "info", nil)
	time.Sleep(20 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("detach 后不得再收到事件，got %d", len(got))
	}
}

// TestNilSinkNoOp nil sink 不炸、detach 可用。
func TestNilSinkNoOp(t *testing.T) {
	bus := eventbus.New()
	detach := observe.Attach(context.Background(), bus, nil)
	detach()
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("等待事件超时")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
