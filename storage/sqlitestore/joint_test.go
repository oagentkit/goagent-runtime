// Joint 模式端到端与原子性直证（runtime issue #2 主体；#9 的 SQLite 形态）。
//
// 依赖方向：agentloop 不 import 具体存储（红线），故集成测试落**绑定包**
// ——storage/sqlitestore 是 SQLite 形态的装配邻域，可同时 import agentloop
// （装配）与 sqliteadapter（联合提交面实现）。
//
// 另一条同形判据（#10 拆分带出的缺口）：这里用的是 **sqlitestore.Open** 出来的
// Provider（不是裸 sqliteadapter）⇒ 顺带钉住「*sqlitestore.SQLite 直接实现
// CommitWithEvents」——只给 AsJoint 转发器时 runtime 装配的能力断言恒 false，
// 会静默落回两步提交路径（批662 缺口）。
package sqlitestore_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/yi-nology/goagent-kernel/checkpoint"
	"github.com/yi-nology/goagent-kernel/contract"
	"github.com/yi-nology/goagent-kernel/eventbus"

	"github.com/yi-nology/goagent-runtime/agentloop"
	"github.com/yi-nology/goagent-runtime/chat"
	"github.com/yi-nology/goagent-runtime/pipeline"
	"github.com/yi-nology/goagent-runtime/storage/sqlitestore"
)

// jointFakeModel 脚本化模型（与 agentloop 测试同款判据，包内独立最小形态）。
type jointFakeModel struct {
	calls  int
	script []chat.ChatResponse
}

func (f *jointFakeModel) Chat(_ context.Context, _ chat.ChatRequest) (chat.ChatResponse, error) {
	i := f.calls
	f.calls++
	if i < len(f.script) {
		return f.script[i], nil
	}
	return chat.ChatResponse{Content: "fallback-final"}, nil
}

// TestJointCommitEndToEnd Joint 装配跑一轮：事件行与快照行同库同落——
// 一轮四事件（两轮 round_start + tool_call + session_state）全部持久化，
// 且提交后 Bus 缺省时无广播面也不丢数据。
func TestJointCommitEndToEnd(t *testing.T) {
	ctx := context.Background()
	sq, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "joint-e2e.db"))
	if err != nil {
		t.Fatalf("开库失败: %v", err)
	}
	defer func() { _ = sq.Close() }()

	calls := 0
	pipe, err := pipeline.New(sq.Effects(), time.Minute, []pipeline.Tool{
		{Name: "calc", Safe: true, Handler: func(_ context.Context, _ []byte) (string, error) {
			calls++
			return "ok", nil
		}},
	})
	if err != nil {
		t.Fatalf("建管线失败: %v", err)
	}
	m := &jointFakeModel{script: []chat.ChatResponse{
		{ToolCalls: []chat.ToolCall{{ID: "call-1", Name: "calc", Arguments: "{}"}}},
		{Content: "终答"},
	}}
	loop, err := agentloop.New(agentloop.Config{
		Model: m, Tools: pipe, Store: sq.Checkpoints(),
		Joint: sq,
	})
	if err != nil {
		t.Fatalf("建循环失败: %v", err)
	}

	res, err := loop.Run(ctx, "sess-joint", "开始")
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	if res.Status != contract.StatusSucceeded {
		t.Fatalf("应 succeeded，got %s", res.Status)
	}
	if calls != 1 {
		t.Fatalf("工具应执行 1 次，got %d", calls)
	}

	events := listEvents(t, sq.Events())
	if len(events) != 5 {
		t.Fatalf("一轮应持久化 5 事件（2 round_start+tool_call+tool_result+session_state），got %d", len(events))
	}
	if events[len(events)-1].Type != agentloop.EventSessionState {
		t.Fatalf("末事件应为 session_state，got %s", events[len(events)-1].Type)
	}
}

// TestJointCommitAtomicOnFailure 原子性直证：联合提交失败（快照 CAS 拒收
// 陈旧快照）⇒ 整批回滚 ⇒ 事件行一个都不落——同生共死的另一半。
func TestJointCommitAtomicOnFailure(t *testing.T) {
	ctx := context.Background()
	sq, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "joint-atomic.db"))
	if err != nil {
		t.Fatalf("开库失败: %v", err)
	}
	defer func() { _ = sq.Close() }()

	pendings := []eventbus.Event{
		{SessionID: "sess-atomic", Type: "x", Level: "info", OccurredAt: time.Now()},
	}
	// 先落高序快照建基线（seq=2），再试 seq=1 的陈旧快照——CAS 拒收语义。
	if err := sq.CommitWithEvents(ctx, snapshotAtSeq(2, "sess-atomic"), nil); err != nil {
		t.Fatalf("基线快照落库失败: %v", err)
	}
	if err := sq.CommitWithEvents(ctx, snapshotAtSeq(1, "sess-atomic"), pendings); err == nil {
		t.Fatal("陈旧快照必须被 CAS 拒收")
	}
	events := listEvents(t, sq.Events())
	if len(events) != 0 {
		t.Fatalf("联合提交失败后事件行必须零落，got %d", len(events))
	}
}

// TestJointCommitAtomicOnEventFailure 反向原子性（#9 判据②的另一半）：
// 事件面写失败 ⇒ 快照行**不落**（无孤儿快照）——「同生共死」的另一半。
//
// 注入手段：payload 里放一个不可序列化的值（函数）⇒ 事件序列化必失败。
func TestJointCommitAtomicOnEventFailure(t *testing.T) {
	ctx := context.Background()
	sq, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "joint-rev.db"))
	if err != nil {
		t.Fatalf("开库失败: %v", err)
	}
	defer func() { _ = sq.Close() }()

	bad := eventbus.Event{
		SessionID: "sess-rev", Type: "x", Level: "info",
		Payload:    map[string]any{"bad": func() {}}, // 不可序列化 ⇒ 写事件必失败
		OccurredAt: time.Now(),
	}
	if err := sq.CommitWithEvents(ctx, snapshotAtSeq(1, "sess-rev"), []eventbus.Event{bad}); err == nil {
		t.Fatal("事件面写失败时联合提交必须报错")
	}

	// 快照行不落（无孤儿快照）。
	if snap, err := sq.Checkpoints().Load(ctx, "sess-rev"); err == nil {
		t.Fatalf("事件面失败后快照行必须不落，got seq=%d", snap.Seq)
	}
	if evs := listEvents(t, sq.Events()); len(evs) != 0 {
		t.Fatalf("事件行也必须零落，got %d", len(evs))
	}
}

// eventLister 事件列举面。kernel 的 eventbus.Store 契约**只保证 Insert**——
// 列举是参考实现面的能力（两脸同库的形态才谈得上「这批事件落了几行」）。
// 故按能力断言取用，不扩 kernel 契约。
type eventLister interface {
	ListAfter(ctx context.Context, cursor uint64, limit int) ([]eventbus.Event, error)
}

// listEvents 列举事件面；能力不成立即判据失效（fail loud，不静默当作零行）。
func listEvents(t *testing.T, s eventbus.Store) []eventbus.Event {
	t.Helper()
	l, ok := s.(eventLister)
	if !ok {
		t.Fatal("事件面必须支持列举（ListAfter）——能力不成立即判据失效")
	}
	evs, err := l.ListAfter(context.Background(), 0, 10)
	if err != nil {
		t.Fatalf("读事件失败: %v", err)
	}
	return evs
}

// snapshotAtSeq 构造指定 seq 的快照（CAS 用例）。
func snapshotAtSeq(seq uint64, runID string) checkpoint.Snapshot {
	return checkpoint.Snapshot{
		RunID:  runID,
		Seq:    seq,
		Status: "succeeded",
		Fields: map[string]string{"history": "[]"},
	}
}
