// pg_apply_test.go —— 跨面 Apply 的判据（PG 侧）：同一次提交 + 失败即作废整批。
//
// # 为什么要有 Apply
//
// 本文件覆盖的这几张面（任务板 / 副作用 / 快照 / 事件）各自 Begin/Commit ⇒
// 「任务做完了但没留痕」「留痕了但没做完」两类半截状态结构性地可能。
// Apply 把各面的变更收进**同一次提交** ⇒ 跨面不会出现「一半已提交」。
//
// # PG 侧与 SQLite 侧**必然不同**的三处（机制决定，不是漏做）
//
//  1. **Apply 的表层形态**：PG 侧是 Apply(ctx, fn func(ctx context.Context) error)
//     —— 面按调用从 ctx 里取 tx；SQLite 侧是 Apply(ctx, fn func(tx *Tx) error)
//     —— 面在构造时绑定 tx。
//  2. **批内的读**：PG 的面读走 pool（MVCC 已提交快照）⇒ 批内**读面**看不见本批
//     未提交写；SQLite 的读走 exec.reader 的路由（tx != nil ⇒ 用 tx）⇒ 看得见。
//  3. **SQL 错误会中止整个事务**：PG 里一条语句失败 ⇒ 事务进入中止态 ⇒ 后续语句
//     全失败 ⇒ 「失败之后再写」的用例在 PG 侧必须用**非 SQL 错误**的失败（校验
//     拒绝、租户拒绝）⇒ 本文件的失败用例一律走那类。
//
// # 用例的 ctx 一律带超时：回归要**快速红**，而不是挂在 60s 超时上。
package pgstore

import (
	"context"
	"testing"
	"time"

	"github.com/oekit/pkg/errors"

	"github.com/yi-nology/goagent-kernel/checkpoint"
	"github.com/yi-nology/goagent-kernel/eventbus"
	"github.com/yi-nology/goagent-kernel/sideeffect"
	"github.com/yi-nology/goagent-kernel/taskboard"
)

// applyCtx 用例的 ctx（带超时：回归要**快速红**，而不是挂在超时上）。
func applyCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// freshStore 一个子测试一份全新门面：随机 schema → 建表 → 独立连接池
// （search_path 指到该 schema）。t.Cleanup 整块删除。
//
// 复用既有 fixture 的零件（basePool / NewSchema / EnsureSchema /
// DropSchema / schemaPool，见 pg_test.go），**不新造**一套建库机制。
func freshStore(t *testing.T, dsn string) *Store {
	t.Helper()
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
	return NewStore(schemaPool(t, dsn, schema))
}

// TestApplyCommitsAllFacesAtomically 本用例覆盖的这几张面（任务板 / 副作用 /
// 快照 / 事件）的变更落在**同一次提交**里。
//
// 判据：提交后这几张面都读得到（一次提交 ⇒ 全部生效）。
func TestApplyCommitsAllFacesAtomically(t *testing.T) {
	dsn := requireDSN(t)
	ctx := applyCtx(t)
	s := freshStore(t, dsn)
	// 任务拓扑是宿主资产：Seed 不进批次（装载入口无 ctx，自带一次提交）。
	s.Board().Seed(taskboard.Task{ID: "t1", Title: "待领", State: taskboard.StateOpen})

	err := s.Apply(ctx, func(ctx context.Context) error {
		// 任务面：认领（读-判-写同事务）。
		if _, err := s.Board().Claim(ctx, "t1", "agent-a", taskboard.ClaimRequest{TTL: time.Minute}); err != nil {
			return err
		}
		// 副作用面：授予执行权。
		if _, _, _, err := s.Effects().Acquire(ctx,
			sideeffect.Action{Key: "k-apply", Resource: "res-apply"}, time.Minute); err != nil {
			return err
		}
		// 快照面：落一条进度快照。
		if err := s.Snapshots().SaveIfNewer(ctx, checkpoint.Snapshot{
			RunID: "run-apply", Seq: 1, Status: "running",
		}); err != nil {
			return err
		}
		// 事件面：发布一条事件。
		if _, err := s.Events().Insert(ctx, eventbus.Event{SessionID: "s-apply", Type: "t-apply"}); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Apply 失败: %v", err)
	}

	// 本用例覆盖的这几张面都读得到（⇒ 变更同生共死）。
	tasks, err := s.Board().List(ctx)
	if err != nil {
		t.Fatalf("读任务失败: %v", err)
	}
	if len(tasks) != 1 || tasks[0].State != taskboard.StateClaimed {
		t.Fatalf("任务面未随批提交：%+v", tasks)
	}
	snap, err := s.Snapshots().Load(ctx, "run-apply")
	if err != nil {
		t.Fatalf("读快照失败（快照面未随批提交）: %v", err)
	}
	if snap.Seq != 1 {
		t.Fatalf("快照序号应为 1，实得 %d", snap.Seq)
	}
	wm, err := s.Events().CommittedWatermark(ctx)
	if err != nil {
		t.Fatalf("读水位线失败: %v", err)
	}
	if wm != 1 {
		t.Fatalf("事件面未随批提交（水位线应为 1），实得 %d", wm)
	}
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT COUNT(*) FROM pgref_effect_action`).Scan(&n); err != nil {
		t.Fatalf("读动作行数失败: %v", err)
	}
	if n != 1 {
		t.Fatalf("副作用面未随批提交，实得 %d 行", n)
	}
}

// TestApplyRollsBackEveryFaceTogether 回调返回错误 ⇒ 本用例覆盖的这几张面
// （任务板 / 副作用 / 快照 / 事件）的写**一个都不留**。
//
// 这是「同一次提交」的**承重**判据：若各面各自提交，除事件面外的那几张面的写
// 早已落库、只有事件面被回滚 ⇒ 跨面半截状态 ⇒ 判据必须红。
func TestApplyRollsBackEveryFaceTogether(t *testing.T) {
	dsn := requireDSN(t)
	ctx := applyCtx(t)
	s := freshStore(t, dsn)
	s.Board().Seed(taskboard.Task{ID: "t1", Title: "待领", State: taskboard.StateOpen})

	sentinel := errors.New("故意失败")
	err := s.Apply(ctx, func(ctx context.Context) error {
		if _, err := s.Board().Claim(ctx, "t1", "agent-a", taskboard.ClaimRequest{TTL: time.Minute}); err != nil {
			return err
		}
		if _, _, _, err := s.Effects().Acquire(ctx,
			sideeffect.Action{Key: "k-roll", Resource: "res-roll"}, time.Minute); err != nil {
			return err
		}
		if err := s.Snapshots().SaveIfNewer(ctx, checkpoint.Snapshot{
			RunID: "run-roll", Seq: 1, Status: "running",
		}); err != nil {
			return err
		}
		if _, err := s.Events().Insert(ctx, eventbus.Event{SessionID: "s-roll", Type: "t-roll"}); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("Apply 必须原样传出回调的错误: %v", err)
	}

	// 本用例覆盖的这几张面一个都没留下。
	tasks, lerr := s.Board().List(ctx)
	if lerr != nil {
		t.Fatalf("读任务失败: %v", lerr)
	}
	if len(tasks) != 1 || tasks[0].State != taskboard.StateOpen {
		t.Fatalf("任务面的写不得留下（跨面半截状态），实得 %+v", tasks)
	}
	if _, err := s.Snapshots().Load(ctx, "run-roll"); !errors.Is(err, checkpoint.ErrNotFound) {
		t.Fatalf("快照面的写不得留下，实得 err=%v", err)
	}
	wm, werr := s.Events().CommittedWatermark(ctx)
	if werr != nil {
		t.Fatalf("读水位线失败: %v", werr)
	}
	if wm != 0 {
		t.Fatalf("事件面的写不得留下（水位线应为 0），实得 %d", wm)
	}
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT COUNT(*) FROM pgref_effect_action`).Scan(&n); err != nil {
		t.Fatalf("读动作行数失败: %v", err)
	}
	if n != 0 {
		t.Fatalf("副作用面的写不得留下，实得 %d 行", n)
	}
}

// TestApplyFacesShareTheBatchTransaction 各面共用**同一个**事务句柄。
//
// 判据（PG 原生形态）：批内经公开接缝 TxFromContext 取到的事务句柄，能读到
// 本批刚写的**未提交**行 ⇒ 各面的写落进了这个 tx（分面各开事务时读不到——
// 那是另一条连接上的另一个事务）。
//
// ⚠ 与 SQLite 侧的同名判据**形态不同**：那边的面读走 exec.reader 的路由
// （tx != nil ⇒ 用 tx）⇒ 批内读面直接看得见未提交写；PG 侧的面读走 pool
// （MVCC 已提交快照）⇒ 批内**读面**看不见未提交写 ⇒ 本条经事务句柄直接读。
func TestApplyFacesShareTheBatchTransaction(t *testing.T) {
	dsn := requireDSN(t)
	ctx := applyCtx(t)
	s := freshStore(t, dsn)

	err := s.Apply(ctx, func(ctx context.Context) error {
		if _, _, _, err := s.Effects().Acquire(ctx,
			sideeffect.Action{Key: "k-shared", Resource: "res-shared"}, time.Minute); err != nil {
			return err
		}
		tx, ok := TxFromContext(ctx)
		if !ok {
			return errors.New("批内 ctx 必须带事务句柄（各面没有共用同一个 tx）")
		}
		var n int
		if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM pgref_effect_action`).Scan(&n); err != nil {
			return errors.Wrap(err, "批内读事务失败")
		}
		if n != 1 {
			return errors.New("事务内读不到本批刚写的行（各面没有共用同一个 tx）")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Apply 失败（各面没有共用同一个事务）: %v", err)
	}
}

// TestApplyVoidsBatchWhenInnerFailureSwallowed 内层错误被吞掉 ⇒ 整批仍作废。
//
// 这是「失败即作废整批」的**承重**判据：调用方吞掉错误后，若批次仍提交，
// 就会出现「一半已提交」——正是本任务要杜绝的形态。
func TestApplyVoidsBatchWhenInnerFailureSwallowed(t *testing.T) {
	dsn := requireDSN(t)
	ctx := applyCtx(t)
	s := freshStore(t, dsn)
	s.Board().Seed(taskboard.Task{ID: "t1", Title: "待领", State: taskboard.StateOpen})

	err := s.Apply(ctx, func(ctx context.Context) error {
		// 内层失败：令牌不匹配的完成必被拒（**非 SQL 错误** ⇒ 事务未中止）。
		// **故意吞掉**这个错误继续写。
		if cerr := s.Board().Complete(ctx, "t1", "no-such-token"); cerr == nil {
			return errors.New("令牌不匹配的完成必须被拒")
		}
		// 继续写别的面：这些写也必须随整批一起消失。
		if _, ierr := s.Events().Insert(ctx, eventbus.Event{SessionID: "s", Type: "t"}); ierr != nil {
			return ierr
		}
		return nil
	})
	// 失败被吞掉时，Apply 仍须把「本批已失败」这件事报出来（否则调用方以为
	// 整批提交了，而其实什么都没写 ⇒ 静默的半截状态）。
	if err == nil {
		t.Fatal("内层失败被吞掉时 Apply 不得报成功（本批已作废）")
	}
	if !errors.Is(err, taskboard.ErrLeaseLost) {
		t.Fatalf("Apply 报出的应是内层那条失败（taskboard.ErrLeaseLost），实得 %v", err)
	}
	wm, werr := s.Events().CommittedWatermark(ctx)
	if werr != nil {
		t.Fatalf("读水位线失败: %v", werr)
	}
	if wm != 0 {
		t.Fatalf("失败被吞掉后整批仍必须回滚（水位线应为 0），实得 %d", wm)
	}
	tasks, lerr := s.Board().List(ctx)
	if lerr != nil {
		t.Fatalf("读任务失败: %v", lerr)
	}
	if len(tasks) != 1 || tasks[0].State != taskboard.StateOpen {
		t.Fatalf("失败被吞掉后任务状态不得被提交（半截事务），实得 %+v", tasks)
	}
}
