package runtime_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/yi-nology/goagent-kernel/contract"

	"github.com/yi-nology/goagent-runtime/agentloop"
	"github.com/yi-nology/goagent-runtime/chat"
	"github.com/yi-nology/goagent-runtime/pipeline"
	"github.com/yi-nology/goagent-runtime/runtime"
)

// recordingModel 记录每次 Chat 收到的请求（验证历史共享用），按脚本应答。
type recordingModel struct {
	mu     sync.Mutex
	reqs   []chat.ChatRequest
	script []chat.ChatResponse
}

func (m *recordingModel) Chat(_ context.Context, req chat.ChatRequest) (chat.ChatResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reqs = append(m.reqs, req)
	i := len(m.reqs) - 1
	if i < len(m.script) {
		return m.script[i], nil
	}
	return chat.ChatResponse{Content: "fallback"}, nil
}

func (m *recordingModel) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.reqs)
}

// TestNewSessionRejectsEmptyID 空 ID 是编程错误：匿名会话无处可寻，且所有
// 空 ID 会话的历史会在同一锚点上互相碰撞——装配期拒绝，不留到恢复时才炸。
func TestNewSessionRejectsEmptyID(t *testing.T) {
	rt, err := runtime.NewEmbedded(runtime.Config{Model: &scriptedModel{}})
	if err != nil {
		t.Fatalf("装配失败: %v", err)
	}
	defer func() { _ = rt.Close() }()

	if _, err := rt.NewSession(context.Background(), runtime.SessionOptions{}); err == nil {
		t.Fatal("空 ID 应拒绝打开会话")
	}
	// Runtime.Run 走同一校验（薄委托）。
	if _, err := rt.Run(context.Background(), "", "hi"); err == nil {
		t.Fatal("空 sessionID 应拒绝执行")
	}
}

// TestSessionRunEndToEnd 会话对象的完整执行面：NewSession → Run → 终态。
func TestSessionRunEndToEnd(t *testing.T) {
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

	session, err := rt.NewSession(context.Background(), runtime.SessionOptions{ID: "sess-1"})
	if err != nil {
		t.Fatalf("NewSession 失败: %v", err)
	}
	if session.ID() != "sess-1" {
		t.Fatalf("会话 ID 错: %q", session.ID())
	}
	res, err := session.Run(context.Background(), "开始")
	if err != nil {
		t.Fatalf("session.Run 失败: %v", err)
	}
	if res.Status != contract.StatusSucceeded || res.Final != "做完了" {
		t.Fatalf("结果错: %+v", res)
	}
	if tl.count() != 1 {
		t.Fatalf("工具应施加一次，got %d", tl.count())
	}
}

// TestSessionSharesHistoryWithRuntimeRun 同一锚点上 Runtime.Run 与
// Session.Run 共享历史：第二次执行的模型请求里带着第一次的问答。
func TestSessionSharesHistoryWithRuntimeRun(t *testing.T) {
	m := &recordingModel{script: []chat.ChatResponse{
		{Content: "第一答"},
		{Content: "第二答"},
	}}
	rt, err := runtime.NewEmbedded(runtime.Config{Model: m})
	if err != nil {
		t.Fatalf("装配失败: %v", err)
	}
	defer func() { _ = rt.Close() }()

	// 第一次：兼容入口 Runtime.Run。
	if _, err := rt.Run(context.Background(), "sess-share", "第一问"); err != nil {
		t.Fatalf("Runtime.Run 失败: %v", err)
	}
	// 第二次：会话对象 Session.Run，同锚点。
	session, err := rt.NewSession(context.Background(), runtime.SessionOptions{ID: "sess-share"})
	if err != nil {
		t.Fatalf("NewSession 失败: %v", err)
	}
	if _, err := session.Run(context.Background(), "第二问"); err != nil {
		t.Fatalf("session.Run 失败: %v", err)
	}

	if m.callCount() != 2 {
		t.Fatalf("模型应调用 2 次，got %d", m.callCount())
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	msgs := m.reqs[1].Messages
	joined := ""
	for _, msg := range msgs {
		joined += msg.Content + "\n"
	}
	for _, want := range []string{"第一问", "第一答", "第二问"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("第二次执行的模型请求应含历史 %q，got:\n%s", want, joined)
		}
	}
	if last := msgs[len(msgs)-1]; last.Role != chat.RoleUser || last.Content != "第二问" {
		t.Fatalf("末条应是本次用户输入，got role=%s content=%q", last.Role, last.Content)
	}
}

// TestSessionStatusLifecycle 状态是快照的投影：从未执行 → Exists=false；
// 成功后 → Exists=true + succeeded + 序号推进。不另造状态面。
func TestSessionStatusLifecycle(t *testing.T) {
	rt, err := runtime.NewEmbedded(runtime.Config{
		Model: &scriptedModel{script: []chat.ChatResponse{{Content: "答"}}},
	})
	if err != nil {
		t.Fatalf("装配失败: %v", err)
	}
	defer func() { _ = rt.Close() }()

	session, err := rt.NewSession(context.Background(), runtime.SessionOptions{ID: "sess-st"})
	if err != nil {
		t.Fatalf("NewSession 失败: %v", err)
	}
	st, err := session.Status(context.Background())
	if err != nil {
		t.Fatalf("首读 Status 失败: %v", err)
	}
	if st.Exists || st.Status != "" || st.Seq != 0 {
		t.Fatalf("从未执行的会话应是零值读数，got %+v", st)
	}

	if _, err := session.Run(context.Background(), "问"); err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	st, err = session.Status(context.Background())
	if err != nil {
		t.Fatalf("执行后 Status 失败: %v", err)
	}
	if !st.Exists || st.Status != contract.StatusSucceeded || st.Seq == 0 {
		t.Fatalf("成功后应 Exists=true + succeeded + 序号>0，got %+v", st)
	}
}

// TestSessionApproveSuspendsAndResumes 挂起 → 恢复的统一接缝走会话对象：
// 闸 Suspend ⇒ Run 返回 awaiting；Status 读得出挂起；Approve 批准 ⇒ 续跑到
// 终态且副作用只施加一次。
func TestSessionApproveSuspendsAndResumes(t *testing.T) {
	tl := &tally{}
	gate := func(context.Context, string, chat.ToolCall) (agentloop.Approval, error) {
		return agentloop.ApprovalSuspend, nil
	}
	rt, err := runtime.NewEmbedded(runtime.Config{
		Model: &scriptedModel{script: []chat.ChatResponse{
			{ToolCalls: []chat.ToolCall{{ID: "call-1", Name: "calc", Arguments: "{}"}}},
			{Content: "续跑终答"},
		}},
		Tools: []pipeline.Tool{{
			Name: "calc", Description: "算一下", Safe: true,
			Handler:       tl.handler,
			NeedsApproval: true,
		}},
		ApprovalGate: gate,
	})
	if err != nil {
		t.Fatalf("装配失败: %v", err)
	}
	defer func() { _ = rt.Close() }()

	session, err := rt.NewSession(context.Background(), runtime.SessionOptions{ID: "sess-appr"})
	if err != nil {
		t.Fatalf("NewSession 失败: %v", err)
	}
	res, err := session.Run(context.Background(), "开始")
	if err != nil {
		t.Fatalf("挂起 Run 失败: %v", err)
	}
	if res.Status != agentloop.StatusAwaitingApproval || res.PendingCallID != "call-1" {
		t.Fatalf("应挂起并携带挂起调用，got %+v", res)
	}
	if tl.count() != 0 {
		t.Fatalf("挂起时副作用不得施加，got %d", tl.count())
	}
	st, err := session.Status(context.Background())
	if err != nil {
		t.Fatalf("挂起后 Status 失败: %v", err)
	}
	if !st.Exists || st.Status != agentloop.StatusAwaitingApproval {
		t.Fatalf("挂起态应读得出 waiting_approval，got %+v", st)
	}

	// 挂起态重复 Run 幂等短路（不调模型）。
	res2, err := session.Run(context.Background(), "开始")
	if err != nil {
		t.Fatalf("挂起态重复 Run 失败: %v", err)
	}
	if res2.Status != agentloop.StatusAwaitingApproval || res2.PendingCallID != "call-1" {
		t.Fatalf("挂起态重复 Run 应幂等短路，got %+v", res2)
	}

	// 经会话对象落决策续跑。
	res3, err := session.Approve(context.Background(), "call-1", true)
	if err != nil {
		t.Fatalf("Approve 失败: %v", err)
	}
	if res3.Status != contract.StatusSucceeded || res3.Final != "续跑终答" {
		t.Fatalf("批准续跑应成功，got %+v", res3)
	}
	if tl.count() != 1 {
		t.Fatalf("批准后工具应执行 1 次，got %d", tl.count())
	}
}

// TestRuntimeApproveCompat 兼容入口：Runtime.Approve 薄委托到 Session，
// 不用会话对象的宿主也能走完挂起 → 恢复。
func TestRuntimeApproveCompat(t *testing.T) {
	tl := &tally{}
	rt, err := runtime.NewEmbedded(runtime.Config{
		Model: &scriptedModel{script: []chat.ChatResponse{
			{ToolCalls: []chat.ToolCall{{ID: "call-1", Name: "calc", Arguments: "{}"}}},
			{Content: "终答"},
		}},
		Tools: []pipeline.Tool{{
			Name: "calc", Safe: true, Handler: tl.handler, NeedsApproval: true,
		}},
		ApprovalGate: func(context.Context, string, chat.ToolCall) (agentloop.Approval, error) {
			return agentloop.ApprovalSuspend, nil
		},
	})
	if err != nil {
		t.Fatalf("装配失败: %v", err)
	}
	defer func() { _ = rt.Close() }()

	if _, err := rt.Run(context.Background(), "sess-c", "开始"); err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	res, err := rt.Approve(context.Background(), "sess-c", "call-1", true)
	if err != nil {
		t.Fatalf("Runtime.Approve 失败: %v", err)
	}
	if res.Status != contract.StatusSucceeded {
		t.Fatalf("批准续跑应 succeeded，got %s", res.Status)
	}
	if tl.count() != 1 {
		t.Fatalf("工具应执行 1 次，got %d", tl.count())
	}
}

// TestRuntimeCloseFailsFastAndIdempotent Close 幂等；关闭后会话对象与兼容
// 入口一律 fail-fast——存储已收尾，含糊的驱动层错误不如入口说清。
func TestRuntimeCloseFailsFastAndIdempotent(t *testing.T) {
	rt, err := runtime.NewEmbedded(runtime.Config{Model: &scriptedModel{}})
	if err != nil {
		t.Fatalf("装配失败: %v", err)
	}
	session, err := rt.NewSession(context.Background(), runtime.SessionOptions{ID: "sess-x"})
	if err != nil {
		t.Fatalf("NewSession 失败: %v", err)
	}

	if err := rt.Close(); err != nil {
		t.Fatalf("首次 Close 失败: %v", err)
	}
	if err := rt.Close(); err != nil {
		t.Fatalf("重复 Close 应幂等返回 nil，got %v", err)
	}

	if _, err := rt.NewSession(context.Background(), runtime.SessionOptions{ID: "sess-y"}); err == nil {
		t.Fatal("Close 后 NewSession 应 fail-fast")
	}
	if _, err := rt.Run(context.Background(), "sess-x", "hi"); err == nil {
		t.Fatal("Close 后 Run 应 fail-fast")
	}
	if _, err := session.Run(context.Background(), "hi"); err == nil {
		t.Fatal("Close 后 session.Run 应 fail-fast")
	}
	if _, err := session.Status(context.Background()); err == nil {
		t.Fatal("Close 后 session.Status 应 fail-fast")
	}
	if _, err := session.Approve(context.Background(), "call-1", true); err == nil {
		t.Fatal("Close 后 session.Approve 应 fail-fast")
	}
}
