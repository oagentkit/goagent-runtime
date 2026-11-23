// 联合提交面契约测试（issue #9——#2 主体的 PG 形态）。无 GAK_PGREF_DSN
// 显式 skip（不伪造结果，与套件纪律一致）。
//
// 判据对齐 sqliteadapter 联合面测试（storage/joint_test.go）：
//   - 端到端：快照+事件一批落库，事件行与快照行同读可见；
//   - 原子性：快照 CAS 拒收 ⇒ 整批回滚 ⇒ 事件行零落。
package pgstore

import (
	"context"
	"testing"
	"time"

	"github.com/yi-nology/goagent-kernel/checkpoint"
	"github.com/yi-nology/goagent-kernel/eventbus"

)

// TestCommitWithEventsEndToEnd 快照+事件一批落库、同读可见。
func TestCommitWithEventsEndToEnd(t *testing.T) {
	dsn := requireDSN(t)
	ctx := context.Background()
	admin := basePool(t, dsn)
	schema, err := NewSchema(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = DropSchema(ctx, admin, schema) })
	if err := EnsureSchema(ctx, admin, schema); err != nil {
		t.Fatal(err)
	}
	store := NewStore(schemaPool(t, dsn, schema))

	snap := checkpoint.Snapshot{
		RunID:  "sess-joint",
		Seq:    1,
		Status: "running",
		Fields: map[string]string{"history": "[]"},
	}
	events := []eventbus.Event{
		{SessionID: "sess-joint", Type: "agent.round_start", Level: "info", OccurredAt: time.Now()},
		{SessionID: "sess-joint", Type: "agent.tool_call", Level: "info", OccurredAt: time.Now()},
	}
	if err := store.CommitWithEvents(ctx, snap, events); err != nil {
		t.Fatalf("联合提交失败: %v", err)
	}

	gotSnap, err := store.Snapshots().Load(ctx, "sess-joint")
	if err != nil {
		t.Fatalf("读快照失败: %v", err)
	}
	if gotSnap.Seq != 1 {
		t.Fatalf("快照 seq 应 1，got %d", gotSnap.Seq)
	}
	events, err = store.Events().ListAfter(ctx, 0, 10)
	if err != nil {
		t.Fatalf("读事件失败: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("应落 2 事件行，got %d", len(events))
	}
}

// TestCommitWithEventsAtomicOnFailure 原子性直证：先落 seq=2 基线快照，
// 再提交 seq=1 陈旧快照+一条事件 ⇒ CAS 拒收 ⇒ 整批回滚 ⇒ 事件行零落。
func TestCommitWithEventsAtomicOnFailure(t *testing.T) {
	dsn := requireDSN(t)
	ctx := context.Background()
	admin := basePool(t, dsn)
	schema, err := NewSchema(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = DropSchema(ctx, admin, schema) })
	if err := EnsureSchema(ctx, admin, schema); err != nil {
		t.Fatal(err)
	}
	store := NewStore(schemaPool(t, dsn, schema))

	// 高序基线（seq=2）。
	if err := store.CommitWithEvents(ctx, checkpoint.Snapshot{
		RunID:  "sess-atomic",
		Seq:    2,
		Status: "succeeded",
		Fields: map[string]string{"history": "[]"},
	}, nil); err != nil {
		t.Fatalf("基线快照落库失败: %v", err)
	}

	// 陈旧快照（seq=1）+ 一条事件 ⇒ 整批必须作废。
	err = store.CommitWithEvents(ctx, checkpoint.Snapshot{
		RunID:  "sess-atomic",
		Seq:    1,
		Status: "running",
		Fields: map[string]string{"history": "[]"},
	}, []eventbus.Event{
		{SessionID: "sess-atomic", Type: "x", Level: "info", OccurredAt: time.Now()},
	})
	if err == nil {
		t.Fatal("陈旧快照必须被 CAS 拒收")
	}
	events, err := store.Events().ListAfter(ctx, 0, 10)
	if err != nil {
		t.Fatalf("读事件失败: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("联合提交失败后事件行必须零落，got %d", len(events))
	}
}
