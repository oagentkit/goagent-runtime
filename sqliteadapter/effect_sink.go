// effect_sink.go —— sideeffect.FencedSink 的 SQLite 参考实现（下游栅栏）。
//
// # 原子性要求（实现这条契约的唯一要点）
//
// 「比较与写入必须在**同一次原子操作**里完成」——本实现把它落成两件事：
//
//  1. **栅栏预占是一条带条件的 upsert**：INSERT … ON CONFLICT (tenant, resource)
//     DO UPDATE SET seen_fence = excluded.seen_fence
//     WHERE gak_downstream.seen_fence <= excluded.seen_fence。
//     影响行数为 0 ⇒ 令牌陈旧（ErrFenceStale），此刻**什么都没写**；
//     影响行数为 1 ⇒ 预占成功，且这行就是「比较 + 更新已见最大」本身。
//     （SQLite 的 ON CONFLICT … WHERE 与 PG 同款语义；方言差异只占位符。）
//  2. **写入体在同一事务里执行**：ctx 里带上了事务句柄（WithTx），写入体的
//     业务写入必须用 ExecWithin 走这个事务。写入体报错 ⇒ 整个事务回滚 ⇒
//     预占与已写的业务数据一起消失——**不得部分写**。
//
// # 反面教材（为什么不能三步写）
//
// 「先读已见最大 → 判断 → 再写入」三步各自独立提交时，两个持有者可以同时
// 读到同一个旧值、双双通过检查，然后先后写入——更旧的那次写入反而后落，
// 覆盖掉更新的。栅栏形同虚设。
//
// # 事务边界
//
// 写事务走写句柄（BEGIN IMMEDIATE ⇒ 先拿写锁）；独立读（SeenFence / Marker）
// 走**只读句柄**——写事务占用唯一连接期间从写句柄取连接会永久等待（实测挂到
// ctx 超时）。
package sqliteadapter

import (
	"context"
	"database/sql"

	"github.com/oekit/pkg/errors"

	"github.com/yi-nology/goagent-kernel/sideeffect"
)

// txKey 事务句柄在 ctx 里的键（私有：外部造不出带事务的 ctx）。
type txKey struct{}

// resolvedTenantKey 已解析租户在 ctx 里的键（私有：外部造不出带结论的 ctx）。
type resolvedTenantKey struct{}

// Downstream FencedSink 的 SQLite 参考实现。
type Downstream struct {
	// exec 执行位置：db 是写句柄，read 是只读句柄（独立读路径走它）。
	exec exec
	// tenantScoped 装配期声明（issue #275）：true ⇒ 资源键按 (tenant,
	// resource) 隔离，缺租户上下文拒绝。false ⇒ tenant ''（旧布局），
	// 资源字符串本身的作用域仍有效。
	tenantScoped bool
}

// 编译期断言：本实现满足下游栅栏契约。
var _ sideeffect.FencedSink = (*Downstream)(nil)

// Downstream 造下游栅栏（门面自带独立只读句柄 ⇒ 独立读不与写事务抢连接）。
func (s *Store) Downstream() *Downstream {
	return &Downstream{exec: s.execBase()}
}

// NewDownstream 构造下游栅栏（db 归调用方管理）。
//
// ⚠ 同 NewEffectStore：独立构造器开不出第二个只读句柄 ⇒ 独立读路径与写
// 句柄同源。需要「读不阻塞写」的宿主请用 Store.Downstream()。
func NewDownstream(db *sql.DB) *Downstream {
	return &Downstream{exec: execOf(db)}
}

// WithTx 把事务句柄放进 ctx（下游栅栏内部用；写入体拿到的是它的产物）。
func WithTx(ctx context.Context, tx *sql.Tx) context.Context {
	return context.WithValue(ctx, txKey{}, tx)
}

// TxFromContext 返回写入体所在的事务句柄。
//
// 写入体**必须**通过它（或 ExecWithin）执行业务写入——绕开事务自己开连接
// 写入的数据不受栅栏保护，也无法在失败时一起回滚。
func TxFromContext(ctx context.Context) (*sql.Tx, bool) {
	tx, ok := ctx.Value(txKey{}).(*sql.Tx)
	return tx, ok
}

// ExecWithin 在写入体所在的事务里执行一条写入。
//
// 不在事务里（ctx 没带事务句柄）时显式报错：静默落到事务外等于绕开栅栏。
func ExecWithin(ctx context.Context, query string, args ...any) (int64, error) {
	tx, ok := TxFromContext(ctx)
	if !ok {
		return 0, errors.New("sqliteadapter: 业务写入必须从 WriteFenced 的写入体内执行（用 ExecWithin 走同一事务）")
	}
	res, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, errors.Wrap(err, "sqliteadapter: 业务写入失败")
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// WriteFenced 在资源上执行一次带栅栏令牌的写入。
//
// 语义（契约逐条对应）：
//  1. presented 小于已见最大 → 不执行写入，返回 ErrFenceStale（此刻零写入）；
//  2. 否则执行写入体，并把已见最大更新为 presented（**等于**也放行——
//     同一代持有者的多次写入合法，如分批提交）；
//  3. 第 1 条的判定与第 2 条的写入（含令牌更新）在同一个事务里。
func (d *Downstream) WriteFenced(ctx context.Context, resource string, presented sideeffect.Fence, write func(ctx context.Context) error) error {
	// 租户解析在**入口**发生一次（后续 SQL 一律用这一份结论）。
	tenantID, err := resolveTenant(ctx, d.tenantScoped, "")
	if err != nil {
		// 失败登记：拒绝发生在 exec.run 之外 ⇒ 不登记的话调用方吞掉它时
		// 整批仍会提交（「失败即作废整批」对这条路径是假的）。
		d.exec.fail(err)
		return err
	}
	return d.exec.run(ctx, func(tx *sql.Tx) error {
		// 一条带条件的 upsert 完成比较与预占：
		//   新资源直接以 presented 建行（首次写入恒通过）；
		//   既有资源仅在 seen_fence <= presented 时更新（小于即陈旧）。
		// (tenant, resource) 是联合键：跨租户同名资源各有各的栅栏。
		res, err := tx.ExecContext(ctx,
			`INSERT INTO gak_downstream (tenant, resource, seen_fence, value) VALUES (?, ?, ?, '')
			 ON CONFLICT (tenant, resource) DO UPDATE SET seen_fence = excluded.seen_fence
			 WHERE gak_downstream.seen_fence <= excluded.seen_fence`,
			tenantID, resource, int64(presented))
		if err != nil {
			return errors.Wrap(err, "sqliteadapter: 栅栏预占失败", "resource", resource)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return errors.Wrap(sideeffect.ErrFenceStale,
				"栅栏令牌已不是最新代（陈旧写入被拒）",
				"resource", resource, "presented", uint64(presented))
		}
		if write != nil {
			// 写入体在同一事务里执行：报错 ⇒ 整体回滚 ⇒ 预占与已写的业务
			// 数据一起消失（不得部分写）。
			// 入口的租户结论带进写入体（写入体不再二次解析 ⇒ 同一份结论）。
			if err := write(withResolvedTenant(WithTx(ctx, tx), tenantID)); err != nil {
				return err
			}
		}
		return nil
	})
}

// SeenFence 读资源已见过的最大令牌（无记录返回 0）。
//
// 独立读路径 ⇒ 走只读句柄（写事务占用写句柄唯一连接时，从写句柄取连接会
// 永久等待）。读的是**已提交快照**（WAL 下的读者用自己的连接看已提交数据）。
func (d *Downstream) SeenFence(ctx context.Context, resource string) (sideeffect.Fence, error) {
	tenantID, err := d.resourceTenant(ctx)
	if err != nil {
		return 0, err
	}
	q, err := d.exec.reader()
	if err != nil {
		return 0, err
	}
	var seen sql.NullInt64
	if err := q.QueryRowContext(ctx,
		`SELECT seen_fence FROM gak_downstream WHERE tenant = ? AND resource = ?`,
		tenantID, resource).Scan(&seen); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, errors.Wrap(err, "sqliteadapter: 读已见最大令牌失败", "resource", resource)
	}
	return sideeffect.Fence(seen.Int64), nil
}

// withResolvedTenant 把入口解析出的租户结论带进写入体（私有键）。
func withResolvedTenant(ctx context.Context, tenantID string) context.Context {
	return context.WithValue(ctx, resolvedTenantKey{}, tenantID)
}

// resolvedTenant 取写入体 ctx 里带的那份结论（没有 ⇒ ok=false）。
func resolvedTenant(ctx context.Context) (string, bool) {
	t, ok := ctx.Value(resolvedTenantKey{}).(string)
	return t, ok
}

// resourceTenant 资源面的租户解析：写入体里的 ctx 已带入口结论（不做第二次
// 解析），外部调用走同一套 resolveTenant。
func (d *Downstream) resourceTenant(ctx context.Context) (string, error) {
	if t, ok := resolvedTenant(ctx); ok {
		return t, nil
	}
	return resolveTenant(ctx, d.tenantScoped, "")
}

// WriteMarker 在写入体里写一个参考业务标记（值追加 'm'）。
//
// 它是参考实现的「业务写入」占位：宿主的真实业务写入走同一条路
// （ExecWithin，同一事务）。供共享套件判「不得部分写」。
func (d *Downstream) WriteMarker(ctx context.Context, resource string) error {
	tenantID, err := d.resourceTenant(ctx)
	if err != nil {
		return err
	}
	_, err = ExecWithin(ctx,
		`UPDATE gak_downstream SET value = value || 'm' WHERE tenant = ? AND resource = ?`,
		tenantID, resource)
	return err
}

// Marker 读已落盘的参考业务标记（无记录返回空串）。
func (d *Downstream) Marker(ctx context.Context, resource string) (string, error) {
	tenantID, err := d.resourceTenant(ctx)
	if err != nil {
		return "", err
	}
	q, err := d.exec.reader()
	if err != nil {
		return "", err
	}
	var value sql.NullString
	if err := q.QueryRowContext(ctx,
		`SELECT value FROM gak_downstream WHERE tenant = ? AND resource = ?`,
		tenantID, resource).Scan(&value); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", errors.Wrap(err, "sqliteadapter: 读参考业务标记失败", "resource", resource)
	}
	return value.String, nil
}
