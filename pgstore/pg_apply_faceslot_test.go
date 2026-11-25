// pg_apply_faceslot_test.go —— 失败槽**覆盖面**的逐面判据（PG 侧）。
//
// 「失败即作废整批」只有一个承诺：**批内任一面失败，整批都不提交**。
// 这个承诺要靠失败槽的覆盖面承重 —— 某个面的写路径若没经失败槽登记，
// 那条路径上的失败被调用方吞掉时，整批仍会提交 ⇒ 跨面半截状态。
//
// ⇒ 本文件**逐面**触发一次失败（错误被吞掉），断言整批仍被作废。
// 少任何一面的登记，那一面就是「失败即作废整批」里的假话。
//
// ⚠ PG 侧的两个取样点与 SQLite 侧不同（机制决定）：
//   - 事件面用**租户作用域形态**的缺租户拒绝（resolveTenant 在事务内失败，
//     **非** SQL 错误 ⇒ 事务未中止 ⇒ 正是失败槽要兜的形态）；
//   - 快照面用序号为 0 的落库前拒绝（**不进** runTx 的那条路径 ⇒ 必须显式
//     登记，否则该面对「失败即作废整批」是假的）。
package pgstore

import (
	"context"
	"testing"
	"time"

	"github.com/oekit/pkg/errors"

	"github.com/oagentkit/goagent-kernel/checkpoint"
	"github.com/oagentkit/goagent-kernel/eventbus"
	"github.com/oagentkit/goagent-kernel/sideeffect"
	"github.com/oagentkit/goagent-kernel/taskboard"
)

// TestApplyFailureSlotCoversEachFace 失败槽的覆盖面：**逐面**核。
//
// 每个面各触发一次失败（错误被吞掉），断言整批仍被作废 ⇒ 该面的写路径确实
// 经由失败槽登记。少了任何一面的登记，「失败即作废整批」对那一面就是假的。
//
// ⚠ PG 侧的两个取样点与 SQLite 侧不同（机制决定）：
//   - 事件面用**租户作用域形态**的缺租户拒绝（resolveTenant 在事务内失败，
//     **非** SQL 错误 ⇒ 事务未中止 ⇒ 正是失败槽要兜的形态）；
//   - 快照面用序号为 0 的落库前拒绝（**不进** runTx 的那条路径 ⇒ 必须显式
//     登记，否则该面对「失败即作废整批」是假的）。
func TestApplyFailureSlotCoversEachFace(t *testing.T) {
	dsn := requireDSN(t)
	cases := []struct {
		name string
		// fail 在批内触发一次失败（返回的错误会被用例吞掉）。
		fail func(ctx context.Context, s *Store) error
		// write 批内的一次成功写（用于证明整批被作废）。
		write func(ctx context.Context, s *Store) error
		// assertVoided 批外读：断言批内的写没有留下。
		assertVoided func(t *testing.T, ctx context.Context, s *Store)
	}{
		{
			name: "任务面",
			fail: func(ctx context.Context, s *Store) error {
				return s.Board().Complete(ctx, "t1", "no-such-token")
			},
			write: func(ctx context.Context, s *Store) error {
				_, err := s.Board().Claim(ctx, "t1", "agent-a", taskboard.ClaimRequest{TTL: time.Minute})
				return err
			},
			assertVoided: func(t *testing.T, ctx context.Context, s *Store) {
				t.Helper()
				tasks, err := s.Board().List(ctx)
				if err != nil {
					t.Fatalf("读任务失败: %v", err)
				}
				if len(tasks) != 1 || tasks[0].State != taskboard.StateOpen {
					t.Fatalf("任务面未登记失败 ⇒ 整批仍提交了（半截事务），实得 %+v", tasks)
				}
			},
		},
		{
			name: "副作用面",
			fail: func(ctx context.Context, s *Store) error {
				// 先在同一批里登记身份（成功），再用不一致的资源 ⇒ 身份冲突。
				if _, _, _, err := s.Effects().Acquire(ctx,
					sideeffect.Action{Key: "k-conflict", Resource: "res-a"}, time.Minute); err != nil {
					return err
				}
				_, _, _, err := s.Effects().Acquire(ctx,
					sideeffect.Action{Key: "k-conflict", Resource: "res-b"}, time.Minute)
				return err
			},
			write: func(ctx context.Context, s *Store) error {
				_, _, _, err := s.Effects().Acquire(ctx,
					sideeffect.Action{Key: "k-other", Resource: "res-c"}, time.Minute)
				return err
			},
			assertVoided: func(t *testing.T, ctx context.Context, s *Store) {
				t.Helper()
				var n int
				if err := s.pool.QueryRow(ctx,
					`SELECT COUNT(*) FROM pgref_effect_action`).Scan(&n); err != nil {
					t.Fatalf("读动作行数失败: %v", err)
				}
				if n != 0 {
					t.Fatalf("副作用面未登记失败 ⇒ 整批仍提交了（半截事务），实得 %d 行", n)
				}
			},
		},
		{
			name: "快照面",
			fail: func(ctx context.Context, s *Store) error {
				// 序号 0 ⇒ 落库前的拒绝（**不进** runTx 的那条路径）。
				return s.Snapshots().SaveIfNewer(ctx,
					checkpoint.Snapshot{RunID: "run-bad", Seq: 0})
			},
			write: func(ctx context.Context, s *Store) error {
				return s.Snapshots().SaveIfNewer(ctx,
					checkpoint.Snapshot{RunID: "run-ok", Seq: 1, Status: "running"})
			},
			assertVoided: func(t *testing.T, ctx context.Context, s *Store) {
				t.Helper()
				if _, err := s.Snapshots().Load(ctx, "run-ok"); !errors.Is(err, checkpoint.ErrNotFound) {
					t.Fatalf("快照面未登记失败 ⇒ 整批仍提交了（半截事务），实得 err=%v", err)
				}
			},
		},
		{
			name: "事件面",
			fail: func(ctx context.Context, s *Store) error {
				// 租户作用域形态 + 缺租户上下文 ⇒ 批内拒绝（事务未中止）。
				_, err := NewTenantEventStore(s.pool).Insert(ctx,
					eventbus.Event{SessionID: "s-bad", Type: "t"})
				return err
			},
			write: func(ctx context.Context, s *Store) error {
				_, err := s.Events().Insert(ctx, eventbus.Event{SessionID: "s-ok", Type: "t"})
				return err
			},
			assertVoided: func(t *testing.T, ctx context.Context, s *Store) {
				t.Helper()
				wm, err := s.Events().CommittedWatermark(ctx)
				if err != nil {
					t.Fatalf("读水位线失败: %v", err)
				}
				if wm != 0 {
					t.Fatalf("事件面未登记失败 ⇒ 整批仍提交了（半截事务），水位线=%d", wm)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := applyCtx(t)
			s := freshStore(t, dsn)
			s.Board().Seed(taskboard.Task{ID: "t1", Title: "待领", State: taskboard.StateOpen})

			err := s.Apply(ctx, func(ctx context.Context) error {
				// 批内的一次成功写（先写：PG 里 SQL 错误会中止事务，失败用例一律
				// 用非 SQL 错误的拒绝 ⇒ 事务仍然可用）。
				if werr := tc.write(ctx, s); werr != nil {
					return werr
				}
				// 内层失败：错误被吞掉。
				_ = tc.fail(ctx, s)
				return nil
			})
			if err == nil {
				t.Fatalf("%s：内层失败被吞掉时 Apply 不得报成功（本批已作废）", tc.name)
			}
			tc.assertVoided(t, ctx, s)
		})
	}
}
