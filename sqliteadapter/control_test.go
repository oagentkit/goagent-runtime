// control_test.go —— 控制面（controlplane.Store）的 SQLite 参考实现判据。
//
// # 判据对应验收标准（逐条对位 pgstore（v0.2.0 自 goagent-kernel 迁入）/pg_control_test.go）
//
//   - **版本锚**：快照之后目标被改过 ⇒ 提交必失败（陈旧版本 ⇒ ErrStoreConflict），
//     且失败的提交**不写任何状态**（状态与版本一字不动）；同版本重复提交同样
//     被拒（版本锚已被上一次成功提交推进）；
//   - **快照一致**：版本锚与任务集合来自**同一瞬间**（不得混读两个瞬间）；
//   - **留痕与状态同事务方向**：留痕失败 ⇒ 状态一字不动；缺留痕面 ⇒ 一律
//     拒绝；留痕实现 panic ⇒ 被隔离为 ErrAuditorPanic，状态一字不动；
//   - **控制面与任务板同表**：CommitTask 后 Board.List 立即看到同一份状态
//     （不存在第二份可写任务状态）；
//   - **装配期同源**：控制面与任务板同一数据文件 ⇒ 来源标识一致。
//
// # 夹具用合成数据
//
// 任务 id 用 t1/t2、run id 用 run-1、效果键用 effect-1——夹具是契约的示例，
// 示例带领域味等于把契约钉死在某个域上。
package sqliteadapter

import (
	"context"
	"testing"
	"time"

	"github.com/oekit/pkg/errors"

	"github.com/oagentkit/goagent-kernel/controlplane"
	"github.com/oagentkit/goagent-kernel/taskboard"
)

// ctrlCtx 用例的 ctx（带超时：回归要**快速红**，而不是挂在 60s 超时上）。
//
// 超时取 10s（同包既有 helper 的档位）：控制面的提交路径一旦误开嵌套事务，
// 这里会快速变红，而不是挂死。
func ctrlCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// failAuditor 留痕失败注入：Append 永远报错。
type failAuditor struct{}

func (failAuditor) Append(context.Context, controlplane.AuditEntry) error {
	return errors.New("sqliteadapter-test: 留痕后端不可用")
}

func (failAuditor) Entries(context.Context, string) ([]controlplane.AuditEntry, error) {
	return nil, nil
}

// panicAuditor 留痕 panic 注入（issue #327 的隔离判据）。
type panicAuditor struct{}

func (panicAuditor) Append(context.Context, controlplane.AuditEntry) error {
	panic("sqliteadapter-test: 留痕实现 panic")
}

func (panicAuditor) Entries(context.Context, string) ([]controlplane.AuditEntry, error) {
	return nil, nil
}

// auditEntry 一条**合规**的介入留痕。
//
// MemAuditor 拒收缺 Operator/Target/Basis 或词表外动作的条目 ⇒ 夹具必须给全，
// 否则失败的是夹具而不是被测实现。
func auditEntry(action controlplane.Action, target string) controlplane.AuditEntry {
	return controlplane.AuditEntry{
		Operator: "op-1",
		Action:   action,
		Target:   target,
		Basis:    "synthetic basis",
	}
}

// TestControlStoreVersionAnchor 快照 → 陈旧版本提交被拒 → 正确版本提交成功。
//
// 这是控制面的**全部价值所在**：快照之后目标被任何副本改过 ⇒ 提交必失败。
func TestControlStoreVersionAnchor(t *testing.T) {
	ctx := ctrlCtx(t)
	s := openStore(t)
	s.Board().Seed(taskboard.Task{ID: "t1", State: taskboard.StateOpen})
	aud := &controlplane.MemAuditor{}
	c := s.Control(aud)

	snap, err := c.SnapshotTask(ctx, "t1")
	if err != nil {
		t.Fatalf("SnapshotTask: %v", err)
	}
	if snap.Version != 0 || snap.Task.ID != "t1" {
		t.Fatalf("任务快照内容错误: %+v", snap)
	}

	// 陈旧版本（快照之后目标被改过）⇒ 拒绝且不写。
	next := snap.Task
	next.State = taskboard.StateDone
	next.Seq++
	err = c.CommitTask(ctx, snap.Version+1, next,
		auditEntry(controlplane.ActionForceRelease, "t1"))
	if !errors.Is(err, controlplane.ErrStoreConflict) {
		t.Fatalf("陈旧版本提交必须 ErrStoreConflict，实得 %v", err)
	}
	after, err := c.SnapshotTask(ctx, "t1")
	if err != nil {
		t.Fatalf("SnapshotTask after conflict: %v", err)
	}
	if after.Version != snap.Version || after.Task.State != taskboard.StateOpen {
		t.Fatalf("失败的 CAS 不得改变状态/版本：%+v", after)
	}
	// 失败的 CAS **不是一次介入** ⇒ 不得追加留痕（契约明写）。
	entries, err := aud.Entries(ctx, "t1")
	if err != nil {
		t.Fatalf("读留痕失败: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("失败的 CAS 不得追加留痕，实得 %d 条", len(entries))
	}

	// 正确版本 ⇒ 提交成功并推进版本。
	if err := c.CommitTask(ctx, snap.Version, next,
		auditEntry(controlplane.ActionForceRelease, "t1")); err != nil {
		t.Fatalf("CommitTask: %v", err)
	}
	got, err := c.SnapshotTask(ctx, "t1")
	if err != nil {
		t.Fatalf("SnapshotTask committed: %v", err)
	}
	if got.Version != snap.Version+1 || got.Task.State != taskboard.StateDone {
		t.Fatalf("提交后版本/状态错误：%+v", got)
	}

	// 同版本重复提交 ⇒ 冲突（版本锚已被上一次成功提交推进）。
	err = c.CommitTask(ctx, snap.Version, next,
		auditEntry(controlplane.ActionForceRelease, "t1"))
	if !errors.Is(err, controlplane.ErrStoreConflict) {
		t.Fatalf("同版本重复提交必须 ErrStoreConflict，实得 %v", err)
	}
	final, err := c.SnapshotTask(ctx, "t1")
	if err != nil {
		t.Fatalf("SnapshotTask final: %v", err)
	}
	if final.Version != snap.Version+1 {
		t.Fatalf("失败的重复提交不得推进版本：%d → %d", snap.Version+1, final.Version)
	}
}

// TestControlStoreSnapshotNotFound 目标不存在 ⇒ ErrNotFound（不是零值 + nil）。
func TestControlStoreSnapshotNotFound(t *testing.T) {
	ctx := ctrlCtx(t)
	s := openStore(t)
	c := s.Control(&controlplane.MemAuditor{})

	if _, err := c.SnapshotTask(ctx, "nope"); !errors.Is(err, controlplane.ErrNotFound) {
		t.Fatalf("不存在的任务必须 ErrNotFound，实得 %v", err)
	}
	if _, err := c.SnapshotRun(ctx, "nope"); !errors.Is(err, controlplane.ErrNotFound) {
		t.Fatalf("不存在的运行必须 ErrNotFound，实得 %v", err)
	}
	if _, err := c.SnapshotEffect(ctx, "nope"); !errors.Is(err, controlplane.ErrNotFound) {
		t.Fatalf("不存在的效果键必须 ErrNotFound，实得 %v", err)
	}
}

// TestControlStoreNeedsAuditor 缺留痕面 ⇒ 提交一律拒绝，状态与版本不变。
func TestControlStoreNeedsAuditor(t *testing.T) {
	ctx := ctrlCtx(t)
	s := openStore(t)
	s.Board().Seed(taskboard.Task{ID: "t1", State: taskboard.StateOpen})
	c := s.Control(nil) // 留痕面缺位

	snap, err := c.SnapshotTask(ctx, "t1")
	if err != nil {
		t.Fatalf("SnapshotTask: %v", err)
	}
	next := snap.Task
	next.State = taskboard.StateDone
	err = c.CommitTask(ctx, snap.Version, next,
		auditEntry(controlplane.ActionForceRelease, "t1"))
	if !errors.Is(err, controlplane.ErrNoAuditor) {
		t.Fatalf("缺留痕面必须 ErrNoAuditor，实得 %v", err)
	}
	after, err := c.SnapshotTask(ctx, "t1")
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
	ctx := ctrlCtx(t)
	s := openStore(t)
	s.Board().Seed(taskboard.Task{ID: "t1", State: taskboard.StateOpen})
	c := s.Control(failAuditor{})

	snap, err := c.SnapshotTask(ctx, "t1")
	if err != nil {
		t.Fatalf("SnapshotTask: %v", err)
	}
	next := snap.Task
	next.State = taskboard.StateDone
	if err := c.CommitTask(ctx, snap.Version, next,
		auditEntry(controlplane.ActionForceRelease, "t1")); err == nil {
		t.Fatal("留痕失败必须让提交失败")
	}
	after, err := c.SnapshotTask(ctx, "t1")
	if err != nil {
		t.Fatalf("SnapshotTask after: %v", err)
	}
	if after.Version != snap.Version || after.Task.State != taskboard.StateOpen {
		t.Fatalf("留痕失败后状态/版本必须不变：%+v", after)
	}
}

// TestControlStoreAuditorPanicIsolated 留痕实现 panic ⇒ 被隔离为
// ErrAuditorPanic，且状态一字不动（issue #327）。
func TestControlStoreAuditorPanicIsolated(t *testing.T) {
	ctx := ctrlCtx(t)
	s := openStore(t)
	s.Board().Seed(taskboard.Task{ID: "t1", State: taskboard.StateOpen})
	c := s.Control(panicAuditor{})

	snap, err := c.SnapshotTask(ctx, "t1")
	if err != nil {
		t.Fatalf("SnapshotTask: %v", err)
	}
	next := snap.Task
	next.State = taskboard.StateDone
	err = c.CommitTask(ctx, snap.Version, next,
		auditEntry(controlplane.ActionForceRelease, "t1"))
	if !errors.Is(err, controlplane.ErrAuditorPanic) {
		t.Fatalf("留痕 panic 必须被隔离为 ErrAuditorPanic，实得 %v", err)
	}
	after, err := c.SnapshotTask(ctx, "t1")
	if err != nil {
		t.Fatalf("SnapshotTask after: %v", err)
	}
	if after.Version != snap.Version || after.Task.State != taskboard.StateOpen {
		t.Fatalf("留痕 panic 后状态/版本必须不变：%+v", after)
	}
}
