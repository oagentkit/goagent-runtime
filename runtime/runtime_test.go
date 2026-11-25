package runtime_test

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/oagentkit/goagent-kernel/checkpoint"
	"github.com/oagentkit/goagent-kernel/contract"
	"github.com/oagentkit/goagent-kernel/eventbus"

	"github.com/oagentkit/goagent-runtime/agentloop"
	"github.com/oagentkit/goagent-runtime/chat"
	"github.com/oagentkit/goagent-runtime/pipeline"
	"github.com/oagentkit/goagent-runtime/runtime"
	"github.com/oagentkit/goagent-runtime/storage"
)

// TestNewEmbeddedRejectsApprovalToolWithoutGate 标准模板的审批闸装配判定：
// 工具声明了 NeedsApproval 而宿主没装配审批闸 ⇒ 装配失败（不放行）。
//
// 判据在**装配期**，不在第一次执行时——事后拦截已经晚了（副作用已施加）。
func TestNewEmbeddedRejectsApprovalToolWithoutGate(t *testing.T) {
	tools := []pipeline.Tool{{
		Name: "danger", Safe: true,
		Handler:       func(context.Context, []byte) (string, error) { return "ok", nil },
		NeedsApproval: true,
	}}
	if _, err := runtime.NewEmbedded(runtime.Config{
		Model: &scriptedModel{},
		Tools: tools,
	}); err == nil {
		t.Fatal("声明了审批的工具却无审批闸装配成功（审批会被绕过）")
	}
	// 装配了审批闸 ⇒ 允许装配。
	rt, err := runtime.NewEmbedded(runtime.Config{
		Model: &scriptedModel{},
		Tools: tools,
		ApprovalGate: func(context.Context, string, chat.ToolCall) (agentloop.Approval, error) {
			return agentloop.ApprovalApprove, nil
		},
	})
	if err != nil {
		t.Fatalf("已装配审批闸却拒绝装配: %v", err)
	}
	_ = rt.Close()
}

// scriptedModel 脚本化模型（与 agentloop 测试同款，测试包独立各写一份）。
type scriptedModel struct {
	mu     sync.Mutex
	calls  int
	script []chat.ChatResponse
}

func (m *scriptedModel) Chat(_ context.Context, _ chat.ChatRequest) (chat.ChatResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	i := m.calls
	m.calls++
	if i < len(m.script) {
		return m.script[i], nil
	}
	return chat.ChatResponse{Content: "fallback"}, nil
}

type tally struct {
	mu    sync.Mutex
	calls int
}

func (t *tally) handler(_ context.Context, _ []byte) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.calls++
	return "ok", nil
}

func (t *tally) count() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.calls
}

func demoTools(tl *tally) []pipeline.Tool {
	return []pipeline.Tool{{Name: "calc", Description: "算一下", Safe: true, Handler: tl.handler}}
}

// TestEmbeddedEndToEnd embedded 模板完整闭环。
func TestEmbeddedEndToEnd(t *testing.T) {
	tl := &tally{}
	rt, err := runtime.NewEmbedded(runtime.Config{
		Model: &scriptedModel{script: []chat.ChatResponse{
			{ToolCalls: []chat.ToolCall{{ID: "call-1", Name: "calc", Arguments: "{}"}}},
			{Content: "做完了"},
		}},
		Tools: demoTools(tl),
	})
	if err != nil {
		t.Fatalf("装配失败: %v", err)
	}
	defer func() { _ = rt.Close() }()

	res, err := rt.Run(context.Background(), "sess-1", "开始")
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	if res.Status != contract.StatusSucceeded || res.Final != "做完了" {
		t.Fatalf("结果错: %+v", res)
	}
	if tl.count() != 1 {
		t.Fatalf("工具应施加一次，got %d", tl.count())
	}
}

// TestEmbeddedResumeAcrossRuntimes 模拟进程重启：共享同一存储的两个
// Runtime 接力同一会话，恢复重放不重复施加。
func TestEmbeddedResumeAcrossRuntimes(t *testing.T) {
	tl := &tally{}
	mem := storage.NewMem()

	// 进程 1：正常完成一轮工具调用（历史已落快照），随后 Close。
	m1 := &scriptedModel{script: []chat.ChatResponse{
		{ToolCalls: []chat.ToolCall{{ID: "call-1", Name: "calc", Arguments: "{}"}}},
	}}
	rt1, err := runtime.NewEmbedded(runtime.Config{
		Model: m1, Tools: demoTools(tl),
	}, runtime.WithStorage(mem))
	if err != nil {
		t.Fatalf("装配失败: %v", err)
	}
	if _, err := rt1.Run(context.Background(), "sess-1", "开始"); err != nil {
		t.Fatalf("第一次 Run 失败: %v", err)
	}
	_ = rt1.Close()

	// 进程 2：全新 Runtime，同存储同会话。模型按历史重放同一调用。
	m2 := &scriptedModel{script: []chat.ChatResponse{
		{ToolCalls: []chat.ToolCall{{ID: "call-1", Name: "calc", Arguments: "{}"}}},
		{Content: "接力完成"},
	}}
	rt2, err := runtime.NewEmbedded(runtime.Config{
		Model: m2, Tools: demoTools(tl),
	}, runtime.WithStorage(mem))
	if err != nil {
		t.Fatalf("装配失败: %v", err)
	}
	defer func() { _ = rt2.Close() }()

	res, err := rt2.Run(context.Background(), "sess-1", "继续")
	if err != nil {
		t.Fatalf("恢复 Run 失败: %v", err)
	}
	if res.Status != contract.StatusSucceeded {
		t.Fatalf("恢复后应为 succeeded，got %s", res.Status)
	}
	if tl.count() != 1 {
		t.Fatalf("跨 Runtime 重放不得重复施加：got %d 次", tl.count())
	}
}

// TestStandaloneEndToEnd standalone 模板：真 SQLite，Close 后重开同一会话
// 能从快照恢复。
func TestStandaloneEndToEnd(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rt.db")
	tl := &tally{}

	// 进程 1：跑完一个工具回合 + 收尾。
	rt1, err := runtime.NewStandalone(runtime.Config{
		Model: &scriptedModel{script: []chat.ChatResponse{
			{ToolCalls: []chat.ToolCall{{ID: "call-1", Name: "calc", Arguments: "{}"}}},
			{Content: "第一段完成"},
		}},
		Tools: demoTools(tl),
	}, path)
	if err != nil {
		t.Fatalf("装配失败: %v", err)
	}
	res, err := rt1.Run(context.Background(), "sess-1", "开始")
	if err != nil || res.Status != contract.StatusSucceeded {
		t.Fatalf("第一段失败: err=%v res=%+v", err, res)
	}
	if err := rt1.Close(); err != nil {
		t.Fatalf("关库失败: %v", err)
	}

	// 进程 2：新 Runtime 打开同一库，同会话继续（历史在 SQLite 里）。
	rt2, err := runtime.NewStandalone(runtime.Config{
		Model: &scriptedModel{script: []chat.ChatResponse{
			{Content: "我记得之前算过了"},
		}},
		Tools: demoTools(tl),
	}, path)
	if err != nil {
		t.Fatalf("重开失败: %v", err)
	}
	defer func() { _ = rt2.Close() }()

	res2, err := rt2.Run(context.Background(), "sess-1", "继续")
	if err != nil {
		t.Fatalf("续跑失败: %v", err)
	}
	if res2.Status != contract.StatusSucceeded || res2.Final != "我记得之前算过了" {
		t.Fatalf("续跑结果错: %+v", res2)
	}
}

// TestObserverReceivesEvents 观测回调经事件面收到循环事件。
func TestObserverReceivesEvents(t *testing.T) {
	var mu sync.Mutex
	var types []string
	tl := &tally{}
	rt, err := runtime.NewEmbedded(runtime.Config{
		Model: &scriptedModel{script: []chat.ChatResponse{
			{Content: "直接回答"},
		}},
		Tools: demoTools(tl),
		Observer: func(ev eventbus.Event) {
			mu.Lock()
			defer mu.Unlock()
			types = append(types, ev.Type)
		},
	})
	if err != nil {
		t.Fatalf("装配失败: %v", err)
	}
	defer func() { _ = rt.Close() }()

	if _, err := rt.Run(context.Background(), "sess-1", "开始"); err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	// 事件转发是异步 goroutine：轮询等待，不裸断言。
	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(types) > 0
	})
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("等待条件超时")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestNilModelRejected 装配期 fail fast。
func TestNilModelRejected(t *testing.T) {
	if _, err := runtime.NewEmbedded(runtime.Config{}); err == nil {
		t.Fatal("缺模型必须装配期报错")
	}
	if _, err := runtime.NewStandalone(runtime.Config{Model: &scriptedModel{}}, ""); err == nil {
		t.Fatal("空 SQLite 路径必须装配期报错")
	}
}

// jointSpyProvider 能力断言用 Provider：Mem 全功能 + 内存事件面 +
// CommitWithEvents 计数（assemble 是否选中 Joint 模式的可观测面）。
type jointSpyProvider struct {
	*storage.Mem
	events eventbus.Store
	joints int
}

func (s *jointSpyProvider) Events() eventbus.Store { return s.events }

func (s *jointSpyProvider) CommitWithEvents(_ context.Context, _ checkpoint.Snapshot, _ []eventbus.Event) error {
	s.joints++
	return nil
}

// TestAssemblePicksJointProvider assemble 的 Joint 能力断言回归钉：Provider
// 实现 JointCommitter 且 Events 非 nil ⇒ 必须选 Joint 模式（commit 走
// CommitWithEvents）。批662 遗留缺口的堵点——只给 AsJoint 转发器时类型
// 断言恒 false、NewStandalone 静默落回两步 DurablePublisher 路径，本用例
// 让该形态恒红。
func TestAssemblePicksJointProvider(t *testing.T) {
	spy := &jointSpyProvider{Mem: storage.NewMem(), events: eventbus.NewMemoryStore()}
	m := &scriptedModel{script: []chat.ChatResponse{{Content: "ok"}}}
	rt, err := runtime.NewEmbedded(runtime.Config{
		Model: m,
		Tools: []pipeline.Tool{},
	}, runtime.WithStorage(spy))
	if err != nil {
		t.Fatalf("装配失败: %v", err)
	}
	defer func() { _ = rt.Close() }()

	res, err := rt.Run(context.Background(), "sess-detect", "hi")
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	if res.Status != contract.StatusSucceeded {
		t.Fatalf("应 succeeded，got %s", res.Status)
	}
	if spy.joints == 0 {
		t.Fatal("Provider 实现 JointCommitter 且 Events 非 nil ⇒ assemble 必须选 Joint 模式（commit 走 CommitWithEvents）")
	}
}
