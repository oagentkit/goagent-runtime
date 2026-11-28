package runtime_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/yi-nology/goagent-kernel/invocation"

	"github.com/yi-nology/goagent-runtime/chat"
	"github.com/yi-nology/goagent-runtime/pipeline"
	"github.com/yi-nology/goagent-runtime/runtime"
)

// Runtime 级接缝测试：Config.Journal / SessionOptions.Reconcile 必须传到 Loop。
// 判据（恢复按账本续跑、裁决只认 applied/executing）归 agentloop 的既有用例，不另写。
//
// memJournal 测试用账本：Plan 同身份幂等；Advance 直接置态（转移合法性判据
// 归 kernel 参考实现与 sidetest，这里不需要第二份）。

type memJournal struct {
	mu      sync.Mutex
	entries map[string]invocation.Entry // key = sessionID#callID
}

func newMemJournal() *memJournal {
	return &memJournal{entries: map[string]invocation.Entry{}}
}

func (j *memJournal) Plan(_ context.Context, e invocation.Entry) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	key := e.SessionID + "#" + e.CallID
	if cur, ok := j.entries[key]; ok {
		if cur.Tool != e.Tool || cur.ArgsDigest != e.ArgsDigest {
			return invocation.ErrIdentityConflict
		}
		return nil // 同身份重放：幂等无操作
	}
	e.Seq = uint64(len(j.entries) + 1)
	j.entries[key] = e
	return nil
}

func (j *memJournal) Get(_ context.Context, sessionID, callID string) (invocation.Entry, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	e, ok := j.entries[sessionID+"#"+callID]
	if !ok {
		return invocation.Entry{}, invocation.ErrNotFound
	}
	return e, nil
}

func (j *memJournal) List(_ context.Context, sessionID string) ([]invocation.Entry, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	var out []invocation.Entry
	for _, e := range j.entries {
		if e.SessionID == sessionID {
			out = append(out, e)
		}
	}
	return out, nil
}

func (j *memJournal) Advance(_ context.Context, sessionID, callID string, to invocation.State, u invocation.Update) (invocation.Entry, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	key := sessionID + "#" + callID
	e, ok := j.entries[key]
	if !ok {
		return invocation.Entry{}, invocation.ErrNotFound
	}
	e.State = to
	if u.Result != nil {
		e.Result = u.Result
	}
	if u.Reason != "" {
		e.Reason = u.Reason
	}
	j.entries[key] = e
	return e, nil
}

// TestSessionJournalPassThrough Config.Journal 必须传到 Loop：模型请求一次工具，
// 账本应出现一条 planned→applied→result_recorded 的账目（装配层漏传 = 账本空）。
func TestSessionJournalPassThrough(t *testing.T) {
	j := newMemJournal()
	m := &recordingModel{script: []chat.ChatResponse{
		{ToolCalls: []chat.ToolCall{{ID: "call-1", Name: "echo", Arguments: "{}"}}},
		{Content: "done"},
	}}
	rt, err := runtime.NewEmbedded(runtime.Config{
		Model:   m,
		Tools:   []pipeline.Tool{countingToolNamed("echo", new(int64), &sync.Mutex{})},
		Journal: j,
	})
	if err != nil {
		t.Fatalf("装配失败: %v", err)
	}
	defer func() { _ = rt.Close() }()

	s, err := rt.NewSession(context.Background(), runtime.SessionOptions{ID: "sess-j"})
	if err != nil {
		t.Fatalf("NewSession 失败: %v", err)
	}
	res, err := s.Run(context.Background(), "请调用 echo")
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	if res.Status != "succeeded" {
		t.Fatalf("状态应 succeeded, got %q", res.Status)
	}
	entries, err := j.List(context.Background(), "sess-j")
	if err != nil {
		t.Fatalf("List 失败: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("账本应恰一条, got %d", len(entries))
	}
	if entries[0].Tool != "echo" || entries[0].State != invocation.StateResultRecorded {
		t.Fatalf("账目应 echo/result_recorded, got tool=%s state=%s", entries[0].Tool, entries[0].State)
	}
}

// TestSessionReconcileOverride SessionOptions.Reconcile 必须传到 Loop：预埋
// executing 账目 + 裁决钩子判「已生效」，恢复续跑应回放结果而不重执行工具。
func TestSessionReconcileOverride(t *testing.T) {
	j := newMemJournal()
	args := []byte("{}")
	_ = j.Plan(context.Background(), invocation.Entry{
		SessionID: "sess-r", CallID: "call-r", Tool: "echo",
		ArgsDigest: invocation.Digest(args), Args: args,
		State: invocation.StateUncertain,
	})
	var reconciled int32
	reconcile := func(_ context.Context, _ string, e invocation.Entry) (invocation.State, []byte, error) {
		atomic.AddInt32(&reconciled, 1)
		return invocation.StateApplied, []byte("replayed"), nil
	}
	var exec int64
	var execMu sync.Mutex
	m := &recordingModel{script: []chat.ChatResponse{{Content: "final"}}}
	rt, err := runtime.NewEmbedded(runtime.Config{
		Model:   m,
		Tools:   []pipeline.Tool{countingToolNamed("echo", &exec, &execMu)},
		Journal: j,
	})
	if err != nil {
		t.Fatalf("装配失败: %v", err)
	}
	defer func() { _ = rt.Close() }()

	s, err := rt.NewSession(context.Background(), runtime.SessionOptions{ID: "sess-r", Reconcile: reconcile})
	if err != nil {
		t.Fatalf("NewSession 失败: %v", err)
	}
	res, err := s.Run(context.Background(), "继续")
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	if res.Status != "succeeded" {
		t.Fatalf("状态应 succeeded, got %q", res.Status)
	}
	if atomic.LoadInt32(&reconciled) != 1 {
		t.Fatalf("裁决钩子应恰调 1 次, got %d", reconciled)
	}
	if loadInt64(&exec, &execMu) != 0 {
		t.Fatalf("工具不得重执行, got %d", exec)
	}
}
