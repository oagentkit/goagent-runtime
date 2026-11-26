package agentloop_test

import (
	"context"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/oekit/pkg/errors"

	"github.com/yi-nology/goagent-kernel/invocation"

	"github.com/yi-nology/goagent-runtime/agentloop"
	"github.com/yi-nology/goagent-runtime/chat"
	"github.com/yi-nology/goagent-runtime/pipeline"
	"github.com/yi-nology/goagent-runtime/storage"
)

// memJournal 内存账本（测试替身）：原子推进 + 操作轨迹 + 故障注入。
//
// 为什么需要替身而不是直接用 sqliteadapter：这条用例要精确制造「崩在某一
// 步」的形态（推进到某个状态时报错），替身把那一步做成可注入的开关。
type memJournal struct {
	mu     sync.Mutex
	rows   map[string]invocation.Entry
	ops    []string
	failTo invocation.State // 推进到该状态时报错（"" = 不注入）
	seq    uint64
}

func newMemJournal() *memJournal {
	return &memJournal{rows: map[string]invocation.Entry{}}
}

func journalKey(sessionID, callID string) string { return sessionID + "/" + callID }

// Plan 落账（同身份幂等、异身份冲突）。
func (m *memJournal) Plan(_ context.Context, e invocation.Entry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := journalKey(e.SessionID, e.CallID)
	if cur, ok := m.rows[k]; ok {
		if cur.Tool != e.Tool || cur.ArgsDigest != e.ArgsDigest {
			return invocation.ErrIdentityConflict
		}
		m.ops = append(m.ops, "plan:"+e.CallID)
		return nil
	}
	m.seq++
	e.Seq = m.seq
	m.rows[k] = e
	m.ops = append(m.ops, "plan:"+e.CallID)
	return nil
}

func (m *memJournal) Get(_ context.Context, sessionID, callID string) (invocation.Entry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.rows[journalKey(sessionID, callID)]
	if !ok {
		return invocation.Entry{}, invocation.ErrNotFound
	}
	return cur, nil
}

func (m *memJournal) List(_ context.Context, sessionID string) ([]invocation.Entry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []invocation.Entry
	for _, e := range m.rows {
		if e.SessionID == sessionID {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out, nil
}

// Advance 原子推进：读-判-写在一把锁里（判据来自内核的转移表）。
func (m *memJournal) Advance(_ context.Context, sessionID, callID string, to invocation.State, u invocation.Update) (invocation.Entry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := journalKey(sessionID, callID)
	cur, ok := m.rows[k]
	if !ok {
		return invocation.Entry{}, invocation.ErrNotFound
	}
	if m.failTo != "" && to == m.failTo {
		m.ops = append(m.ops, "advance-fail:"+string(to))
		return invocation.Entry{}, errors.New("注入故障：账目推进失败")
	}
	if cur.State == to {
		return cur, nil // 幂等：推进到当前状态 = 无操作
	}
	if !invocation.CanTransition(cur.State, to) {
		return invocation.Entry{}, invocation.ErrIllegalTransition
	}
	cur.State = to
	if len(u.Result) > 0 {
		cur.Result = u.Result
	}
	if u.Reason != "" {
		cur.Reason = u.Reason
	}
	m.rows[k] = cur
	m.ops = append(m.ops, "advance:"+string(to))
	return cur, nil
}

// forget 抹掉一条账目（模拟「账本没记着这次调用」——外部篡改的形态）。
func (m *memJournal) forget(sessionID, callID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.rows, journalKey(sessionID, callID))
}

func (m *memJournal) trace() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.ops...)
}

// journalLoop 装一个带账本的循环（工具执行体把轨迹记进 trace）。
func journalLoop(t *testing.T, mem *storage.Mem, j *memJournal, m chat.Model, trace *[]string) *agentloop.Loop {
	t.Helper()
	pipe, err := pipeline.New(mem.Effects(), time.Minute, []pipeline.Tool{{
		Name: "calc", Safe: true,
		Handler: func(_ context.Context, _ []byte) (string, error) {
			*trace = append(*trace, "exec")
			return "ok-42", nil
		},
	}})
	if err != nil {
		t.Fatalf("建管线失败: %v", err)
	}
	l, err := agentloop.New(agentloop.Config{
		Model: m, Tools: pipe, Store: mem.Checkpoints(), Journal: j,
	})
	if err != nil {
		t.Fatalf("建循环失败: %v", err)
	}
	return l
}

// TestJournalResumeAfterCrash 崩溃后按账本续跑：同一个 callID，副作用只施加一次。
//
// 这条是 P0-09/P0-10 的闭合判据：崩溃发生在「副作用已施加、结果还没回填」
// 那一段 ⇒ 恢复时**不得**重新生成一次调用（否则幂等键变 ⇒ 同一业务动作再
// 施加一遍），也**不得**重复施加（那要靠幂等闸，不是靠模型不重复请求）。
func TestJournalResumeAfterCrash(t *testing.T) {
	ctx := context.Background()
	mem := storage.NewMem()
	j := newMemJournal()
	var trace []string
	// 崩在「执行完、推进 applied」那一刻：副作用已施加，账目停在 executing。
	j.failTo = invocation.StateApplied
	l := journalLoop(t, mem, j, &fakeModel{errAt: -1, script: []chat.ChatResponse{{
		Content:   "我要算一下",
		ToolCalls: []chat.ToolCall{{ID: "call-fixed", Name: "calc", Arguments: `{"k":1}`}},
	}}}, &trace)

	if _, err := l.Run(ctx, "sess-1", "算一下"); err == nil {
		t.Fatal("注入的故障应让本轮失败（模拟崩溃）")
	}
	if got := len(trace); got != 1 || trace[0] != "exec" {
		t.Fatalf("副作用应已施加一次，轨迹=%v", trace)
	}
	e, err := j.Get(ctx, "sess-1", "call-fixed")
	if err != nil {
		t.Fatalf("读账目失败: %v", err)
	}
	if e.State != invocation.StateExecuting {
		t.Fatalf("崩溃后账目应停在 executing，got %s", e.State)
	}

	// 进程重启（新循环，同一批存储与账本）：存储恢复 ⇒ 按账本续跑。
	j.failTo = ""
	m2 := &fakeModel{errAt: -1, script: []chat.ChatResponse{{Content: "算完了"}}}
	l2 := journalLoop(t, mem, j, m2, &trace)
	res, err := l2.Run(ctx, "sess-1", "继续")
	if err != nil {
		t.Fatalf("恢复失败: %v", err)
	}
	// ★ 副作用**只施加一次**：恢复走幂等闸重放，不重新施加。
	if got := len(trace); got != 1 {
		t.Fatalf("恢复不得重复施加副作用，轨迹=%v", trace)
	}
	// 模型被问的时候，历史里那次调用已经有结果了——模型不需要重新生成它。
	answered := false
	for _, msg := range m2.lastRequest().Messages {
		if msg.Role == chat.RoleTool && msg.ToolCallID == "call-fixed" {
			answered = true
		}
	}
	if !answered {
		t.Fatal("恢复后模型看到的历史里没有那次调用的结果（模型会被要求重新生成）")
	}
	if res.Status == "" {
		t.Fatal("恢复结果缺状态")
	}
	// 账目收口：applied → result_recorded（终态）。
	e, err = j.Get(ctx, "sess-1", "call-fixed")
	if err != nil {
		t.Fatalf("读账目失败: %v", err)
	}
	if e.State != invocation.StateResultRecorded {
		t.Fatalf("恢复后账目应收口到 result_recorded，got %s", e.State)
	}
	if string(e.Result) != "ok-42" {
		t.Fatalf("账上应留有结果，got %q", e.Result)
	}
}

// TestJournalRejectsUnbackedUnansweredCall 历史里未答复、账本里没有 ⇒ fail-closed。
//
// 判据：**合法未完成调用**由账本背书（账本说它还没收口）；账本里根本没有它 ⇒
// 那是外部篡改（历史里凭空多了一次调用）⇒ 拒绝恢复，不猜。
func TestJournalRejectsUnbackedUnansweredCall(t *testing.T) {
	ctx := context.Background()
	mem := storage.NewMem()
	j := newMemJournal()
	var trace []string
	j.failTo = invocation.StateApplied
	l := journalLoop(t, mem, j, &fakeModel{errAt: -1, script: []chat.ChatResponse{{
		Content:   "我要算一下",
		ToolCalls: []chat.ToolCall{{ID: "call-fixed", Name: "calc", Arguments: `{"k":1}`}},
	}}}, &trace)
	if _, err := l.Run(ctx, "sess-1", "算一下"); err == nil {
		t.Fatal("注入的故障应让本轮失败（模拟崩溃）")
	}
	// 账本里那条没了（外部篡改/账本丢失）⇒ 恢复必须拒绝。
	j.forget("sess-1", "call-fixed")
	j.failTo = ""
	l2 := journalLoop(t, mem, j, &fakeModel{errAt: -1, script: []chat.ChatResponse{{Content: "算完了"}}}, &trace)
	if _, err := l2.Run(ctx, "sess-1", "继续"); err == nil {
		t.Fatal("账本背书不了的未答复调用应判红（历史被篡改），却恢复了")
	}
}

// TestJournalPlansBeforeExecution 落账在**执行之前**（身份先落账）。
//
// 顺序有讲究：身份（callID + 工具名 + 参数）必须先落在账上，恢复才谈得上
// 「按账本续跑」；反了就变成「执行完了才知道这次调用是谁」。
func TestJournalPlansBeforeExecution(t *testing.T) {
	ctx := context.Background()
	mem := storage.NewMem()
	j := newMemJournal()
	var trace []string
	l := journalLoop(t, mem, j, &fakeModel{errAt: -1, script: []chat.ChatResponse{{
		Content:   "我要算一下",
		ToolCalls: []chat.ToolCall{{ID: "call-fixed", Name: "calc", Arguments: `{"k":1}`}},
	}}}, &trace)
	if _, err := l.Run(ctx, "sess-1", "算一下"); err != nil {
		t.Fatalf("本轮应成功: %v", err)
	}
	ops := j.trace()
	planAt, execAt := -1, -1
	for i, op := range ops {
		if op == "plan:call-fixed" && planAt < 0 {
			planAt = i
		}
		if op == "advance:executing" && execAt < 0 {
			execAt = i
		}
	}
	if planAt < 0 || execAt < 0 {
		t.Fatalf("账目轨迹缺关键步骤: %v", ops)
	}
	if planAt > execAt {
		t.Fatalf("落账必须早于执行（推进到 executing），轨迹=%v", ops)
	}
	// 终态：账目收口到 result_recorded。
	e, err := j.Get(ctx, "sess-1", "call-fixed")
	if err != nil {
		t.Fatalf("读账目失败: %v", err)
	}
	if e.State != invocation.StateResultRecorded {
		t.Fatalf("账目应收口到 result_recorded，got %s", e.State)
	}
}
