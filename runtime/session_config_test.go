package runtime_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oagentkit/agent/roundguard"
	"github.com/yi-nology/goagent-kernel/eventbus"

	"github.com/yi-nology/goagent-runtime/agentloop"
	"github.com/yi-nology/goagent-runtime/chat"
	"github.com/yi-nology/goagent-runtime/pipeline"
	"github.com/yi-nology/goagent-runtime/runtime"
)

// 配置分层（issue #41 切片 2）：Runtime 级 Config 是默认，SessionOptions
// 覆盖只作用于本会话。判据核心 = **不泄漏**：覆盖不改 Runtime 默认、
// 不串到其他会话。

// countingToolNamed 指名计数工具。
func countingToolNamed(name string, n *int64, mu *sync.Mutex) pipeline.Tool {
	return pipeline.Tool{
		Name: name,
		Handler: func(context.Context, []byte) (string, error) {
			mu.Lock()
			defer mu.Unlock()
			*n++
			return name + "-ok", nil
		},
	}
}

func loadInt64(n *int64, mu *sync.Mutex) int64 {
	mu.Lock()
	defer mu.Unlock()
	return *n
}

// TestSessionSystemOverrideNoLeak System 覆盖只影响本会话首跑：
// 覆盖会话的模型请求带自己的系统提示，未覆盖会话带 Runtime 级的。
func TestSessionSystemOverrideNoLeak(t *testing.T) {
	m := &recordingModel{script: []chat.ChatResponse{
		{Content: "A 答"},
		{Content: "B 答"},
	}}
	rt, err := runtime.NewEmbedded(runtime.Config{
		Model:  m,
		System: "BASE-PROMPT",
	})
	if err != nil {
		t.Fatalf("装配失败: %v", err)
	}
	defer func() { _ = rt.Close() }()

	a, err := rt.NewSession(context.Background(), runtime.SessionOptions{
		ID:     "sess-a",
		System: "CUSTOM-A",
	})
	if err != nil {
		t.Fatalf("NewSession A 失败: %v", err)
	}
	if _, err := a.Run(context.Background(), "问 A"); err != nil {
		t.Fatalf("A.Run 失败: %v", err)
	}
	b, err := rt.NewSession(context.Background(), runtime.SessionOptions{ID: "sess-b"})
	if err != nil {
		t.Fatalf("NewSession B 失败: %v", err)
	}
	if _, err := b.Run(context.Background(), "问 B"); err != nil {
		t.Fatalf("B.Run 失败: %v", err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.reqs) != 2 {
		t.Fatalf("模型应调用 2 次，got %d", len(m.reqs))
	}
	if got := firstSystem(m.reqs[0].Messages); got != "CUSTOM-A" {
		t.Fatalf("覆盖会话的系统提示应是 CUSTOM-A，got %q", got)
	}
	if got := firstSystem(m.reqs[1].Messages); got != "BASE-PROMPT" {
		t.Fatalf("未覆盖会话不得被泄漏，应保持 BASE-PROMPT，got %q", got)
	}
}

// firstSystem 取首条系统提示（无系统消息返回空串）。
func firstSystem(msgs []chat.Message) string {
	if len(msgs) > 0 && msgs[0].Role == chat.RoleSystem {
		return msgs[0].Content
	}
	return ""
}

// TestSessionBudgetOverrideNoLeak Budget 覆盖只掐本会话：覆盖会话
// MaxRounds=1 ⇒ 强制收尾轮仍要工具即 timed_out（Rounds=2/ToolCalls=1）；
// 未覆盖会话按 Runtime 级（不限）跑到自然终态（Rounds=3/ToolCalls=2）。
func TestSessionBudgetOverrideNoLeak(t *testing.T) {
	// 剧本按调用序：A 两轮（第 2 轮被预算掐住）+ B 三轮（自然走完）。
	m := &scriptedModel{script: []chat.ChatResponse{
		{ToolCalls: []chat.ToolCall{{ID: "a1", Name: "calc", Arguments: "{}"}}}, // A 轮1
		{ToolCalls: []chat.ToolCall{{ID: "a2", Name: "calc", Arguments: "{}"}}}, // A 强制收尾轮仍要工具 → timed_out
		{ToolCalls: []chat.ToolCall{{ID: "b1", Name: "calc", Arguments: "{}"}}}, // B 轮1
		{ToolCalls: []chat.ToolCall{{ID: "b2", Name: "calc", Arguments: "{}"}}}, // B 轮2
		{Content: "自然终答"}, // B 轮3
	}}
	var applied int64
	var mu sync.Mutex
	rt, err := runtime.NewEmbedded(runtime.Config{
		Model:  m,
		Tools:  []pipeline.Tool{countingToolNamed("calc", &applied, &mu)},
		System: "",
	})
	if err != nil {
		t.Fatalf("装配失败: %v", err)
	}
	defer func() { _ = rt.Close() }()

	capped := roundguard.Budget{MaxRounds: 1}
	a, err := rt.NewSession(context.Background(), runtime.SessionOptions{
		ID:     "sess-cap",
		Budget: &capped,
	})
	if err != nil {
		t.Fatalf("NewSession A 失败: %v", err)
	}
	resA, err := a.Run(context.Background(), "跑")
	if err != nil {
		t.Fatalf("A.Run 失败: %v", err)
	}
	if resA.Status != "timed_out" || resA.Rounds != 2 || resA.ToolCalls != 1 {
		t.Fatalf("覆盖会话应被 MaxRounds=1 掐住（timed_out/Rounds=2/ToolCalls=1），got %+v", resA)
	}

	b, err := rt.NewSession(context.Background(), runtime.SessionOptions{ID: "sess-free"})
	if err != nil {
		t.Fatalf("NewSession B 失败: %v", err)
	}
	resB, err := b.Run(context.Background(), "跑")
	if err != nil {
		t.Fatalf("B.Run 失败: %v", err)
	}
	if resB.Status != "succeeded" || resB.Rounds != 3 || resB.ToolCalls != 2 {
		t.Fatalf("未覆盖会话不得被泄漏，应自然终态（succeeded/Rounds=3/ToolCalls=2），got %+v", resB)
	}
	if got := loadInt64(&applied, &mu); got != 3 {
		t.Fatalf("工具共施加 3 次（A 一次 + B 两次），got %d", got)
	}
}

// TestSessionToolsOverrideNoLeak Tools 覆盖只换本会话的工具面：覆盖会话
// 请求未注册工具 ⇒ refused 观察（不施加）；未覆盖会话同请求正常施加。
func TestSessionToolsOverrideNoLeak(t *testing.T) {
	// 剧本：A 要 beta → A 收口；B 要 beta → B 收口。
	m := &scriptedModel{script: []chat.ChatResponse{
		{ToolCalls: []chat.ToolCall{{ID: "a1", Name: "beta", Arguments: "{}"}}},
		{Content: "A 收口"},
		{ToolCalls: []chat.ToolCall{{ID: "b1", Name: "beta", Arguments: "{}"}}},
		{Content: "B 收口"},
	}}
	var alphaN, betaN int64
	var mu sync.Mutex
	rt, err := runtime.NewEmbedded(runtime.Config{
		Model: m,
		Tools: []pipeline.Tool{
			countingToolNamed("alpha", &alphaN, &mu),
			countingToolNamed("beta", &betaN, &mu),
		},
	})
	if err != nil {
		t.Fatalf("装配失败: %v", err)
	}
	defer func() { _ = rt.Close() }()

	// 覆盖会话只带 alpha：请求 beta 必须 refused（工具面换了）。
	a, err := rt.NewSession(context.Background(), runtime.SessionOptions{
		ID:    "sess-tools-a",
		Tools: []pipeline.Tool{countingToolNamed("alpha", &alphaN, &mu)},
	})
	if err != nil {
		t.Fatalf("NewSession A 失败: %v", err)
	}
	if _, err := a.Run(context.Background(), "跑"); err != nil {
		t.Fatalf("A.Run 失败: %v", err)
	}
	// 未覆盖会话：全量工具面，beta 正常施加。
	b, err := rt.NewSession(context.Background(), runtime.SessionOptions{ID: "sess-tools-b"})
	if err != nil {
		t.Fatalf("NewSession B 失败: %v", err)
	}
	if _, err := b.Run(context.Background(), "跑"); err != nil {
		t.Fatalf("B.Run 失败: %v", err)
	}

	if got := loadInt64(&betaN, &mu); got != 1 {
		t.Fatalf("覆盖会话 beta 应 refused 不施加，未覆盖会话施加一次 ⇒ beta 共 1，got %d", got)
	}
	if got := loadInt64(&alphaN, &mu); got != 0 {
		t.Fatalf("alpha 两次都没被请求，应 0，got %d", got)
	}
}

// TestSessionObserverFilteredAndDetached 会话级 Observer 按 sessionID 过滤
// （只收本会话），Runtime 级照旧全收；Close 统一 detach（不泄漏、不 panic）。
func TestSessionObserverFilteredAndDetached(t *testing.T) {
	var baseMu, aMu sync.Mutex
	var baseEvents, aEvents []eventbus.Event
	rt, err := runtime.NewEmbedded(runtime.Config{
		Model: &scriptedModel{script: []chat.ChatResponse{
			{Content: "A 答"},
			{Content: "B 答"},
		}},
		Observer: func(ev eventbus.Event) {
			baseMu.Lock()
			defer baseMu.Unlock()
			baseEvents = append(baseEvents, ev)
		},
	})
	if err != nil {
		t.Fatalf("装配失败: %v", err)
	}

	a, err := rt.NewSession(context.Background(), runtime.SessionOptions{
		ID: "sess-obs-a",
		Observer: func(ev eventbus.Event) {
			aMu.Lock()
			defer aMu.Unlock()
			aEvents = append(aEvents, ev)
		},
	})
	if err != nil {
		t.Fatalf("NewSession A 失败: %v", err)
	}
	b, err := rt.NewSession(context.Background(), runtime.SessionOptions{ID: "sess-obs-b"})
	if err != nil {
		t.Fatalf("NewSession B 失败: %v", err)
	}
	if _, err := a.Run(context.Background(), "问 A"); err != nil {
		t.Fatalf("A.Run 失败: %v", err)
	}
	if _, err := b.Run(context.Background(), "问 B"); err != nil {
		t.Fatalf("B.Run 失败: %v", err)
	}

	// 事件转发是异步的：轮询等两边都收到本会话的终态事件。
	deadline := time.Now().Add(5 * time.Second)
	for {
		aMu.Lock()
		aDone := hasEventType(aEvents, agentloop.EventSessionState)
		aMu.Unlock()
		baseMu.Lock()
		baseDone := hasEventType(baseEvents, agentloop.EventSessionState) && baseHasBoth(baseEvents)
		baseMu.Unlock()
		if aDone && baseDone {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("等事件超时: aEvents=%d baseEvents=%d", len(aEvents), len(baseEvents))
		}
		time.Sleep(20 * time.Millisecond)
	}

	// 会话级：只收本会话事件。
	aMu.Lock()
	for _, ev := range aEvents {
		if ev.SessionID != "sess-obs-a" {
			t.Fatalf("会话级 Observer 泄漏了别的会话事件: session=%s type=%s", ev.SessionID, ev.Type)
		}
	}
	aMu.Unlock()
	// Runtime 级：两个会话都收得到。
	baseMu.Lock()
	if !baseHasBoth(baseEvents) {
		t.Fatalf("Runtime 级应收到两个会话的事件，got sessions=%v", sessionsOf(baseEvents))
	}
	baseMu.Unlock()

	// Close 统一 detach（会话级登记表抽干）：不 panic、不悬挂。
	if err := rt.Close(); err != nil {
		t.Fatalf("Close 失败: %v", err)
	}
	if err := rt.Close(); err != nil {
		t.Fatalf("Close 幂等失败: %v", err)
	}
}

// hasEventType 列表里是否有指定类型事件。
func hasEventType(evs []eventbus.Event, typ string) bool {
	for _, ev := range evs {
		if ev.Type == typ {
			return true
		}
	}
	return false
}

// baseHasBoth Runtime 级是否收到了两个目标会话的事件。
func baseHasBoth(evs []eventbus.Event) bool {
	seen := map[string]bool{}
	for _, ev := range evs {
		seen[ev.SessionID] = true
	}
	return seen["sess-obs-a"] && seen["sess-obs-b"]
}

// sessionsOf 事件列表里的会话集合（诊断输出用）。
func sessionsOf(evs []eventbus.Event) []string {
	seen := map[string]bool{}
	var out []string
	for _, ev := range evs {
		if !seen[ev.SessionID] {
			seen[ev.SessionID] = true
			out = append(out, ev.SessionID)
		}
	}
	return out
}

// TestSessionOverlayApprovalFailClosed 覆盖工具面里有声明审批的工具、
// Runtime 级没装配闸 ⇒ NewSession 即报（fail-closed 判定在打开时，不等
// 第一次 Run——事后拦截已经晚了，副作用可能已施加）。
func TestSessionOverlayApprovalFailClosed(t *testing.T) {
	rt, err := runtime.NewEmbedded(runtime.Config{
		Model: &scriptedModel{},
		Tools: []pipeline.Tool{{Name: "plain", Safe: true,
			Handler: func(context.Context, []byte) (string, error) { return "ok", nil }}},
	})
	if err != nil {
		t.Fatalf("装配失败: %v", err)
	}
	defer func() { _ = rt.Close() }()

	_, err = rt.NewSession(context.Background(), runtime.SessionOptions{
		ID: "sess-approval",
		Tools: []pipeline.Tool{{
			Name: "danger", Safe: true, NeedsApproval: true,
			Handler: func(context.Context, []byte) (string, error) { return "ok", nil },
		}},
	})
	if err == nil {
		t.Fatal("覆盖工具面声明了审批却没有闸，NewSession 应 fail-closed 拒绝")
	}
	if !strings.Contains(err.Error(), "审批") {
		t.Fatalf("错误应指明审批闸缺失，got: %v", err)
	}
	// 无覆盖的会话不受影响（基座工具面没有审批需求）。
	if _, err := rt.NewSession(context.Background(), runtime.SessionOptions{ID: "sess-plain"}); err != nil {
		t.Fatalf("普通会话不应被影响: %v", err)
	}
}
