// pg_store_facade_test.go —— 门面**可达性**的判据（评审 I-3）+ nil pool 的
// fail fast（M-1）。
//
// # 为什么判「可达性」而不是「门面暴露了几个访问器」
//
// Task 9 的比对口径 = **门面可达性**：判「宿主只持门面时能做什么」——宿主拿到
// `*pgstore.Store` 就应当能装配出全部面（租户版面、栅栏面、游标面、控制面），
// 而不必绕过门面去拿连接池自己造。
//
// ⇒ PG 门面补齐 7 个访问器（与 SQLite 门面同款集合）⇒ 两侧对位成为**事实**，
// 而不是「口径上说等价」。
//
// # 每个访问器一条判据
//
// 返回的面必须**可用**（构造成功 + 关键方法可调），且**租户型**必须缺租户
// fail-closed（与既有口径同款）⇒ 「返回错形态的面」（默认型冒充租户型，或
// 反之）会被判出来。
package pgstore

import (
	"testing"
	"time"

	"github.com/oekit/pkg/errors"

	"github.com/oagentkit/goagent-kernel/checkpoint"
	"github.com/oagentkit/goagent-kernel/controlplane"
	"github.com/oagentkit/goagent-kernel/eventbus"
	"github.com/oagentkit/goagent-kernel/sideeffect"
	"github.com/oagentkit/goagent-kernel/taskboard"
	"github.com/oagentkit/goagent-kernel/tenant"
)

// TestStoreFacadeReachesEveryFace 门面可达性：宿主只持 `*Store` 就能装配出
// 全部面，且每个面都可用。
func TestStoreFacadeReachesEveryFace(t *testing.T) {
	dsn := requireDSN(t)
	ctx := applyCtx(t)
	s := freshStore(t, dsn)
	tCtx := tenant.WithContext(ctx, tenant.Context{Tenant: "t-facade"})

	t.Run("任务面", func(t *testing.T) {
		s.Board().Seed(taskboard.Task{ID: "t1", Title: "待领", State: taskboard.StateOpen})
		tasks, err := s.Board().List(ctx)
		if err != nil {
			t.Fatalf("任务面不可用: %v", err)
		}
		if len(tasks) != 1 || tasks[0].ID != "t1" {
			t.Fatalf("任务面读不到刚摆的任务，实得 %+v", tasks)
		}
	})

	t.Run("副作用面", func(t *testing.T) {
		// 默认形态：它不解析租户 ⇒ 缺租户**不**拒绝（判它确实是默认形态）。
		if _, _, _, err := s.Effects().Acquire(ctx,
			sideeffect.Action{Key: "k-f1", Resource: "res-f1"}, time.Minute); err != nil {
			t.Fatalf("默认形态的副作用面不应要求租户上下文: %v", err)
		}
	})

	t.Run("租户作用域副作用面", func(t *testing.T) {
		act := sideeffect.Action{Key: "k-f2", Resource: "res-f2"}
		if _, _, _, err := s.TenantEffects().Acquire(ctx, act, time.Minute); !errors.Is(err, tenant.ErrNoContext) {
			t.Fatalf("租户作用域形态缺租户必须 fail-closed（tenant.ErrNoContext），实得 %v", err)
		}
		if _, _, _, err := s.TenantEffects().Acquire(tCtx, act, time.Minute); err != nil {
			t.Fatalf("带租户上下文时租户作用域形态必须可用: %v", err)
		}
	})

	t.Run("快照面", func(t *testing.T) {
		if err := s.Snapshots().SaveIfNewer(ctx,
			checkpoint.Snapshot{RunID: "run-f", Seq: 1, Status: "running"}); err != nil {
			t.Fatalf("快照面不可用: %v", err)
		}
	})

	t.Run("事件面", func(t *testing.T) {
		if _, err := s.Events().Insert(ctx, eventbus.Event{SessionID: "s-f", Type: "t"}); err != nil {
			t.Fatalf("事件面不可用: %v", err)
		}
	})

	t.Run("租户作用域事件面", func(t *testing.T) {
		ev := eventbus.Event{SessionID: "s-f2", Type: "t"}
		if _, err := s.TenantEvents().Insert(ctx, ev); !errors.Is(err, tenant.ErrNoContext) {
			t.Fatalf("租户作用域形态缺租户必须 fail-closed，实得 %v", err)
		}
		if _, err := s.TenantEvents().Insert(tCtx, ev); err != nil {
			t.Fatalf("带租户上下文时租户作用域形态必须可用: %v", err)
		}
	})

	t.Run("下游栅栏面", func(t *testing.T) {
		// 默认形态：它不解析租户 ⇒ 缺租户**不**拒绝。
		if err := s.Downstream().WriteFenced(ctx, "res-f3", 1, nil); err != nil {
			t.Fatalf("默认形态的栅栏面不应要求租户上下文: %v", err)
		}
	})

	t.Run("租户作用域下游栅栏面", func(t *testing.T) {
		if err := s.TenantDownstream().WriteFenced(ctx, "res-f4", 1, nil); !errors.Is(err, tenant.ErrNoContext) {
			t.Fatalf("租户作用域形态缺租户必须 fail-closed，实得 %v", err)
		}
		if err := s.TenantDownstream().WriteFenced(tCtx, "res-f4", 1, nil); err != nil {
			t.Fatalf("带租户上下文时租户作用域形态必须可用: %v", err)
		}
	})

	t.Run("尾随游标面", func(t *testing.T) {
		if err := s.TailCursors("consumer-a").SaveCursor(ctx, 3); err != nil {
			t.Fatalf("游标面不可用: %v", err)
		}
		cur, found, err := s.TailCursors("consumer-a").LoadCursor(ctx)
		if err != nil {
			t.Fatalf("读游标失败: %v", err)
		}
		if !found || cur != 3 {
			t.Fatalf("游标面读不回刚写的游标，实得 cursor=%d found=%v", cur, found)
		}
		// consumer key 必须**真的**接上：另一个 consumer 读不到这一行。
		if _, found2, err2 := s.TailCursors("consumer-b").LoadCursor(ctx); err2 != nil {
			t.Fatalf("读游标失败: %v", err2)
		} else if found2 {
			t.Fatal("不同 consumer key 必须互相独立（consumer 没接上？）")
		}
	})

	t.Run("租户作用域尾随游标面", func(t *testing.T) {
		if err := s.TenantTailCursors("consumer-c").SaveCursor(ctx, 4); !errors.Is(err, tenant.ErrNoContext) {
			t.Fatalf("租户作用域形态缺租户必须 fail-closed，实得 %v", err)
		}
		if err := s.TenantTailCursors("consumer-c").SaveCursor(tCtx, 4); err != nil {
			t.Fatalf("带租户上下文时租户作用域形态必须可用: %v", err)
		}
		cur, found, err := s.TenantTailCursors("consumer-c").LoadCursor(tCtx)
		if err != nil {
			t.Fatalf("读游标失败: %v", err)
		}
		if !found || cur != 4 {
			t.Fatalf("租户作用域游标面读不回刚写的游标，实得 cursor=%d found=%v", cur, found)
		}
		// consumer key 必须**真的**接上：另一个 consumer 读不到这一行。
		if _, found2, err2 := s.TenantTailCursors("consumer-d").LoadCursor(tCtx); err2 != nil {
			t.Fatalf("读游标失败: %v", err2)
		} else if found2 {
			t.Fatal("不同 consumer key 必须互相独立（consumer 没接上？）")
		}
	})

	t.Run("控制面", func(t *testing.T) {
		ctrl := s.Control(&controlplane.MemAuditor{})
		if ctrl.TaskSourceID() != s.Board().TaskSourceID() {
			t.Fatalf("控制面与任务面必须同源（同一张任务表），实得 %q vs %q",
				ctrl.TaskSourceID(), s.Board().TaskSourceID())
		}
		if err := ctrl.SeedRun(ctx, "run-f", "running", 1,
			taskboard.Task{ID: "t2", Title: "待领", State: taskboard.StateOpen}); err != nil {
			t.Fatalf("控制面不可用: %v", err)
		}
		snap, err := ctrl.SnapshotTask(ctx, "t2")
		if err != nil {
			t.Fatalf("控制面读快照失败: %v", err)
		}
		if snap.Task.ID != "t2" {
			t.Fatalf("控制面读不到刚摆的任务，实得 %+v", snap)
		}
		// 审计器必须**真的**接上：提交要经留痕面（审计器缺位 ⇒ ErrNoAuditor）。
		next := snap.Task
		next.State = taskboard.StateDone
		next.Seq++
		if err := ctrl.CommitTask(ctx, snap.Version, next, controlplane.AuditEntry{
			Operator: "op", Action: controlplane.ActionForceRelease, Target: "t2", Basis: "facade",
		}); err != nil {
			t.Fatalf("控制面提交失败（审计器没接上？）: %v", err)
		}
	})
}

// TestNewStoreRejectsNilPool NewStore(nil) 是装配期编程错误 ⇒ fail fast。
//
// 判据：必须 panic（静默返回一个 pool 为 nil 的门面 ⇒ 后续每个方法都在 nil 上
// 调用，报错点离装配点十万八千里 ⇒ 排障成本高）。
func TestNewStoreRejectsNilPool(t *testing.T) {
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("NewStore(nil) 必须 panic（装配期编程错误不容静默通过）")
			}
		}()
		_ = NewStore(nil)
	}()
}
