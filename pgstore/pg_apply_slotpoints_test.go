// pg_apply_slotpoints_test.go —— 失败槽**逐个登记点**的判据（评审 I-1）。
//
// # 为什么一登记点一条判据
//
// 「失败即作废整批」只有一个承诺：**批内任一面失败，整批都不提交**。
// 失败槽的每个登记点都是这个承诺的一根承重柱：拆掉一个 ⇒ 对应路径上的失败
// 被调用方吞掉时，整批仍会提交 ⇒ 跨面半截状态 ⇒ 而当时**全套测试仍然全绿**
// （判据缺失 ⇒ 门禁绿 = 没判）。
//
// ⇒ 每个登记点**独立**一条判据：登记点被拆 ⇒ 只有它那条用例变红。
// 「一个用例覆盖多个面」**不算**逐点覆盖——拆掉其中一个登记点时那条用例仍可能
// 绿 ⇒ 判据是假的。
//
// # 判据形态（统一）
//
// 批内触发**该点**的失败 → **吞掉**它（回调返回 nil）⇒
//
//	① Apply 必须报出那条失败（errors.Is 判得到）；
//	② 批内那次成功写**不得**落库（整批回滚的见证——不是「不报错」那种空断言）。
//
// ⚠ 本文件的失败一律是**非 SQL 错误**（租户拒绝 / 入参校验拒绝 / 事务句柄
// 缺失）：PG 里一条语句失败会让整个事务进入中止态 ⇒ 那种形态下回滚是数据库
// 兜的，不是失败槽兜的 ⇒ 判不到失败槽的承重。
package pgstore

import (
	"context"
	"testing"
	"time"

	"github.com/oekit/pkg/errors"

	"github.com/yi-nology/goagent-kernel/eventbus"
	"github.com/yi-nology/goagent-kernel/sideeffect"
	"github.com/yi-nology/goagent-kernel/tenant"
)

// assertSlotPointVoidsBatch 跑一次「批内触发某个登记点的失败 → 吞掉它 ⇒ 整批
// 仍回滚」，并断言**非空**（Apply 报出那条失败 + 批内的成功写没落库）。
func assertSlotPointVoidsBatch(t *testing.T, s *Store, ctx context.Context,
	want error, fail func(ctx context.Context, s *Store) error) {
	t.Helper()
	err := s.Apply(ctx, func(ctx context.Context) error {
		// 批内的一次成功写（见证）：它必须随整批一起消失。
		if _, ierr := s.Events().Insert(ctx, eventbus.Event{SessionID: "s", Type: "t"}); ierr != nil {
			return ierr
		}
		// 内层失败：错误被吞掉（调用方假装没看见，继续走）。
		_ = fail(ctx, s)
		return nil
	})
	if err == nil {
		t.Fatal("内层失败被吞掉时 Apply 不得报成功（本批已作废）")
	}
	if !errors.Is(err, want) {
		t.Fatalf("Apply 必须报出内层那条失败（%v），实得 %v", want, err)
	}
	wm, werr := s.Events().CommittedWatermark(ctx)
	if werr != nil {
		t.Fatalf("读水位线失败: %v", werr)
	}
	if wm != 0 {
		t.Fatalf("失败被吞掉 ⇒ 整批必须回滚（事件水位线应为 0），实得 %d", wm)
	}
}

// TestApplySlotPointEffectTenantReject 登记点：副作用面的**租户解析拒绝**
// （prepareAction，effect.go）。
//
// 拆掉这处登记 ⇒ 租户拒绝被吞掉时整批仍提交 ⇒ 半截状态 ⇒ 本用例必须红。
func TestApplySlotPointEffectTenantReject(t *testing.T) {
	dsn := requireDSN(t)
	ctx := applyCtx(t)
	s := freshStore(t, dsn)

	assertSlotPointVoidsBatch(t, s, ctx, tenant.ErrNoContext,
		func(ctx context.Context, s *Store) error {
			// 租户作用域形态 + 缺租户上下文 ⇒ 入口拒绝（不进事务）。
			_, _, _, err := s.TenantEffects().Acquire(ctx,
				sideeffect.Action{Key: "k-slot-a", Resource: "res-slot-a"}, time.Minute)
			return err
		})
}

// TestApplySlotPointReconcileEmptyHolder 登记点：对账裁决的**空凭据拒绝**
// （ResolveUncertainOwned，effect_reconcile.go）。
//
// 拆掉这处登记 ⇒ 空凭据拒绝被吞掉时整批仍提交 ⇒ 本用例必须红。
func TestApplySlotPointReconcileEmptyHolder(t *testing.T) {
	dsn := requireDSN(t)
	ctx := applyCtx(t)
	s := freshStore(t, dsn)

	assertSlotPointVoidsBatch(t, s, ctx, sideeffect.ErrReconcileLost,
		func(ctx context.Context, s *Store) error {
			_, _, err := s.Effects().ResolveUncertainOwned(ctx,
				sideeffect.Action{Key: "k-slot-b", Resource: "res-slot-b"},
				sideeffect.Resolution{}, "")
			return err
		})
}

// TestApplySlotPointReconcileNonPositiveTTL 登记点：对账认领的**非正 TTL 拒绝**
// （BeginReconcile，effect_reconcile.go）。
//
// 拆掉这处登记 ⇒ TTL 拒绝被吞掉时整批仍提交 ⇒ 本用例必须红。
func TestApplySlotPointReconcileNonPositiveTTL(t *testing.T) {
	dsn := requireDSN(t)
	ctx := applyCtx(t)
	s := freshStore(t, dsn)

	assertSlotPointVoidsBatch(t, s, ctx, sideeffect.ErrInvalidTTL,
		func(ctx context.Context, s *Store) error {
			_, err := s.Effects().BeginReconcile(ctx,
				sideeffect.Action{Key: "k-slot-c", Resource: "res-slot-c"}, 0)
			return err
		})
}

// TestApplySlotPointSinkTenantReject 登记点：下游栅栏面的**租户解析拒绝**
// （WriteFenced，sink.go）。
//
// ⚠ 这是**栅栏面**自己的面向失败判据（此前栅栏面一条都没有）：栅栏面是
// 「不得部分写」这条契约的承载面，它的失败若不作废整批 ⇒ 批内出现「栅栏预占
// 落了、别的面没落」的半截状态。
//
// 拆掉这处登记 ⇒ 栅栏面的租户拒绝被吞掉时整批仍提交 ⇒ 本用例必须红。
func TestApplySlotPointSinkTenantReject(t *testing.T) {
	dsn := requireDSN(t)
	ctx := applyCtx(t)
	s := freshStore(t, dsn)

	assertSlotPointVoidsBatch(t, s, ctx, tenant.ErrNoContext,
		func(ctx context.Context, s *Store) error {
			return s.TenantDownstream().WriteFenced(ctx, "res-slot-d", 1, nil)
		})
}

// TestApplySlotPointSinkMarkerTenantReject 登记点：业务标记写的**租户解析拒绝**
// （WriteMarker，sink.go）。
//
// 拆掉这处登记 ⇒ 标记写的租户拒绝被吞掉时整批仍提交 ⇒ 本用例必须红。
func TestApplySlotPointSinkMarkerTenantReject(t *testing.T) {
	dsn := requireDSN(t)
	ctx := applyCtx(t)
	s := freshStore(t, dsn)

	assertSlotPointVoidsBatch(t, s, ctx, tenant.ErrNoContext,
		func(ctx context.Context, s *Store) error {
			return s.TenantDownstream().WriteMarker(ctx, "res-slot-e")
		})
}

// TestApplySlotPointSinkMarkerOutsideTx 登记点：业务标记写在**写入体之外**的
// 拒绝（WriteMarker → ExecWithin，sink.go）。
//
// 这条路径的触发条件是「ctx 里没有事务句柄」：批内拿不到本批的 tx 时（丢了
// 句柄 / 自造 ctx）就会走到 ⇒ 此时标记写必须**被拒**（绕开事务自己写 = 绕开
// 栅栏 + 独立提交），且该拒绝要**登记进失败槽** ⇒ 吞掉它也照样整批作废。
//
// 拆掉这处登记 ⇒ 拒绝被吞掉时整批仍提交 ⇒ 本用例必须红。
func TestApplySlotPointSinkMarkerOutsideTx(t *testing.T) {
	dsn := requireDSN(t)
	ctx := applyCtx(t)
	s := freshStore(t, dsn)

	err := s.Apply(ctx, func(ctx context.Context) error {
		if _, ierr := s.Events().Insert(ctx, eventbus.Event{SessionID: "s", Type: "t"}); ierr != nil {
			return ierr
		}
		// 自造一个「有批次标记、没有事务句柄」的 ctx（模拟调用方把句柄丢了）。
		stripped := withBatch(context.Background(), batchFrom(ctx))
		if werr := s.Downstream().WriteMarker(stripped, "res-slot-f"); werr == nil {
			return errors.New("批内拿不到事务句柄时，业务标记写必须被拒（绕开事务 = 绕开栅栏）")
		}
		return nil
	})
	if err == nil {
		t.Fatal("批内自开事务被拒时，Apply 必须报错（整批已作废）")
	}
	wm, werr := s.Events().CommittedWatermark(ctx)
	if werr != nil {
		t.Fatalf("读水位线失败: %v", werr)
	}
	if wm != 0 {
		t.Fatalf("失败被吞掉 ⇒ 整批必须回滚（事件水位线应为 0），实得 %d", wm)
	}
}
