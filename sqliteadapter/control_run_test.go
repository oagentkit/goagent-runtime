// control_run_test.go —— 控制面的运行面 / 效果面 / 并发与一致快照判据。
//
// 与 control_test.go 同批落地（单文件 400 行阈值 ⇒ 拆两个文件）。
package sqliteadapter

import (
	"sync"
	"testing"
	"time"

	"github.com/oekit/pkg/errors"

	"github.com/oagentkit/goagent-kernel/controlplane"
	"github.com/oagentkit/goagent-kernel/taskboard"
)

// TestControlStoreRunAndEffect 运行面与效果裁决账的持久化面。
//
// 判据逐条对位 pgstore 的同名用例：TaskIDs 非 nil ⇒ 同步运行成员（只改任务表
// 的 run_id，不另存清单）；目标不存在 ⇒ ErrNotFound 且**不推进版本**。
func TestControlStoreRunAndEffect(t *testing.T) {
	ctx := ctrlCtx(t)
	s := openStore(t)
	c := s.Control(&controlplane.MemAuditor{})

	if err := c.SeedRun(ctx, "run-1", "running", 1,
		taskboard.Task{ID: "t1", Title: "first", State: taskboard.StateOpen},
		taskboard.Task{ID: "t2", Title: "second", State: taskboard.StateOpen,
			Deps: []taskboard.TaskID{"t1"}},
	); err != nil {
		t.Fatalf("SeedRun: %v", err)
	}

	// 任务快照带上所属运行的全集（run 级判据要看全集，不能只取一个任务）。
	tsnap, err := c.SnapshotTask(ctx, "t1")
	if err != nil {
		t.Fatalf("SnapshotTask: %v", err)
	}
	if tsnap.RunID != "run-1" || len(tsnap.RunTasks) != 2 {
		t.Fatalf("任务快照的运行全集错误：%+v", tsnap)
	}

	rs, err := c.SnapshotRun(ctx, "run-1")
	if err != nil {
		t.Fatalf("SnapshotRun: %v", err)
	}
	if rs.Status != "running" || rs.Gen != 1 || len(rs.Tasks) != 2 {
		t.Fatalf("运行快照错误：%+v", rs)
	}

	// TaskIDs 非 nil ⇒ 同步运行成员。
	if err := c.CommitRun(ctx, rs.Version, "run-1", controlplane.RunState{
		Status: "done", Gen: 2, TaskIDs: []taskboard.TaskID{"t2"},
	}, auditEntry(controlplane.ActionTerminate, "run-1")); err != nil {
		t.Fatalf("CommitRun: %v", err)
	}
	rs2, err := c.SnapshotRun(ctx, "run-1")
	if err != nil {
		t.Fatalf("SnapshotRun after: %v", err)
	}
	if rs2.Status != "done" || rs2.Gen != 2 || len(rs2.Tasks) != 1 || rs2.Tasks[0].ID != "t2" {
		t.Fatalf("运行成员/状态未按 CommitRun 落库：%+v", rs2)
	}

	// TaskIDs 为空集 ⇒ 运行成员被清空（与 PG 侧同款语义）。
	if err := c.CommitRun(ctx, rs2.Version, "run-1", controlplane.RunState{
		Status: "done", Gen: 3, TaskIDs: []taskboard.TaskID{},
	}, auditEntry(controlplane.ActionTerminate, "run-1")); err != nil {
		t.Fatalf("CommitRun 清空成员: %v", err)
	}
	rs3, err := c.SnapshotRun(ctx, "run-1")
	if err != nil {
		t.Fatalf("SnapshotRun cleared: %v", err)
	}
	if len(rs3.Tasks) != 0 {
		t.Fatalf("空集合应清空运行成员，实得 %+v", rs3.Tasks)
	}

	// 效果账：seed → snapshot → commit。
	if err := c.SeedEffect(ctx, "effect-1",
		controlplane.EffectState{Uncertain: true, Unsafe: true}); err != nil {
		t.Fatalf("SeedEffect: %v", err)
	}
	es, err := c.SnapshotEffect(ctx, "effect-1")
	if err != nil {
		t.Fatalf("SnapshotEffect: %v", err)
	}
	if !es.Effect.Uncertain || !es.Effect.Unsafe {
		t.Fatalf("效果账快照错误：%+v", es.Effect)
	}
	if err := c.CommitEffect(ctx, es.Version, "effect-1", controlplane.EffectState{
		Uncertain: false, Unsafe: true, Resolved: controlplane.VerdictApplied,
	}, auditEntry(controlplane.ActionAdjudicate, "effect-1")); err != nil {
		t.Fatalf("CommitEffect: %v", err)
	}
	es2, err := c.SnapshotEffect(ctx, "effect-1")
	if err != nil {
		t.Fatalf("SnapshotEffect after: %v", err)
	}
	if es2.Effect.Uncertain || es2.Effect.Resolved != controlplane.VerdictApplied {
		t.Fatalf("效果账未按 CommitEffect 落库：%+v", es2.Effect)
	}

	// 目标不存在 ⇒ ErrNotFound，且不推进版本。
	before := es2.Version
	if err := c.CommitEffect(ctx, before, "nope", controlplane.EffectState{},
		auditEntry(controlplane.ActionAdjudicate, "nope")); !errors.Is(err, controlplane.ErrNotFound) {
		t.Fatalf("不存在的效果键必须 ErrNotFound，实得 %v", err)
	}
	es3, err := c.SnapshotEffect(ctx, "effect-1")
	if err != nil {
		t.Fatalf("SnapshotEffect final: %v", err)
	}
	if es3.Version != before {
		t.Fatalf("失败的提交不得推进版本：%d → %d", before, es3.Version)
	}
}

// TestControlStoreSharesTaskTableWithBoard 控制面与任务板**同一张可写表**，
// 且来源标识一致（装配期同源判据）。
func TestControlStoreSharesTaskTableWithBoard(t *testing.T) {
	ctx := ctrlCtx(t)
	s := openStore(t)
	s.Board().Seed(taskboard.Task{ID: "t1", State: taskboard.StateOpen})
	c := s.Control(&controlplane.MemAuditor{})

	snap, err := c.SnapshotTask(ctx, "t1")
	if err != nil {
		t.Fatalf("SnapshotTask: %v", err)
	}
	next := snap.Task
	next.State = taskboard.StateDone
	next.Seq++
	if err := c.CommitTask(ctx, snap.Version, next,
		auditEntry(controlplane.ActionForceRelease, "t1")); err != nil {
		t.Fatalf("CommitTask: %v", err)
	}

	// 同表判据：任务板读到的是同一份状态（不是第二份可写任务状态）。
	tasks, err := s.Board().List(ctx)
	if err != nil {
		t.Fatalf("Board.List: %v", err)
	}
	if len(tasks) != 1 || tasks[0].State != taskboard.StateDone || tasks[0].Seq != next.Seq {
		t.Fatalf("控制面提交必须落在任务板的同一张表：%+v", tasks)
	}

	// 同源声明：同一数据文件的控制面 / 任务板来源标识一致。
	if c.TaskSourceID() == "" || c.TaskSourceID() != s.Board().TaskSourceID() {
		t.Fatalf("同一数据文件的来源标识必须一致：ctrl=%q board=%q",
			c.TaskSourceID(), s.Board().TaskSourceID())
	}
}

// TestControlStoreConcurrentCommitsSingleWinner 两个副本拿同一快照并发提交：
// 恰好一个赢家，另一个 ErrStoreConflict，版本只推进一次。
func TestControlStoreConcurrentCommitsSingleWinner(t *testing.T) {
	ctx := ctrlCtx(t)
	s := openStore(t)
	s.Board().Seed(taskboard.Task{ID: "t1", State: taskboard.StateOpen})
	c := s.Control(&controlplane.MemAuditor{})

	snap, err := c.SnapshotTask(ctx, "t1")
	if err != nil {
		t.Fatalf("SnapshotTask: %v", err)
	}
	next := snap.Task
	next.State = taskboard.StateDone
	next.Seq++
	entry := auditEntry(controlplane.ActionForceRelease, "t1")

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	start := make(chan struct{})
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs <- c.CommitTask(ctx, snap.Version, next, entry)
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
	after, err := c.SnapshotTask(ctx, "t1")
	if err != nil {
		t.Fatalf("SnapshotTask after: %v", err)
	}
	if after.Version != snap.Version+1 {
		t.Fatalf("版本应只推进一次：%d → %d", snap.Version, after.Version)
	}
}

// TestControlStoreSnapshotNeverMixesVersionAndState 版本锚与任务状态必须来自
// **同一瞬间**（不得混读两个瞬间）。
//
// 判据形态：一边提交（任务状态与版本锚同事务推进），一边连读快照 ⇒ 每个快照
// 要么是「旧状态 + 旧版本」，要么是「新状态 + 新版本」；出现「新状态 + 旧版本」
// 就是快照读到了两次不同的瞬间（一致快照被破坏）。
func TestControlStoreSnapshotNeverMixesVersionAndState(t *testing.T) {
	ctx := ctrlCtx(t)
	s := openStore(t)
	s.Board().Seed(taskboard.Task{ID: "t1", State: taskboard.StateOpen})
	c := s.Control(&controlplane.MemAuditor{})

	snap, err := c.SnapshotTask(ctx, "t1")
	if err != nil {
		t.Fatalf("SnapshotTask: %v", err)
	}
	next := snap.Task
	next.State = taskboard.StateDone
	next.Seq++

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := c.CommitTask(ctx, snap.Version, next,
			auditEntry(controlplane.ActionForceRelease, "t1")); err != nil {
			t.Errorf("CommitTask: %v", err)
		}
	}()
	// 收尾等待必须写成 defer 形态（issue #440）：失败路径上（t.Fatalf → Goexit）
	// 不执行它之后的普通语句 ⇒ 提交 goroutine 必须由 defer join。
	defer wg.Wait()
	for range 200 {
		got, err := c.SnapshotTask(ctx, "t1")
		if err != nil {
			t.Fatalf("SnapshotTask: %v", err)
		}
		done := got.Task.State == taskboard.StateDone
		if done != (got.Version == snap.Version+1) {
			t.Fatalf("快照混读了两个瞬间：状态=%q 版本=%d", got.Task.State, got.Version)
		}
	}
}

// TestControlStoreCommitRejectsMissingTarget 目标不存在的提交 ⇒ ErrNotFound，
// 且**不推进版本**（失败的提交不留下任何痕迹）。
func TestControlStoreCommitRejectsMissingTarget(t *testing.T) {
	ctx := ctrlCtx(t)
	s := openStore(t)
	c := s.Control(&controlplane.MemAuditor{})

	// 先做一次成功提交，拿到一个确定的版本基线。
	if err := c.SeedRun(ctx, "run-1", "running", 1,
		taskboard.Task{ID: "t1", State: taskboard.StateOpen}); err != nil {
		t.Fatalf("SeedRun: %v", err)
	}
	base, err := c.SnapshotTask(ctx, "t1")
	if err != nil {
		t.Fatalf("SnapshotTask: %v", err)
	}

	// 任务不存在。
	err = c.CommitTask(ctx, base.Version, taskboard.Task{ID: "nope", State: taskboard.StateDone},
		auditEntry(controlplane.ActionForceRelease, "nope"))
	if !errors.Is(err, controlplane.ErrNotFound) {
		t.Fatalf("不存在的任务提交必须 ErrNotFound，实得 %v", err)
	}
	// 运行不存在。
	err = c.CommitRun(ctx, base.Version, "nope", controlplane.RunState{Status: "done"},
		auditEntry(controlplane.ActionTerminate, "nope"))
	if !errors.Is(err, controlplane.ErrNotFound) {
		t.Fatalf("不存在的运行提交必须 ErrNotFound，实得 %v", err)
	}
	// 效果键不存在。
	err = c.CommitEffect(ctx, base.Version, "nope", controlplane.EffectState{},
		auditEntry(controlplane.ActionAdjudicate, "nope"))
	if !errors.Is(err, controlplane.ErrNotFound) {
		t.Fatalf("不存在的效果键提交必须 ErrNotFound，实得 %v", err)
	}

	after, err := c.SnapshotTask(ctx, "t1")
	if err != nil {
		t.Fatalf("SnapshotTask after: %v", err)
	}
	if after.Version != base.Version {
		t.Fatalf("失败的提交不得推进版本：%d → %d", base.Version, after.Version)
	}
}

// TestControlStoreCommitTaskRoundTripsLease 提交**认领态**的任务 ⇒ 租约三件
// 套（owner / token / fence）与到期时刻原样落库并读得回。
//
// 为什么这条要有：非认领态的空值与认领态的非空值走的是两条归一分支
// （owner/token 空串 → NULL；until 零值 → NULL）⇒ 只测空值那侧 ⇒ 非空那侧
// 的归一与回读没人验证（写成了空串也读不出差别）。
func TestControlStoreCommitTaskRoundTripsLease(t *testing.T) {
	ctx := ctrlCtx(t)
	s := openStore(t)
	s.Board().Seed(taskboard.Task{ID: "t1", State: taskboard.StateOpen})
	c := s.Control(&controlplane.MemAuditor{})

	snap, err := c.SnapshotTask(ctx, "t1")
	if err != nil {
		t.Fatalf("SnapshotTask: %v", err)
	}
	next := snap.Task
	next.State = taskboard.StateClaimed
	next.Owner = "agent-a"
	next.Lease.Token = "tok-1"
	next.Lease.Fence = 7
	next.Lease.Until = time.UnixMilli(1_700_000_000_000).UTC()
	next.Seq++

	if err := c.CommitTask(ctx, snap.Version, next,
		auditEntry(controlplane.ActionForceRelease, "t1")); err != nil {
		t.Fatalf("CommitTask: %v", err)
	}
	got, err := c.SnapshotTask(ctx, "t1")
	if err != nil {
		t.Fatalf("SnapshotTask after: %v", err)
	}
	if got.Task.State != taskboard.StateClaimed {
		t.Fatalf("状态未按提交落库：%+v", got.Task)
	}
	if got.Task.Owner != "agent-a" || got.Task.Lease.Token != "tok-1" {
		t.Fatalf("持有者/令牌未按提交落库：%+v", got.Task)
	}
	if got.Task.Lease.Fence != 7 {
		t.Fatalf("接管代次未按提交落库：%+v", got.Task.Lease)
	}
	if !got.Task.Lease.Until.Equal(next.Lease.Until) {
		t.Fatalf("到期时刻未按提交落库：got=%v want=%v", got.Task.Lease.Until, next.Lease.Until)
	}
}
