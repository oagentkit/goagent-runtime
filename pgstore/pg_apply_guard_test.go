// pg_apply_guard_test.go —— 跨面 Apply 的**护栏**判据（PG 侧）。
//
// 护栏与主判据分开：主判据（pg_apply_test.go）判「同一次提交 + 失败即作废
// 整批」；本文件判「哪些形态必须被**拒绝**」。
//
// # 为什么 PG 侧需要护栏（与 SQLite 侧同因不同果）
//
// SQLite 侧嵌套事务 = 外层占着唯一连接、内层永远等 ⇒ 挂到 ctx 超时（响亮
// 失败）。PG 侧 pool 有多条连接 ⇒ 内层拿到**另一条连接** ⇒ **独立提交** ⇒
// 外层回滚时内层已落库 ⇒ 同样是「一半已提交」，而且**一声不吭**。
//
// ⇒ PG 侧的护栏把三处「会静默独立提交」的口子改成响亮拒绝：
//   - 批内拿不到本批事务句柄时，面**不得**自开事务；
//   - Apply **不可重入**（嵌套 Apply 会从 pool 拿另一条连接开事务）；
//   - 空回调是编程错误，不得静默提交一个空批。
package pgstore

import (
	"context"
	"reflect"
	"testing"

	"github.com/oekit/pkg/errors"

	"github.com/yi-nology/goagent-kernel/eventbus"
)

// TestApplyReturnsCallbackErrorVerbatim 回调的错误**原样**传出（不被包装）。
//
// 判据：错误身份必须保持（等值也断）⇒ 调用方能按自己的哨兵分流。
func TestApplyReturnsCallbackErrorVerbatim(t *testing.T) {
	dsn := requireDSN(t)
	ctx := applyCtx(t)
	s := freshStore(t, dsn)

	sentinel := errors.New("故意失败")
	err := s.Apply(ctx, func(ctx context.Context) error { return sentinel })
	if err != sentinel {
		t.Fatalf("Apply 必须原样传出回调的错误（不得包装），实得 %v", err)
	}
}

// TestApplyRejectsNilCallback 空回调是编程错误，显式报错（不静默提交空批）。
func TestApplyRejectsNilCallback(t *testing.T) {
	dsn := requireDSN(t)
	ctx := applyCtx(t)
	s := freshStore(t, dsn)
	if err := s.Apply(ctx, nil); err == nil {
		t.Fatal("空回调必须报错（不得静默提交空批）")
	}
}

// TestApplyRejectsNestedApply 嵌套 Apply 必须被拒。
//
// PG 侧嵌套 Apply 会从 pool 拿**另一条连接**开事务 ⇒ 内层独立提交 ⇒ 外层
// 回滚时内层已落库 ⇒ 跨面原子性静默失效（且没有报错）。
//
// 判据两条：① 内层被拒（响亮）；② 内层**没有**独立提交（事件行不得留下）。
func TestApplyRejectsNestedApply(t *testing.T) {
	dsn := requireDSN(t)
	ctx := applyCtx(t)
	s := freshStore(t, dsn)

	err := s.Apply(ctx, func(ctx context.Context) error {
		inner := s.Apply(ctx, func(ctx context.Context) error {
			_, e := s.Events().Insert(ctx, eventbus.Event{SessionID: "s-inner", Type: "t"})
			return e
		})
		if inner == nil {
			return errors.New("嵌套 Apply 必须被拒（内层会独立提交 ⇒ 外层回滚时内层已落库）")
		}
		// 内层的拒绝被吞掉 ⇒ 外层批次必须一并作废（失败槽登记了它）。
		return nil
	})
	if err == nil {
		t.Fatal("嵌套 Apply 被拒时，外层批次必须作废（不得报成功）")
	}
	wm, werr := s.Events().CommittedWatermark(ctx)
	if werr != nil {
		t.Fatalf("读水位线失败: %v", werr)
	}
	if wm != 0 {
		t.Fatalf("嵌套 Apply 独立提交了（跨面原子性已破），水位线=%d", wm)
	}
}

// TestApplyRefusesFaceSelfOpenedTransaction 批内拿不到本批事务句柄时，面
// **不得**自开事务。
//
// 自开事务拿到的是 pool 里的**另一条连接** ⇒ 独立提交 ⇒ 外层回滚时它已落库
// ⇒ 跨面原子性静默失效。⇒ 必须响亮拒绝。
//
// 判据两条：① 面报错（不自开事务）；② 事件行不得留下（没有独立提交）。
func TestApplyRefusesFaceSelfOpenedTransaction(t *testing.T) {
	dsn := requireDSN(t)
	ctx := applyCtx(t)
	s := freshStore(t, dsn)

	err := s.Apply(ctx, func(ctx context.Context) error {
		// 造一个「有批次标记、没有事务句柄」的 ctx（模拟调用方把事务句柄丢了）。
		stripped := withBatch(context.Background(), batchFrom(ctx))
		if _, ierr := s.Events().Insert(stripped,
			eventbus.Event{SessionID: "s", Type: "t"}); ierr == nil {
			return errors.New("批内没有事务句柄时面必须报错（自开事务会独立提交 ⇒ 破坏跨面原子性）")
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
		t.Fatalf("面自开事务并独立提交了（跨面原子性已破），水位线=%d", wm)
	}
}

// TestTenantTailCursorStoreIsTenantScoped 租户作用域的游标面必须自报
// 「声明了租户作用域」（类型即声明）。
//
// 用途（与 SQLite 侧同款）：把「忘记装配」变成**启动期失败**，而不是运行期的
// 跨租户可见（宿主对两种游标面用同一段装配期断言）。
//
// ⚠ 默认型游标面**没有**该方法（它不解析租户 ⇒ 无从声明）——两侧现状一致。
func TestTenantTailCursorStoreIsTenantScoped(t *testing.T) {
	dsn := requireDSN(t)
	s := freshStore(t, dsn)

	c := NewTenantTailCursorStore(s.pool, "consumer-1")
	if !c.TenantScoped() {
		t.Fatal("租户作用域的游标面必须自报已声明租户作用域（类型即声明）")
	}
	if _, ok := reflect.TypeOf(&TailCursorStore{}).MethodByName("TenantScoped"); ok {
		t.Fatal("默认型游标面不得有 TenantScoped（它不解析租户，无从声明）")
	}
}
