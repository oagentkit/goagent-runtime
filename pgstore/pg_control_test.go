// pg_control_test.go —— controlplane.Store 的 PostgreSQL 参考实现判据
// （issue #278）。
//
// 判据对应验收标准：
//   - schema 与契约面一致：真 PG 用例覆盖三个 Snapshot* / 三个 Commit*；
//   - 版本锚：快照之后目标被改过 ⇒ 提交必失败（并发只有一个赢家）；
//   - 留痕与状态同事务方向：留痕失败 ⇒ 状态一字不动；缺留痕面 ⇒ 拒绝；
//   - 控制面与任务板同表：CommitTask 后 Board.List 立即看到同一份状态，
//     不存在第二份可写任务状态；
//   - 装配期同源：ControlStore 与 BoardStore 同一 schema ⇒ 来源标识一致；
//     不同 schema ⇒ ErrAuthorityMismatch；
//   - 权威接线：NewAuthoritativeStorePlane + BoardStore 强释后旧令牌被拒。
//
// 跳过与硬失败的边界与 pg_test.go 同口径：场景硬失败；唯一的 skip 在环境
// 入口（GAK_PGREF_DSN 未设置，Skipf 写明原因与起法）。
package pgstore

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/oekit/pkg/errors"

	"github.com/oagentkit/goagent-kernel/controlplane"
	"github.com/oagentkit/goagent-kernel/sideeffect"
	"github.com/oagentkit/goagent-kernel/taskboard"
)

// failingAuditor 留痕失败注入：Append 永远报错，Entries 空。
type failingAuditor struct{}

func (failingAuditor) Append(context.Context, controlplane.AuditEntry) error {
	return errors.New("pgstore-test: 留痕后端不可用")
}

func (failingAuditor) Entries(context.Context, string) ([]controlplane.AuditEntry, error) {
	return nil, nil
}

// freshControlFixture 一份全新 schema + 同一 pool 上的 Board/Effect/Control
// 三份存储（来源标识因此一致）。
func freshControlFixture(t *testing.T) (*ControlStore, *BoardStore, *EffectStore, *pgxpool.Pool) {
	t.Helper()
	dsn := requireDSN(t)
	admin := basePool(t, dsn)
	ctx := context.Background()
	schema, err := NewSchema(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = DropSchema(ctx, admin, schema) })
	if err := EnsureSchema(ctx, admin, schema); err != nil {
		t.Fatal(err)
	}
	pool := schemaPool(t, dsn, schema)
	aud := &controlplane.MemAuditor{}
	return NewControlStore(pool, aud), NewBoardStore(pool), NewEffectStore(pool), pool
}

// seedRun 摆一条运行与两条任务（t1 → t2 依赖）。
func seedRun(t *testing.T, c *ControlStore, run controlplane.RunID) {
	t.Helper()
	err := c.SeedRun(context.Background(), run, "running", 1,
		taskboard.Task{ID: "t1", Title: "first", State: taskboard.StateOpen},
		taskboard.Task{ID: "t2", Title: "second", State: taskboard.StateOpen,
			Deps: []taskboard.TaskID{"t1"}},
	)
	if err != nil {
		t.Fatalf("SeedRun: %v", err)
	}
}

// TestControlStoreSnapshotCommitVersionAnchor 快照 → 旧版本提交被拒 →
// 新版本提交成功；任务状态与 BoardStore 同表可见。
func TestControlStoreSnapshotCommitVersionAnchor(t *testing.T) {
	ctrl, board, _, _ := freshControlFixture(t)
	ctx := context.Background()
	seedRun(t, ctrl, "run-1")

	snap, err := ctrl.SnapshotTask(ctx, "t1")
	if err != nil {
		t.Fatalf("SnapshotTask: %v", err)
	}
	if snap.RunID != "run-1" || snap.Task.ID != "t1" || len(snap.RunTasks) != 2 {
		t.Fatalf("任务快照内容错误: %+v", snap)
	}

	// 旧版本（快照之后假设被改过）⇒ 拒绝且不写。
	next := snap.Task
	next.State = taskboard.StateDone
	next.Seq++
	err = ctrl.CommitTask(ctx, snap.Version+1, next, controlplane.AuditEntry{
		Operator: "op", Action: controlplane.ActionForceRelease, Target: "t1", Basis: "test",
	})
	if !errors.Is(err, controlplane.ErrStoreConflict) {
		t.Fatalf("过期版本提交必须 ErrStoreConflict，实得 %v", err)
	}
	after, err := ctrl.SnapshotTask(ctx, "t1")
	if err != nil {
		t.Fatalf("SnapshotTask after conflict: %v", err)
	}
	if after.Version != snap.Version || after.Task.State != taskboard.StateOpen {
		t.Fatalf("失败的 CAS 不得改变状态/版本：%+v", after)
	}

	// 正确版本 ⇒ 提交成功并推进版本。
	if err := ctrl.CommitTask(ctx, snap.Version, next, controlplane.AuditEntry{
		Operator: "op", Action: controlplane.ActionForceRelease, Target: "t1", Basis: "test",
	}); err != nil {
		t.Fatalf("CommitTask: %v", err)
	}
	got, err := ctrl.SnapshotTask(ctx, "t1")
	if err != nil {
		t.Fatalf("SnapshotTask committed: %v", err)
	}
	if got.Version != snap.Version+1 || got.Task.State != taskboard.StateDone {
		t.Fatalf("提交后版本/状态错误：%+v", got)
	}

	// 同表判据：任务板读到的是同一份状态（不是第二份可写任务状态）。
	tasks, err := board.List(ctx)
	if err != nil {
		t.Fatalf("Board.List: %v", err)
	}
	found := false
	for _, tk := range tasks {
		if tk.ID == "t1" {
			found = true
			if tk.State != taskboard.StateDone || tk.Seq != next.Seq {
				t.Fatalf("控制面提交必须落在任务板的同一张表：%+v", tk)
			}
		}
	}
	if !found {
		t.Fatal("任务板看不到控制面提交的任务")
	}

	// 同源声明：同一 schema 的 ControlStore / BoardStore 来源标识一致。
	if ctrl.TaskSourceID() == "" || ctrl.TaskSourceID() != board.TaskSourceID() {
		t.Fatalf("同一 schema 的来源标识必须一致：ctrl=%q board=%q", ctrl.TaskSourceID(), board.TaskSourceID())
	}
}

// TestControlStoreNeedsAuditor 缺留痕面 ⇒ 提交一律拒绝，状态不变。
func TestControlStoreNeedsAuditor(t *testing.T) {
	_, _, _, pool := freshControlFixture(t)
	ctx := context.Background()
	ctrl := NewControlStore(pool, nil) // 留痕面缺位
	seedRun(t, ctrl, "run-1")

	snap, err := ctrl.SnapshotTask(ctx, "t1")
	if err != nil {
		t.Fatalf("SnapshotTask: %v", err)
	}
	next := snap.Task
	next.State = taskboard.StateDone
	err = ctrl.CommitTask(ctx, snap.Version, next, controlplane.AuditEntry{
		Operator: "op", Action: controlplane.ActionForceRelease, Target: "t1", Basis: "test",
	})
	if !errors.Is(err, controlplane.ErrNoAuditor) {
		t.Fatalf("缺留痕面必须 ErrNoAuditor，实得 %v", err)
	}
	after, err := ctrl.SnapshotTask(ctx, "t1")
	if err != nil {
		t.Fatalf("SnapshotTask after: %v", err)
	}
	if after.Version != snap.Version || after.Task.State != taskboard.StateOpen {
		t.Fatalf("缺留痕面的提交不得改变状态：%+v", after)
	}
}

// TestControlStoreAuditFailureLeavesStateUntouched 留痕失败 ⇒ 状态与版本
// 一字不动（先留痕后落状态的同事务方向）。
func TestControlStoreAuditFailureLeavesStateUntouched(t *testing.T) {
	_, _, _, pool := freshControlFixture(t)
	ctx := context.Background()
	ctrl := NewControlStore(pool, failingAuditor{})
	seedRun(t, ctrl, "run-1")

	snap, err := ctrl.SnapshotTask(ctx, "t1")
	if err != nil {
		t.Fatalf("SnapshotTask: %v", err)
	}
	next := snap.Task
	next.State = taskboard.StateDone
	if err := ctrl.CommitTask(ctx, snap.Version, next, controlplane.AuditEntry{
		Operator: "op", Action: controlplane.ActionForceRelease, Target: "t1", Basis: "test",
	}); err == nil {
		t.Fatal("留痕失败必须让提交失败")
	}
	after, err := ctrl.SnapshotTask(ctx, "t1")
	if err != nil {
		t.Fatalf("SnapshotTask after: %v", err)
	}
	if after.Version != snap.Version || after.Task.State != taskboard.StateOpen {
		t.Fatalf("留痕失败后状态/版本必须不变：%+v", after)
	}
}

// TestControlStoreRunAndEffect SnapshotRun/CommitRun 与效果账的持久化面。
func TestControlStoreRunAndEffect(t *testing.T) {
	ctrl, _, _, _ := freshControlFixture(t)
	ctx := context.Background()
	seedRun(t, ctrl, "run-1")

	rs, err := ctrl.SnapshotRun(ctx, "run-1")
	if err != nil {
		t.Fatalf("SnapshotRun: %v", err)
	}
	if rs.Status != "running" || rs.Gen != 1 || len(rs.Tasks) != 2 {
		t.Fatalf("运行快照错误：%+v", rs)
	}

	// TaskIDs 非 nil ⇒ 同步运行成员（只改任务表 run_id，不另存清单）。
	if err := ctrl.CommitRun(ctx, rs.Version, "run-1", controlplane.RunState{
		Status: "done", Gen: 2, TaskIDs: []taskboard.TaskID{"t2"},
	}, controlplane.AuditEntry{
		Operator: "op", Action: controlplane.ActionTerminate, Target: "run-1", Basis: "test",
	}); err != nil {
		t.Fatalf("CommitRun: %v", err)
	}
	rs2, err := ctrl.SnapshotRun(ctx, "run-1")
	if err != nil {
		t.Fatalf("SnapshotRun after: %v", err)
	}
	if rs2.Status != "done" || rs2.Gen != 2 || len(rs2.Tasks) != 1 || rs2.Tasks[0].ID != "t2" {
		t.Fatalf("运行成员/状态未按 CommitRun 落库：%+v", rs2)
	}

	// 效果账：seed → snapshot → commit。
	if err := ctrl.SeedEffect(ctx, "effect-1", controlplane.EffectState{Uncertain: true, Unsafe: true}); err != nil {
		t.Fatalf("SeedEffect: %v", err)
	}
	es, err := ctrl.SnapshotEffect(ctx, "effect-1")
	if err != nil {
		t.Fatalf("SnapshotEffect: %v", err)
	}
	if !es.Effect.Uncertain || !es.Effect.Unsafe {
		t.Fatalf("效果账快照错误：%+v", es.Effect)
	}
	if err := ctrl.CommitEffect(ctx, es.Version, "effect-1", controlplane.EffectState{
		Uncertain: false, Unsafe: true, Resolved: controlplane.VerdictApplied,
	}, controlplane.AuditEntry{
		Operator: "op", Action: controlplane.ActionAdjudicate, Target: "effect-1", Basis: "test",
	}); err != nil {
		t.Fatalf("CommitEffect: %v", err)
	}
	es2, err := ctrl.SnapshotEffect(ctx, "effect-1")
	if err != nil {
		t.Fatalf("SnapshotEffect after: %v", err)
	}
	if es2.Effect.Uncertain || es2.Effect.Resolved != controlplane.VerdictApplied {
		t.Fatalf("效果账未按 CommitEffect 落库：%+v", es2.Effect)
	}

	// 目标不存在 ⇒ ErrNotFound，且不推进版本。
	before := es2.Version
	if err := ctrl.CommitEffect(ctx, before, "nope", controlplane.EffectState{}, controlplane.AuditEntry{
		Operator: "op", Action: controlplane.ActionAdjudicate, Target: "nope", Basis: "test",
	}); !errors.Is(err, controlplane.ErrNotFound) {
		t.Fatalf("不存在的效果键必须 ErrNotFound，实得 %v", err)
	}
	es3, err := ctrl.SnapshotEffect(ctx, "effect-1")
	if err != nil {
		t.Fatalf("SnapshotEffect final: %v", err)
	}
	if es3.Version != before {
		t.Fatalf("失败的提交不得推进版本：%d → %d", before, es3.Version)
	}
}

// TestControlStoreConcurrentCommitsSingleWinner 两个副本拿同一快照并发
// 提交：恰好一个赢家，另一个 ErrStoreConflict。
func TestControlStoreConcurrentCommitsSingleWinner(t *testing.T) {
	ctrl, _, _, _ := freshControlFixture(t)
	ctx := context.Background()
	seedRun(t, ctrl, "run-1")
	snap, err := ctrl.SnapshotTask(ctx, "t1")
	if err != nil {
		t.Fatalf("SnapshotTask: %v", err)
	}
	next := snap.Task
	next.State = taskboard.StateDone
	entry := controlplane.AuditEntry{
		Operator: "op", Action: controlplane.ActionForceRelease, Target: "t1", Basis: "test",
	}

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	start := make(chan struct{})
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs <- ctrl.CommitTask(ctx, snap.Version, next, entry)
		}()
	}
	close(start)
	wg.Wait()
	close(errs)

	winners, conflicts := 0, 0
	for err := range errs {
		switch {
		case err == nil:
			winners++
		case errors.Is(err, controlplane.ErrStoreConflict):
			conflicts++
		default:
			t.Fatalf("并发提交只应产生赢家或 ErrStoreConflict，实得 %v", err)
		}
	}
	if winners != 1 || conflicts != 1 {
		t.Fatalf("必须恰好一个赢家（winners=%d conflicts=%d）", winners, conflicts)
	}
	after, err := ctrl.SnapshotTask(ctx, "t1")
	if err != nil {
		t.Fatalf("SnapshotTask after: %v", err)
	}
	if after.Version != snap.Version+1 {
		t.Fatalf("版本应只推进一次：%d → %d", snap.Version, after.Version)
	}
}

// TestControlStoreAuthoritativePlaneSameTable 权威装配：ControlStore 与
// BoardStore 同源 ⇒ 装配通过；强释经任务板同一份状态，旧令牌被拒。
func TestControlStoreAuthoritativePlaneSameTable(t *testing.T) {
	ctrl, board, eff, _ := freshControlFixture(t)
	ctx := tenantCtx("acme") // 控制面写路径的租户 fail-closed（#197）
	aud := &controlplane.MemAuditor{}
	plane, err := controlplane.NewAuthoritativeStorePlane(ctrl, aud, board,
		&controlplane.SideEffectAuthority{
			Store: eff,
			ActionOf: func(key string) (sideeffect.Action, bool) {
				return sideeffect.Action{}, false
			},
		})
	if err != nil {
		t.Fatalf("同源装配必须通过：%v", err)
	}

	// 任务板摆任务并认领；强释后旧令牌的完成必须被同一张表拒收。
	board.Seed(taskboard.Task{ID: "t1", State: taskboard.StateOpen})
	claimed, err := board.Claim(ctx, "t1", "worker-a", taskboard.ClaimRequest{TTL: time.Minute})
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := plane.ForceRelease(ctx, controlplane.ForceRelease{
		TaskID: "t1", Operator: "op", Basis: "test",
	}); err != nil {
		t.Fatalf("ForceRelease: %v", err)
	}
	if err := board.Complete(ctx, "t1", claimed.Lease.Token); !errors.Is(err, taskboard.ErrLeaseLost) {
		t.Fatalf("强释后旧令牌完成必须 ErrLeaseLost（同一张表判定），实得 %v", err)
	}
	tasks, err := board.List(ctx)
	if err != nil {
		t.Fatalf("Board.List: %v", err)
	}
	if len(tasks) != 1 || tasks[0].State != taskboard.StateOpen {
		t.Fatalf("强释后任务必须回到 open：%+v", tasks)
	}

	// 不同 schema 的控制面与任务板 ⇒ 装配期 ErrAuthorityMismatch。
	dsn := requireDSN(t)
	admin := basePool(t, dsn)
	otherSchema, err := NewSchema(context.Background(), admin)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = DropSchema(context.Background(), admin, otherSchema) })
	if err := EnsureSchema(context.Background(), admin, otherSchema); err != nil {
		t.Fatal(err)
	}
	otherPool := schemaPool(t, dsn, otherSchema)
	other := NewControlStore(otherPool, aud)
	if _, err := controlplane.NewAuthoritativeStorePlane(other, aud, board, nil); !errors.Is(err, controlplane.ErrAuthorityMismatch) {
		t.Fatalf("不同 schema 的控制面与任务板必须 ErrAuthorityMismatch，实得 %v", err)
	}
}
