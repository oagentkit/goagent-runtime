package pgstore

import (
	"context"
	"testing"
	"time"

	"github.com/oagentkit/goagent-kernel/eventbus"
	"github.com/oagentkit/goagent-kernel/taskboard"
)

// TestFinishRollsBackEventWhenCompletionFails 原子收尾的判据：收尾失败
// （令牌不匹配 ⇒ 任务完成被拒）⇒ **整批回滚**——结果事件不得留在事件面。
//
// 危害形态（分开写时）：事件已对外可见而任务没完成 ⇒ 等待方拿到**过期执行者**
// 的结论（旧 Worker 的租约已被接管，它的结果却已发布）。
func TestFinishRollsBackEventWhenCompletionFails(t *testing.T) {
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
	pool := schemaPool(t, dsn, schema)
	store := NewStore(pool)
	board := store.Board()

	const taskID = taskboard.TaskID("sess-finish")
	board.Seed(taskboard.Task{ID: taskID, State: taskboard.StateOpen})
	claimed, err := board.Claim(ctx, taskID, taskboard.AgentID("agent-1"), taskboard.ClaimRequest{TTL: time.Minute})
	if err != nil {
		t.Fatalf("认领失败: %v", err)
	}

	err = store.Finish(ctx, FinishRequest{
		TaskID: taskID,
		Token:  "tok-not-the-real-one",
		Event: eventbus.Event{
			SessionID:  string(taskID),
			Type:       "session.result",
			Level:      "info",
			Payload:    map[string]any{"result": "{}"},
			OccurredAt: time.Now(),
		},
	})
	if err == nil {
		t.Fatal("令牌不匹配的收尾必须失败（资格不可证）")
	}

	// 事件面不得留下这次收尾的结果（整批回滚）。
	evs, err := store.Events().ListAfter(ctx, 0, 100)
	if err != nil {
		t.Fatalf("读事件面失败: %v", err)
	}
	for _, ev := range evs {
		if ev.SessionID == string(taskID) {
			t.Fatalf("收尾失败后事件仍留在事件面（旧执行者的结果对外可见）: type=%s", ev.Type)
		}
	}

	// 任务不得变成终态（它仍是可恢复的 claimed）。
	tasks, err := board.List(ctx)
	if err != nil {
		t.Fatalf("读任务失败: %v", err)
	}
	for _, tk := range tasks {
		if tk.ID != taskID {
			continue
		}
		if tk.State.Terminal() {
			t.Fatalf("收尾失败后任务却成了终态: %s", tk.State)
		}
		if tk.Lease.Token != claimed.Lease.Token {
			t.Fatal("收尾失败后租约被改动（应原样保留，等接管重跑）")
		}
	}
}
