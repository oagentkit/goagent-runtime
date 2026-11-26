package agentloop_test

import (
	"context"
	"sync"
	"testing"
	"time"

	kerrors "github.com/oekit/pkg/errors"

	"github.com/yi-nology/goagent-kernel/checkpoint"
	"github.com/yi-nology/goagent-kernel/eventbus"

	"github.com/yi-nology/goagent-runtime/agentloop"
	"github.com/yi-nology/goagent-runtime/chat"
	"github.com/yi-nology/goagent-runtime/pipeline"
	"github.com/yi-nology/goagent-runtime/storage"
)

// jointSpy 记录联合提交面收到的事件（提交面污染的可观测面）。
type jointSpy struct {
	mu      sync.Mutex
	commits []jointCommit
}

type jointCommit struct {
	snap   checkpoint.Snapshot
	events []eventbus.Event
}

func (j *jointSpy) CommitWithEvents(_ context.Context, snap checkpoint.Snapshot, evs []eventbus.Event) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.commits = append(j.commits, jointCommit{snap: snap, events: evs})
	return nil
}

func (j *jointSpy) all() []jointCommit {
	j.mu.Lock()
	defer j.mu.Unlock()
	return append([]jointCommit(nil), j.commits...)
}

// concurrentModel 按会话分别计数：每个会话第一轮要工具、第二轮给终答。
type concurrentModel struct {
	mu    sync.Mutex
	calls map[string]int
}

func (m *concurrentModel) Chat(_ context.Context, req chat.ChatRequest) (chat.ChatResponse, error) {
	sess := ""
	for _, msg := range req.Messages {
		if msg.Role == chat.RoleUser {
			sess = msg.Content
		}
	}
	m.mu.Lock()
	if m.calls == nil {
		m.calls = map[string]int{}
	}
	n := m.calls[sess]
	m.calls[sess] = n + 1
	m.mu.Unlock()
	if n == 0 {
		return chat.ChatResponse{ToolCalls: []chat.ToolCall{{ID: "call-" + sess, Name: "calc", Arguments: "{}"}}}, nil
	}
	return chat.ChatResponse{Content: "done-" + sess}, nil
}

// newJointLoop 装配联合提交模式的循环（事件随快照单事务落库）。
func newJointLoop(t *testing.T, m chat.Model, h pipeline.Handler) (*agentloop.Loop, *jointSpy) {
	t.Helper()
	mem := storage.NewMem()
	pipe, err := pipeline.New(mem.Effects(), time.Minute, []pipeline.Tool{{
		Name: "calc", Safe: true, Handler: h,
	}})
	if err != nil {
		t.Fatalf("建管线失败: %v", err)
	}
	spy := &jointSpy{}
	l, err := agentloop.New(agentloop.Config{
		Model: m, Tools: pipe, Store: mem.Checkpoints(), Joint: spy,
	})
	if err != nil {
		t.Fatalf("建循环失败: %v", err)
	}
	return l, spy
}

// TestConcurrentRunsDoNotCrossTalk 同一个 Loop 并发跑两个会话：
// 每个会话的事件只能进自己的提交——**不得跨会话串扰**。
//
// 串扰的形态（挂可变状态在 Loop 上时）：A 的事件被并进 B 的事务、
// 序号互相回退导致提交被判陈旧。判据：每次提交里，事件的 SessionID 必须
// 等于该次提交的快照 RunID。
func TestConcurrentRunsDoNotCrossTalk(t *testing.T) {
	h := func(_ context.Context, _ []byte) (string, error) {
		time.Sleep(5 * time.Millisecond) // 制造交错窗口
		return "ok", nil
	}
	l, spy := newJointLoop(t, &concurrentModel{}, h)

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, sess := range []string{"sess-A", "sess-B"} {
		wg.Add(1)
		go func(i int, sess string) {
			defer wg.Done()
			_, errs[i] = l.Run(context.Background(), sess, sess)
		}(i, sess)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("并发 Run 失败: %v", err)
		}
		_ = i
	}

	for _, c := range spy.all() {
		for _, ev := range c.events {
			if ev.SessionID != c.snap.RunID {
				t.Fatalf("事件串扰：会话 %s 的事件被并进会话 %s 的提交", ev.SessionID, c.snap.RunID)
			}
		}
	}
}

// TestFailedRunEventsDoNotLeakIntoNextRun 一次失败的执行留下的待发事件
// **不得**滞留到下一次执行里（Joint 模式下那批事件会被下一轮连同别的
// 会话一起提交 = 事实被污染）。
//
// 失败形态取「审批闸报错」（fail-closed）：事件已构造（round_start）但
// 本轮没有走到 commit。
func TestFailedRunEventsDoNotLeakIntoNextRun(t *testing.T) {
	mem := storage.NewMem()
	pipe, err := pipeline.New(mem.Effects(), time.Minute, []pipeline.Tool{{
		Name: "danger", Safe: true, NeedsApproval: true,
		Handler: func(context.Context, []byte) (string, error) { return "ok", nil },
	}})
	if err != nil {
		t.Fatalf("建管线失败: %v", err)
	}
	spy := &jointSpy{}
	l, err := agentloop.New(agentloop.Config{
		Model: &fakeModel{errAt: -1, script: []chat.ChatResponse{
			{ToolCalls: []chat.ToolCall{{ID: "call-1", Name: "danger", Arguments: "{}"}}},
			{Content: "终答"},
		}},
		Tools: pipe, Store: mem.Checkpoints(), Joint: spy,
		ApprovalGate: func(context.Context, string, chat.ToolCall) (agentloop.Approval, error) {
			return agentloop.ApprovalApprove, kerrors.New("审批服务不可达")
		},
	})
	if err != nil {
		t.Fatalf("建循环失败: %v", err)
	}

	if _, err := l.Run(context.Background(), "sess-fail", "开始"); err == nil {
		t.Fatal("审批闸失败必须 fail-closed 上抛")
	}

	// 换一个会话再跑：它的提交里不得含上一轮残留的事件。
	_, err = l.Run(context.Background(), "sess-clean", "开始")
	if err != nil {
		t.Fatalf("第二次 Run 失败: %v", err)
	}
	for _, c := range spy.all() {
		if c.snap.RunID != "sess-clean" {
			continue
		}
		for _, ev := range c.events {
			if ev.SessionID != "sess-clean" {
				t.Fatalf("上一轮残留事件泄漏进本轮提交：%s", ev.SessionID)
			}
		}
	}
}

// barrierModel 让前两次模型调用**同时**抵达：两个 Run 都在提交之前就把
// 历史读完了 ⇒ 它们基于同一个快照基线提交（真正的并发写）。
type barrierModel struct {
	mu      sync.Mutex
	arrived int
	release chan struct{}
}

func (m *barrierModel) Chat(_ context.Context, _ chat.ChatRequest) (chat.ChatResponse, error) {
	m.mu.Lock()
	if m.arrived == 0 {
		m.release = make(chan struct{})
	}
	m.arrived++
	n := m.arrived
	release := m.release
	if n == 2 {
		close(release)
	}
	m.mu.Unlock()
	if n <= 2 {
		<-release
	}
	return chat.ChatResponse{Content: "done"}, nil
}

// TestConcurrentRunsSameSessionNoSilentHistoryLoss 同一会话并发 Run：
// 两者都读到了同一份历史基线 ⇒ 提交必然撞序号。此时**必须有一方被明确
// 拒绝**（kernel 的快照序号闸），而不是两份都「成功」——后者意味着后提交
// 的那份用旧历史盖掉了先提交的那份，会话凭空丢掉一轮（静默丢事实）。
func TestConcurrentRunsSameSessionNoSilentHistoryLoss(t *testing.T) {
	mem := storage.NewMem()
	pipe, err := pipeline.New(mem.Effects(), time.Minute, []pipeline.Tool{{
		Name: "calc", Safe: true,
		Handler: func(context.Context, []byte) (string, error) { return "ok", nil },
	}})
	if err != nil {
		t.Fatalf("建管线失败: %v", err)
	}
	l, err := agentloop.New(agentloop.Config{
		Model: &barrierModel{}, Tools: pipe, Store: mem.Checkpoints(),
	})
	if err != nil {
		t.Fatalf("建循环失败: %v", err)
	}

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = l.Run(context.Background(), "sess-same", "输入")
		}(i)
	}
	wg.Wait()

	failed := 0
	for _, err := range errs {
		if err != nil {
			failed++
		}
	}
	if failed == 0 {
		t.Fatal("同一会话并发 Run 必须有一方被明确拒绝（快照序号闸），不得两方都成功")
	}
}
