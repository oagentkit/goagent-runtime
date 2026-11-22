// apply_tx_test.go —— 批内租户形态与失败槽登记点的**逐点**判据。
//
// # 为什么单独一个文件
//
// apply_test.go 已 362 行（阈值 400）⇒ 本轮的判据放这里，不把已有文件顶到
// 阈值边缘。
//
// # 本文件钉的两类东西
//
//  1. **批内租户形态**（Tx.Tenant* / Tx.Downstream）：租户作用域在批内必须
//     真带租户 ⇒ 第三段（声明 ≠ ctx ⇒ 拒绝）在批内生效；批内写进 ctx 租户的
//     流/命名空间 ⇒ 批外按租户读得到、按默认形态读不到。
//  2. **失败槽登记点的逐点判据**：每一个登记点都必须有「删掉即红」的用例。
//     本轮补的三处（都在 exec.run **之前** 拒绝）：
//     prepareAction 的租户拒绝、BeginReconcile 的 ttl<=0、ResolveUncertainOwned
//     的空 holder。
package sqliteadapter

import (
	"context"
	"testing"
	"time"

	"github.com/oekit/pkg/errors"

	"github.com/yi-nology/goagent-kernel/checkpoint"
	"github.com/yi-nology/goagent-kernel/eventbus"
	"github.com/yi-nology/goagent-kernel/sideeffect"
	"github.com/yi-nology/goagent-kernel/taskboard"
	"github.com/yi-nology/goagent-kernel/tenant"
)

// txTenCtx 批内用例的 ctx：5s 超时（回归要**快速红**，而不是挂在 60s 超时上）
// + 注入租户上下文。
//
// tenantID 为空 ⇒ 不注入租户（模拟「忘了注入上下文」的 fail-closed 场景）。
func txTenCtx(t *testing.T, tenantID string) context.Context {
	t.Helper()
	ctx := appCtx(t)
	if tenantID == "" {
		return ctx
	}
	return tenant.WithContext(ctx, tenant.Context{Tenant: tenantID})
}

// TestApplyTenantEffectFaceRejectsMismatchAndVoidsBatch 批内租户拒绝 ⇒ 整批作废。
//
// 覆盖源码行：sqliteadapter（v0.2.0 自 goagent-kernel 迁入）/effect.go 的 prepareAction ——
// resolveTenant 失败分支里的 `s.exec.fail(err)`（删掉这一行 ⇒ 本用例红）。
//
// 为什么这条必须有：批内的租户作用域面是这条登记点**唯一**可达的入口
// （批外面 failed==nil ⇒ 登记是空操作）⇒ 没有本用例，删掉它没有任何信号。
func TestApplyTenantEffectFaceRejectsMismatchAndVoidsBatch(t *testing.T) {
	ctx := txTenCtx(t, "t-a")
	s := openStore(t)

	err := s.Apply(ctx, func(tx *Tx) error {
		// 批内租户作用域的面：声明的租户与 ctx 不一致 ⇒ 拒绝（ErrTenantMismatch）。
		if _, _, _, ferr := tx.TenantEffects().Acquire(ctx,
			sideeffect.Action{Key: "k-mismatch", Resource: "r-mismatch", Tenant: "t-b"},
			time.Minute); ferr == nil {
			return errors.New("声明租户与 ctx 不一致必须被拒")
		}
		// 继续写（这些写也必须随整批一起消失）。
		if _, _, _, ierr := tx.TenantEffects().Acquire(ctx,
			sideeffect.Action{Key: "k-ok", Resource: "r-ok"}, time.Minute); ierr != nil {
			return ierr
		}
		return nil
	})
	if err == nil {
		t.Fatal("内层失败被吞掉时 Apply 不得报成功（本批已作废）")
	}
	if !errors.Is(err, ErrTenantMismatch) {
		t.Fatalf("Apply 报出的应是租户不一致那条失败（ErrTenantMismatch），实得 %v", err)
	}
	var n int
	if err := s.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM gak_effect_action`).Scan(&n); err != nil {
		t.Fatalf("读动作行数失败: %v", err)
	}
	if n != 0 {
		t.Fatalf("租户拒绝未登记失败 ⇒ 整批仍提交了（半截事务），实得 %d 行", n)
	}
}

// TestApplyReconcilePreRunRejectionsVoidsBatch 对账侧两处「进 run 之前」的拒绝。
//
// 覆盖源码行：sqliteadapter（v0.2.0 自 goagent-kernel 迁入）/effect_reconcile.go 的
// BeginReconcile（ttl<=0 分支的 `s.exec.fail(err)`）与 ResolveUncertainOwned
// （空 holder 分支的 `s.exec.fail(err)`）。
//
// 为什么这条必须有：这两处都在 exec.run **之前**拒绝 ⇒ 不经 run 的登记分支
// ⇒ 显式登记是它们**唯一**的登记点；没有用例 ⇒ 删掉它没有任何信号。
func TestApplyReconcilePreRunRejectionsVoidsBatch(t *testing.T) {
	cases := []struct {
		name string
		fail func(ctx context.Context, tx *Tx) error
		want error
	}{
		{
			name: "BeginReconcile 的 ttl<=0",
			fail: func(ctx context.Context, tx *Tx) error {
				_, err := tx.Effects().BeginReconcile(ctx, sideeffect.Action{Key: "k-ttl"}, 0)
				return err
			},
			want: sideeffect.ErrInvalidTTL,
		},
		{
			name: "ResolveUncertainOwned 的空 holder",
			fail: func(ctx context.Context, tx *Tx) error {
				_, _, err := tx.Effects().ResolveUncertainOwned(ctx,
					sideeffect.Action{Key: "k-holder"}, sideeffect.Resolution{}, "")
				return err
			},
			want: sideeffect.ErrReconcileLost,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := appCtx(t)
			s := openStore(t)

			err := s.Apply(ctx, func(tx *Tx) error {
				_ = tc.fail(ctx, tx) // 故意吞掉内层的失败
				if _, ierr := tx.Events().Insert(ctx, eventbus.Event{SessionID: "s", Type: "t"}); ierr != nil {
					return ierr
				}
				return nil
			})
			if err == nil {
				t.Fatalf("%s：内层失败被吞掉时 Apply 不得报成功（本批已作废）", tc.name)
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("%s：Apply 报出的应是内层那条失败（%v），实得 %v", tc.name, tc.want, err)
			}
			wm, werr := s.Events().CommittedWatermark(ctx)
			if werr != nil {
				t.Fatalf("读水位线失败: %v", werr)
			}
			if wm != 0 {
				t.Fatalf("%s：未登记失败 ⇒ 整批仍提交了（半截事务），水位线=%d", tc.name, wm)
			}
		})
	}
}

// TestApplyTenantScopedBatchFacesUseContextTenant 批内租户形态真按 ctx 租户分区。
//
// 覆盖源码行：sqliteadapter（v0.2.0 自 goagent-kernel 迁入）/apply.go 的 Tx.TenantEffects /
// Tx.TenantEvents / Tx.TenantDownstream（tenantScoped=true + 继承批内失败槽）；
// event.go 的 publishInTx / readTenant；effect_sink.go 的 WriteFenced /
// resourceTenant。
func TestApplyTenantScopedBatchFacesUseContextTenant(t *testing.T) {
	ctx := txTenCtx(t, "t-a")
	s := openStore(t)

	err := s.Apply(ctx, func(tx *Tx) error {
		// 事件面：写进 ctx 租户的流。
		if _, err := tx.TenantEvents().Insert(ctx, eventbus.Event{SessionID: "s", Type: "t"}); err != nil {
			return err
		}
		// 栅栏面：写进 ctx 租户的记录。
		if err := tx.TenantDownstream().WriteFenced(ctx, "res", 3, nil); err != nil {
			return err
		}
		// 副作用面：写进 ctx 租户的命名空间。
		if _, _, _, err := tx.TenantEffects().Acquire(ctx,
			sideeffect.Action{Key: "k-tenant", Resource: "r-tenant"}, time.Minute); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Apply 失败: %v", err)
	}

	// 批外按租户读：看得见。
	wm, err := s.TenantEvents().CommittedWatermark(ctx)
	if err != nil || wm != 1 {
		t.Fatalf("批内租户写入应落在 ctx 租户的流上：wm=%d err=%v", wm, err)
	}
	seen, err := s.TenantDownstream().SeenFence(ctx, "res")
	if err != nil || seen != 3 {
		t.Fatalf("批内租户写入应落在 ctx 租户的栅栏上：seen=%d err=%v", seen, err)
	}
	var n int
	if err := s.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM gak_effect_action WHERE tenant = 't-a'`).Scan(&n); err != nil {
		t.Fatalf("读动作行数失败: %v", err)
	}
	if n != 1 {
		t.Fatalf("批内租户写入应落在 ctx 租户的命名空间，实得 %d 行", n)
	}

	// 批外**默认形态**读：看不到（默认流是 "" 命名空间 ⇒ 分区真的落到了租户列上）。
	if wm0, err := s.Events().CommittedWatermark(ctx); err != nil || wm0 != 0 {
		t.Fatalf("默认形态的事件面不得看到租户流的数据：wm=%d err=%v", wm0, err)
	}
	if seen0, err := s.Downstream().SeenFence(ctx, "res"); err != nil || seen0 != 0 {
		t.Fatalf("默认形态的栅栏不得看到租户的数据：seen=%d err=%v", seen0, err)
	}
}

// TestApplyDownstreamBatchFormJoinsSameTransaction 批内栅栏面不挂死且与批同生共死。
//
// 覆盖源码行：sqliteadapter（v0.2.0 自 goagent-kernel 迁入）/apply.go 的 Tx.Downstream（批内形态 +
// 继承批内失败槽）。
//
// 为什么这条必须有：批内调**门面**的 s.Downstream() 会自开事务 ⇒ 嵌套事务
// ⇒ 挂到 ctx 超时（不是干净报错）⇒ 批内形态是唯一不挂死的入口。
func TestApplyDownstreamBatchFormJoinsSameTransaction(t *testing.T) {
	ctx := appCtx(t)
	s := openStore(t)

	err := s.Apply(ctx, func(tx *Tx) error {
		if err := tx.Downstream().WriteFenced(ctx, "res", 2, nil); err != nil {
			return err
		}
		// 同一事务内读：走 tx ⇒ 看得见本事务刚写的未提交行。
		seen, err := tx.Downstream().SeenFence(ctx, "res")
		if err != nil {
			return err
		}
		if seen != 2 {
			return errors.New("批内栅栏面没有与批共用同一个事务")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Apply 失败（批内栅栏面没有与批共用同一个事务）: %v", err)
	}
	seen, err := s.Downstream().SeenFence(ctx, "res")
	if err != nil || seen != 2 {
		t.Fatalf("批内栅栏写入应随批提交：seen=%d err=%v", seen, err)
	}
}

// TestApplyTenantSinkMissingTenantVoidsBatch 栅栏面的租户拒绝 ⇒ 整批作废。
//
// 覆盖源码行：sqliteadapter（v0.2.0 自 goagent-kernel 迁入）/effect_sink.go 的 WriteFenced ——
// 入口租户解析失败分支里的 `d.exec.fail(err)`。
func TestApplyTenantSinkMissingTenantVoidsBatch(t *testing.T) {
	ctx := txTenCtx(t, "") // 故意不注入租户
	s := openStore(t)

	err := s.Apply(ctx, func(tx *Tx) error {
		if ferr := tx.TenantDownstream().WriteFenced(ctx, "res", 1, nil); ferr == nil {
			return errors.New("缺租户的栅栏写入必须被拒")
		}
		if _, ierr := tx.Events().Insert(ctx, eventbus.Event{SessionID: "s", Type: "t"}); ierr != nil {
			return ierr
		}
		return nil
	})
	if err == nil {
		t.Fatal("内层失败被吞掉时 Apply 不得报成功（本批已作废）")
	}
	if !errors.Is(err, tenant.ErrNoContext) {
		t.Fatalf("Apply 报出的应是缺租户那条失败（tenant.ErrNoContext），实得 %v", err)
	}
	wm, werr := s.Events().CommittedWatermark(ctx)
	if werr != nil {
		t.Fatalf("读水位线失败: %v", werr)
	}
	if wm != 0 {
		t.Fatalf("栅栏面未登记失败 ⇒ 整批仍提交了（半截事务），水位线=%d", wm)
	}
}

// TestApplyTenantShapeAlikeFacesStillJoinBatch 「无租户维度」面的租户形态仍是可用的批内形态。
//
// 覆盖源码行：sqliteadapter（v0.2.0 自 goagent-kernel 迁入）/apply.go 的 Tx.TenantTasks /
// Tx.TenantSnapshots。
//
// 任务面（gak_board_task）与快照面（gak_snapshot）**无 tenant 列**（与 PG 侧
// 一致）⇒ 它们的租户形态与默认形态同款；本用例钉的是「它们仍是真在批内的
// 可用形态」（不是恒等空壳）。
func TestApplyTenantShapeAlikeFacesStillJoinBatch(t *testing.T) {
	ctx := appCtx(t)
	s := openStore(t)
	s.Board().Seed(taskboard.Task{ID: "t1", State: taskboard.StateOpen})

	err := s.Apply(ctx, func(tx *Tx) error {
		if _, err := tx.TenantTasks().Claim(ctx, "t1", "agent-a",
			taskboard.ClaimRequest{TTL: time.Minute}); err != nil {
			return err
		}
		if err := tx.TenantSnapshots().SaveIfNewer(ctx,
			checkpoint.Snapshot{RunID: "run-tx", Seq: 1}); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Apply 失败: %v", err)
	}
	tasks, err := s.Board().List(ctx)
	if err != nil || len(tasks) != 1 || tasks[0].State != taskboard.StateClaimed {
		t.Fatalf("任务面未随批提交：%+v err=%v", tasks, err)
	}
	if _, err := s.Snapshots().Load(ctx, "run-tx"); err != nil {
		t.Fatalf("快照面未随批提交: %v", err)
	}
}
