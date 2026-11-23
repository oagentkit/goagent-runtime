package agentloop_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	kerrors "github.com/oekit/pkg/errors"

	"github.com/yi-nology/goagent-kernel/checkpoint"
	"github.com/yi-nology/goagent-kernel/contract"
	"github.com/yi-nology/goagent-kernel/roundguard"

	"github.com/yi-nology/goagent-runtime/agentloop"
	"github.com/yi-nology/goagent-runtime/chat"
	"github.com/yi-nology/goagent-runtime/pipeline"
	"github.com/yi-nology/goagent-runtime/storage"
)

// fakeModel 脚本化模型：第 N 次 Chat 返回 script[N]；errAt >= 0 时
// 该次调用返回错误（模拟进程死在模型调用上；-1 = 不注入错误）。
type fakeModel struct {
	mu     sync.Mutex
	calls  int
	script []chat.ChatResponse
	errAt  int
	last   chat.ChatRequest
}

func (f *fakeModel) Chat(_ context.Context, req chat.ChatRequest) (chat.ChatResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.last = req
	i := f.calls
	f.calls++
	if f.errAt >= 0 && f.errAt == i {
		return chat.ChatResponse{}, kerrors.New("模拟进程死亡")
	}
	if i < len(f.script) {
		return f.script[i], nil
	}
	return chat.ChatResponse{Content: "fallback-final"}, nil
}

func (f *fakeModel) lastRequest() chat.ChatRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.last
}

// counter 工具执行计数（重放不得重复施加的证据）。
type counter struct {
	mu    sync.Mutex
	calls int
}

func (c *counter) handler(_ context.Context, _ []byte) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	return "ok", nil
}

func (c *counter) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// newTestLoop 装配测试循环：内存存储 + 单工具 + 默认预算。
func newTestLoop(t *testing.T, m chat.Model, c *counter, budget roundguard.Budget) (*agentloop.Loop, *storage.Mem) {
	t.Helper()
	mem := storage.NewMem()
	pipe, err := pipeline.New(mem.Effects(), time.Minute, []pipeline.Tool{{
		Name: "calc", Safe: true, Handler: c.handler,
	}})
	if err != nil {
		t.Fatalf("建管线失败: %v", err)
	}
	l, err := agentloop.New(agentloop.Config{
		Model:  m,
		Tools:  pipe,
		Store:  mem.Checkpoints(),
		Budget: budget,
		System: "you are a test bot",
	})
	if err != nil {
		t.Fatalf("建循环失败: %v", err)
	}
	return l, mem
}

func TestCompleteLoop(t *testing.T) {
	m := &fakeModel{errAt: -1, script: []chat.ChatResponse{
		{ToolCalls: []chat.ToolCall{{ID: "call-1", Name: "calc", Arguments: "{}"}}},
		{Content: "最终回答"},
	}}
	c := &counter{}
	l, _ := newTestLoop(t, m, c, roundguard.Budget{})

	res, err := l.Run(context.Background(), "sess-1", "帮我算一下")
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	if res.Status != contract.StatusSucceeded {
		t.Fatalf("状态应为 succeeded，got %s", res.Status)
	}
	if res.Final != "最终回答" {
		t.Fatalf("终答应回填，got %q", res.Final)
	}
	if res.Rounds != 2 || res.ToolCalls != 1 {
		t.Fatalf("回合/工具计数错：rounds=%d tools=%d", res.Rounds, res.ToolCalls)
	}
	if c.count() != 1 {
		t.Fatalf("工具应只施加一次，got %d", c.count())
	}

	// 模型第二次请求应带着完整历史（用户输入 + assistant 调用 + tool 结果）。
	last := m.lastRequest()
	if len(last.Messages) != 4 {
		t.Fatalf("第二次请求应含 4 条历史，got %d", len(last.Messages))
	}
	if last.Messages[0].Role != chat.RoleSystem || !strings.Contains(last.Messages[0].Content, "test bot") {
		t.Fatalf("历史首条应为系统提示，got %+v", last.Messages[0])
	}
}

// TestResumeAfterModelDeath 恢复语义的核心用例：进程死在第二轮模型调用
// 前（工具已施加并落快照）；新循环读回历史，模型重放同一 toolCallID，
// 副作用闸判 replayed——执行体不得再次施加。
func TestResumeAfterModelDeath(t *testing.T) {
	c := &counter{}
	mem := storage.NewMem()

	// 进程 1：第一回合请求工具并施加，第二回合模型调用前死亡。
	m1 := &fakeModel{
		errAt: 1,
		script: []chat.ChatResponse{
			{ToolCalls: []chat.ToolCall{{ID: "call-1", Name: "calc", Arguments: "{}"}}},
		},
	}
	pipe1, err := pipeline.New(mem.Effects(), time.Minute, []pipeline.Tool{{Name: "calc", Safe: true, Handler: c.handler}})
	if err != nil {
		t.Fatalf("建管线失败: %v", err)
	}
	l1, err := agentloop.New(agentloop.Config{Model: m1, Tools: pipe1, Store: mem.Checkpoints()})
	if err != nil {
		t.Fatalf("建循环失败: %v", err)
	}
	if _, err := l1.Run(context.Background(), "sess-1", "开始"); err == nil {
		t.Fatal("第一次 Run 应报模型死亡错误")
	}
	if c.count() != 1 {
		t.Fatalf("死亡前工具应已施加一次，got %d", c.count())
	}

	// 进程 2：全新模型实例与循环，共享同一存储。模型按历史重放同一调用。
	m2 := &fakeModel{errAt: -1, script: []chat.ChatResponse{
		{ToolCalls: []chat.ToolCall{{ID: "call-1", Name: "calc", Arguments: "{}"}}},
		{Content: "恢复后的最终回答"},
	}}
	pipe2, err := pipeline.New(mem.Effects(), time.Minute, []pipeline.Tool{{Name: "calc", Safe: true, Handler: c.handler}})
	if err != nil {
		t.Fatalf("建管线失败: %v", err)
	}
	l2, err := agentloop.New(agentloop.Config{Model: m2, Tools: pipe2, Store: mem.Checkpoints()})
	if err != nil {
		t.Fatalf("建循环失败: %v", err)
	}
	res, err := l2.Run(context.Background(), "sess-1", "继续")
	if err != nil {
		t.Fatalf("恢复 Run 失败: %v", err)
	}
	if res.Status != contract.StatusSucceeded {
		t.Fatalf("恢复后应为 succeeded，got %s", res.Status)
	}
	if res.Final != "恢复后的最终回答" {
		t.Fatalf("终答错，got %q", res.Final)
	}
	if c.count() != 1 {
		t.Fatalf("恢复重放不得重复施加：执行体仍应只调 1 次，got %d", c.count())
	}
}

// TestBudgetForceFinal 预算用尽（ForceFinal 策略）：注入收尾提示，模型
// 有机会给终答。
func TestBudgetForceFinal(t *testing.T) {
	m := &fakeModel{errAt: -1, script: []chat.ChatResponse{
		{ToolCalls: []chat.ToolCall{{ID: "call-1", Name: "calc", Arguments: "{}"}}},
		{ToolCalls: []chat.ToolCall{{ID: "call-2", Name: "calc", Arguments: "{}"}}},
		{Content: "预算内总结"},
	}}
	c := &counter{}
	l, _ := newTestLoop(t, m, c, roundguard.Budget{
		MaxRounds: 2, Policy: roundguard.PolicyForceFinal,
	})

	res, err := l.Run(context.Background(), "sess-1", "开始")
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	if res.Status != contract.StatusSucceeded {
		t.Fatalf("收尾轮给出终答应为 succeeded，got %s", res.Status)
	}
	if c.count() != 2 {
		t.Fatalf("前两回合各一次工具调用，got %d", c.count())
	}
}

// TestBudgetForceFinalStillToolingTimesOut 收尾轮仍要工具：强制结束，
// 状态 timed_out——额度口径不放行无限循环。
func TestBudgetForceFinalStillToolingTimesOut(t *testing.T) {
	m := &fakeModel{errAt: -1, script: []chat.ChatResponse{
		{ToolCalls: []chat.ToolCall{{ID: "call-1", Name: "calc", Arguments: "{}"}}},
		{ToolCalls: []chat.ToolCall{{ID: "call-2", Name: "calc", Arguments: "{}"}}},
	}}
	c := &counter{}
	l, _ := newTestLoop(t, m, c, roundguard.Budget{
		MaxRounds: 1, Policy: roundguard.PolicyForceFinal,
	})

	res, err := l.Run(context.Background(), "sess-1", "开始")
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	if res.Status != contract.StatusTimedOut {
		t.Fatalf("收尾轮仍要工具应为 timed_out，got %s", res.Status)
	}
}

// TestBudgetForceStop 非 ForceFinal 策略：额度用尽直接停止。
func TestBudgetForceStop(t *testing.T) {
	m := &fakeModel{errAt: -1, script: []chat.ChatResponse{
		{ToolCalls: []chat.ToolCall{{ID: "call-1", Name: "calc", Arguments: "{}"}}},
		{Content: "到不了的终答"},
	}}
	c := &counter{}
	l, _ := newTestLoop(t, m, c, roundguard.Budget{
		MaxRounds: 1, Policy: roundguard.PolicyFailFast,
	})

	res, err := l.Run(context.Background(), "sess-1", "开始")
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	if res.Status != contract.StatusTimedOut {
		t.Fatalf("应为 timed_out，got %s", res.Status)
	}
	if res.Final != "" {
		t.Fatalf("强制停止不得有终答，got %q", res.Final)
	}
}

// TestPerToolLimit 单工具额度：call-1 放行、call-2 被额度拦下。
func TestPerToolLimit(t *testing.T) {
	m := &fakeModel{errAt: -1, script: []chat.ChatResponse{
		{ToolCalls: []chat.ToolCall{
			{ID: "call-1", Name: "calc", Arguments: "{}"},
			{ID: "call-2", Name: "calc", Arguments: "{}"},
		}},
		{Content: "部分完成"},
	}}
	c := &counter{}
	l, _ := newTestLoop(t, m, c, roundguard.Budget{
		MaxPerTool: map[string]int{"calc": 1},
	})

	res, err := l.Run(context.Background(), "sess-1", "开始")
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	if res.Status != contract.StatusTimedOut {
		t.Fatalf("超单工具额度应为 timed_out，got %s", res.Status)
	}
	if c.count() != 1 {
		t.Fatalf("第二个调用应被额度拦下，执行体仍只调 1 次，got %d", c.count())
	}
}

// TestCorruptHistoryRejected 不完整历史（assistant 请求了工具但结果缺失）
// = 外部篡改/实现漂移：fail-closed，不猜。
func TestCorruptHistoryRejected(t *testing.T) {
	c := &counter{}
	mem := storage.NewMem()
	// 手工写入一份不完整历史（assistant 请求了工具但结果缺失）。
	raw, err := json.Marshal([]chat.Message{
		{Role: chat.RoleAssistant, ToolCalls: []chat.ToolCall{{ID: "call-x", Name: "calc", Arguments: "{}"}}},
	})
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	snap := checkpoint.Snapshot{
		RunID: "sess-1", Seq: 1, Status: contract.StatusRunning,
		Fields: map[string]string{"history": string(raw)},
	}
	if err := mem.Checkpoints().SaveIfNewer(context.Background(), snap); err != nil {
		t.Fatalf("写快照失败: %v", err)
	}

	m := &fakeModel{errAt: -1}
	pipe, _ := pipeline.New(mem.Effects(), time.Minute, []pipeline.Tool{{Name: "calc", Safe: true, Handler: c.handler}})
	l, err := agentloop.New(agentloop.Config{Model: m, Tools: pipe, Store: mem.Checkpoints()})
	if err != nil {
		t.Fatalf("建循环失败: %v", err)
	}
	if _, err := l.Run(context.Background(), "sess-1", "继续"); err == nil {
		t.Fatal("不完整历史必须报错，不得静默续跑")
	}
}

// TestNilPartsRejected 装配期 fail fast。
func TestNilPartsRejected(t *testing.T) {
	c := &counter{}
	mem := storage.NewMem()
	pipe, _ := pipeline.New(mem.Effects(), time.Minute, []pipeline.Tool{{Name: "calc", Safe: true, Handler: c.handler}})
	if _, err := agentloop.New(agentloop.Config{Tools: pipe, Store: mem.Checkpoints()}); err == nil {
		t.Fatal("缺 Model 必须装配期报错")
	}
	if _, err := agentloop.New(agentloop.Config{Model: &fakeModel{errAt: -1}, Store: mem.Checkpoints()}); err == nil {
		t.Fatal("缺 Tools 必须装配期报错")
	}
	if _, err := agentloop.New(agentloop.Config{Model: &fakeModel{errAt: -1}, Tools: pipe}); err == nil {
		t.Fatal("缺 Store 必须装配期报错")
	}
	if _, err := agentloop.New(agentloop.Config{
		Model: &fakeModel{errAt: -1}, Tools: pipe, Store: mem.Checkpoints(),
		Budget: roundguard.Budget{MaxRounds: -1},
	}); err == nil {
		t.Fatal("非法预算必须装配期报错（负额度不得静默降级为不限）")
	}
}

// TestForceFinalPromptCustom 收尾提示词宿主注入（runtime issue #3）：
// 提示词是领域概念，硬编码在 loop 层=宿主 i18n/调口径无接缝。
// 断言收尾轮注入的是宿主文案；缺省回落路径由既有 TestBudgetForceFinal
// 的行为面覆盖（文案逐字保留）。
func TestForceFinalPromptCustom(t *testing.T) {
	mem := storage.NewMem()
	c := &counter{}
	pipe, err := pipeline.New(mem.Effects(), time.Minute, []pipeline.Tool{{Name: "calc", Safe: true, Handler: c.handler}})
	if err != nil {
		t.Fatalf("建管线失败: %v", err)
	}
	m := &fakeModel{errAt: -1, script: []chat.ChatResponse{
		{ToolCalls: []chat.ToolCall{{ID: "call-1", Name: "calc", Arguments: "{}"}}},
		{ToolCalls: []chat.ToolCall{{ID: "call-2", Name: "calc", Arguments: "{}"}}},
		{Content: "自定义收尾终答"},
	}}
	l, err := agentloop.New(agentloop.Config{
		Model: m, Tools: pipe, Store: mem.Checkpoints(),
		Budget:           roundguard.Budget{MaxRounds: 2, Policy: roundguard.PolicyForceFinal},
		ForceFinalPrompt: "预算告罄：请用现有材料作答。",
	})
	if err != nil {
		t.Fatalf("建循环失败: %v", err)
	}

	res, err := l.Run(context.Background(), "sess-ffp", "开始")
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	if res.Status != contract.StatusSucceeded {
		t.Fatalf("收尾轮给出终答应为 succeeded，got %s", res.Status)
	}
	last := m.lastRequest()
	n := len(last.Messages)
	if n == 0 || last.Messages[n-1].Role != chat.RoleUser || last.Messages[n-1].Content != "预算告罄：请用现有材料作答。" {
		t.Fatalf("收尾轮应注入宿主自定义文案，got %+v", last.Messages[n-1])
	}
}

// failEmitter 恒失败的发射面（issue #2 可观测面用例）。
type failEmitter struct{}

func (failEmitter) Publish(_ context.Context, _ string, _ string, _ *uint64, _ string, _ string, _ map[string]any) error {
	return kerrors.New("事件总线不可达")
}

// TestEmitFailureObservable 发射失败不再静默吞（runtime issue #2）：
// 计数进 Result.EmitFailed，宿主 OnEmitError 逐次回调——孤儿事件/
// 终态事件丢失从「无人知」变可观测。
func TestEmitFailureObservable(t *testing.T) {
	mem := storage.NewMem()
	c := &counter{}
	pipe, err := pipeline.New(mem.Effects(), time.Minute, []pipeline.Tool{{Name: "calc", Safe: true, Handler: c.handler}})
	if err != nil {
		t.Fatalf("建管线失败: %v", err)
	}
	var cb int
	m := &fakeModel{errAt: -1, script: []chat.ChatResponse{
		{ToolCalls: []chat.ToolCall{{ID: "call-1", Name: "calc", Arguments: "{}"}}},
		{Content: "终答"},
	}}
	l, err := agentloop.New(agentloop.Config{
		Model: m, Tools: pipe, Store: mem.Checkpoints(),
		Emitter:     failEmitter{},
		OnEmitError: func(err error) { cb++ },
	})
	if err != nil {
		t.Fatalf("建循环失败: %v", err)
	}

	res, err := l.Run(context.Background(), "sess-emit", "开始")
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	if res.Status != contract.StatusSucceeded {
		t.Fatalf("应 succeeded，got %s", res.Status)
	}
	// 两轮 round_start + 一次 tool_call + 终态 session_state = 4 次发射。
	if res.EmitFailed != 4 {
		t.Fatalf("四轮事件应全失败计数 4，got %d", res.EmitFailed)
	}
	if cb != 4 {
		t.Fatalf("OnEmitError 应逐次回调 4 次，got %d", cb)
	}
}

// approvalGateSpy 记录询问次数的审批闸（并发面用 mutex 锁住）。
type approvalGateSpy struct {
	mu       sync.Mutex
	asked    int
	decision agentloop.Approval
	err      error
}

func (g *approvalGateSpy) gate(_ context.Context, _ string, _ chat.ToolCall) (agentloop.Approval, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.asked++
	return g.decision, g.err
}

func (g *approvalGateSpy) asks() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.asked
}

// newGatedLoop 装配带审批闸的测试循环（单工具 calc，NeedsApproval 可调）。
func newGatedLoop(t *testing.T, m chat.Model, c *counter, budget roundguard.Budget,
	needsApproval bool, gate func(context.Context, string, chat.ToolCall) (agentloop.Approval, error)) (*agentloop.Loop, *storage.Mem) {
	t.Helper()
	mem := storage.NewMem()
	pipe, err := pipeline.New(mem.Effects(), time.Minute, []pipeline.Tool{{
		Name: "calc", Safe: true, Handler: c.handler, NeedsApproval: needsApproval,
	}})
	if err != nil {
		t.Fatalf("建管线失败: %v", err)
	}
	l, err := agentloop.New(agentloop.Config{
		Model: m, Tools: pipe, Store: mem.Checkpoints(),
		Budget: budget, ApprovalGate: gate,
	})
	if err != nil {
		t.Fatalf("建循环失败: %v", err)
	}
	return l, mem
}

// TestApprovalGateApproved 声明 NeedsApproval 的工具执行前过闸；批准 ⇒ 施加。
func TestApprovalGateApproved(t *testing.T) {
	m := &fakeModel{errAt: -1, script: []chat.ChatResponse{
		{ToolCalls: []chat.ToolCall{{ID: "call-1", Name: "calc", Arguments: "{}"}}},
		{Content: "终答"},
	}}
	c := &counter{}
	g := &approvalGateSpy{decision: agentloop.ApprovalApprove}
	l, _ := newGatedLoop(t, m, c, roundguard.Budget{MaxRounds: 4}, true, g.gate)

	res, err := l.Run(context.Background(), "sess-gate-ok", "开始")
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	if res.Status != contract.StatusSucceeded {
		t.Fatalf("批准后应走完回合，got %s", res.Status)
	}
	if c.count() != 1 {
		t.Fatalf("批准后工具应执行 1 次，got %d", c.count())
	}
	if g.asks() != 1 {
		t.Fatalf("闸应被询问 1 次，got %d", g.asks())
	}
}

// TestApprovalGateDenied 拒绝 ⇒ 副作用不施加，拒绝以工具观察回喂模型，
// 模型可改道收尾。
func TestApprovalGateDenied(t *testing.T) {
	m := &fakeModel{errAt: -1, script: []chat.ChatResponse{
		{ToolCalls: []chat.ToolCall{{ID: "call-1", Name: "calc", Arguments: "{}"}}},
		{Content: "收到拒绝，改道终答"},
	}}
	c := &counter{}
	g := &approvalGateSpy{decision: agentloop.ApprovalDeny}
	l, _ := newGatedLoop(t, m, c, roundguard.Budget{MaxRounds: 4}, true, g.gate)

	res, err := l.Run(context.Background(), "sess-gate-deny", "开始")
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	if res.Status != contract.StatusSucceeded {
		t.Fatalf("拒绝后模型改道终答应 succeeded，got %s", res.Status)
	}
	if c.count() != 0 {
		t.Fatalf("拒绝后副作用不得施加，执行计数应 0，got %d", c.count())
	}
	if g.asks() != 1 {
		t.Fatalf("闸应被询问 1 次，got %d", g.asks())
	}
	// 拒绝观察进了历史：最后一次模型请求的 messages 里应含拒绝文本。
	for _, msg := range m.lastRequest().Messages {
		if msg.Role == chat.RoleTool && strings.Contains(msg.Content, "审批拒绝") {
			return
		}
	}
	t.Fatal("拒绝观察未回喂模型（messages 里找不到含「审批拒绝」的 tool 消息）")
}

// TestApprovalGateError 闸失败 ⇒ fail-closed：本轮失败上抛，工具不执行。
func TestApprovalGateError(t *testing.T) {
	m := &fakeModel{errAt: -1, script: []chat.ChatResponse{
		{ToolCalls: []chat.ToolCall{{ID: "call-1", Name: "calc", Arguments: "{}"}}},
	}}
	c := &counter{}
	g := &approvalGateSpy{err: kerrors.New("审批服务不可达")}
	l, _ := newGatedLoop(t, m, c, roundguard.Budget{MaxRounds: 4}, true, g.gate)

	res, err := l.Run(context.Background(), "sess-gate-err", "开始")
	if err == nil {
		t.Fatal("闸失败必须 fail-closed 上抛")
	}
	if c.count() != 0 {
		t.Fatalf("闸失败后副作用不得施加，执行计数应 0，got %d", c.count())
	}
	if !strings.Contains(err.Error(), "审批闸失败") {
		t.Fatalf("错误应含「审批闸失败」语境，got %v", err)
	}
	_ = res
}

// TestApprovalGateOnlyMarkedTools 未声明 NeedsApproval 的工具不过闸、直通执行。
func TestApprovalGateOnlyMarkedTools(t *testing.T) {
	m := &fakeModel{errAt: -1, script: []chat.ChatResponse{
		{ToolCalls: []chat.ToolCall{{ID: "call-1", Name: "calc", Arguments: "{}"}}},
		{Content: "终答"},
	}}
	c := &counter{}
	g := &approvalGateSpy{decision: agentloop.ApprovalApprove}
	l, _ := newGatedLoop(t, m, c, roundguard.Budget{MaxRounds: 4}, false, g.gate)

	res, err := l.Run(context.Background(), "sess-gate-skip", "开始")
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	if res.Status != contract.StatusSucceeded {
		t.Fatalf("应 succeeded，got %s", res.Status)
	}
	if c.count() != 1 {
		t.Fatalf("未标记工具应直通执行，got %d", c.count())
	}
	if g.asks() != 0 {
		t.Fatalf("未标记工具不得过闸，询问应 0 次，got %d", g.asks())
	}
}

// TestApprovalSuspendResume 异步形态全链：闸 Suspend ⇒ Run 返回 awaiting
// （PendingCallID 携带挂起调用）；Approve 批准 ⇒ 走标准施加面执行+续跑到终态。
func TestApprovalSuspendResume(t *testing.T) {
	m := &fakeModel{errAt: -1, script: []chat.ChatResponse{
		{ToolCalls: []chat.ToolCall{{ID: "call-1", Name: "calc", Arguments: "{}"}}},
		{Content: "续跑终答"},
	}}
	c := &counter{}
	g := &approvalGateSpy{decision: agentloop.ApprovalSuspend}
	l, mem := newGatedLoop(t, m, c, roundguard.Budget{MaxRounds: 4}, true, g.gate)

	res, err := l.Run(context.Background(), "sess-susp", "开始")
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	if res.Status != agentloop.StatusAwaitingApproval {
		t.Fatalf("挂起应返回 awaiting_approval，got %s", res.Status)
	}
	if res.PendingCallID != "call-1" {
		t.Fatalf("PendingCallID 应携带挂起调用，got %q", res.PendingCallID)
	}
	if c.count() != 0 {
		t.Fatalf("挂起时副作用不得施加，got %d", c.count())
	}
	snap, err := mem.Checkpoints().Load(context.Background(), "sess-susp")
	if err != nil {
		t.Fatalf("读挂起快照失败: %v", err)
	}
	if snap.Status != agentloop.StatusAwaitingApproval {
		t.Fatalf("快照状态应为 awaiting_approval，got %s", snap.Status)
	}
	res2, err := l.Run(context.Background(), "sess-susp", "开始")
	if err != nil {
		t.Fatalf("挂起态重复 Run 失败: %v", err)
	}
	if res2.Status != agentloop.StatusAwaitingApproval || res2.PendingCallID != "call-1" {
		t.Fatalf("挂起态重复 Run 应幂等短路，got %+v", res2)
	}
	if m.calls != 1 {
		t.Fatalf("重复 Run 不得调模型，模型调用应 1 次，got %d", m.calls)
	}

	res3, err := l.Approve(context.Background(), "sess-susp", "call-1", true)
	if err != nil {
		t.Fatalf("Approve 失败: %v", err)
	}
	if res3.Status != contract.StatusSucceeded {
		t.Fatalf("批准续跑应 succeeded，got %s", res3.Status)
	}
	if c.count() != 1 {
		t.Fatalf("批准后工具应执行 1 次，got %d", c.count())
	}
}

// TestApprovalSuspendDeny 挂起后拒绝：副作用不施加，拒绝观察回喂模型改道。
func TestApprovalSuspendDeny(t *testing.T) {
	m := &fakeModel{errAt: -1, script: []chat.ChatResponse{
		{ToolCalls: []chat.ToolCall{{ID: "call-1", Name: "calc", Arguments: "{}"}}},
		{Content: "拒绝后改道终答"},
	}}
	c := &counter{}
	g := &approvalGateSpy{decision: agentloop.ApprovalSuspend}
	l, _ := newGatedLoop(t, m, c, roundguard.Budget{MaxRounds: 4}, true, g.gate)

	if _, err := l.Run(context.Background(), "sess-deny", "开始"); err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	res, err := l.Approve(context.Background(), "sess-deny", "call-1", false)
	if err != nil {
		t.Fatalf("Approve 失败: %v", err)
	}
	if res.Status != contract.StatusSucceeded {
		t.Fatalf("拒绝后模型改道应 succeeded，got %s", res.Status)
	}
	if c.count() != 0 {
		t.Fatalf("拒绝后副作用不得施加，got %d", c.count())
	}
	found := false
	for _, msg := range m.lastRequest().Messages {
		if msg.Role == chat.RoleTool && strings.Contains(msg.Content, "审批拒绝") {
			found = true
		}
	}
	if !found {
		t.Fatal("拒绝观察未回喂模型")
	}
}

// TestApprovalSuspendWrongCallID callID 不匹配 ⇒ fail-closed 拒绝落决策，
// 会话保持挂起（幂等可重试）。
func TestApprovalSuspendWrongCallID(t *testing.T) {
	m := &fakeModel{errAt: -1, script: []chat.ChatResponse{
		{ToolCalls: []chat.ToolCall{{ID: "call-1", Name: "calc", Arguments: "{}"}}},
	}}
	c := &counter{}
	g := &approvalGateSpy{decision: agentloop.ApprovalSuspend}
	l, mem := newGatedLoop(t, m, c, roundguard.Budget{MaxRounds: 4}, true, g.gate)

	if _, err := l.Run(context.Background(), "sess-wrong", "开始"); err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	if _, err := l.Approve(context.Background(), "sess-wrong", "call-999", true); err == nil {
		t.Fatal("callID 不匹配必须拒绝落决策")
	}
	if c.count() != 0 {
		t.Fatalf("误决策不得施加副作用，got %d", c.count())
	}
	snap, _ := mem.Checkpoints().Load(context.Background(), "sess-wrong")
	if snap.Status != agentloop.StatusAwaitingApproval {
		t.Fatalf("拒绝决策后会话应保持挂起，got %s", snap.Status)
	}
	if _, err := l.Approve(context.Background(), "sess-wrong", "call-1", true); err != nil {
		t.Fatalf("正确 callID 重试应成功: %v", err)
	}
	if c.count() != 1 {
		t.Fatalf("重试批准后工具应执行，got %d", c.count())
	}
}
