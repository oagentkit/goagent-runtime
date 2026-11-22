// sink.go —— sideeffect.FencedSink 的 PostgreSQL 参考实现（下游栅栏适配）。
//
// # 原子性要求（实现这条契约的唯一要点）
//
// 「比较与写入必须在**同一次原子操作**里完成」——本实现把它落成两件事：
//
//  1. **栅栏预占是一条带条件的 upsert**：INSERT … ON CONFLICT (resource)
//     DO UPDATE SET seen_fence = EXCLUDED.seen_fence WHERE seen_fence <= presented。
//     影响行数为 0 ⇒ 令牌陈旧（ErrFenceStale），此刻**什么都没写**；
//     影响行数为 1 ⇒ 预占成功，且这行就是「比较 + 更新已见最大」本身。
//  2. **写入体在同一事务里执行**：ctx 里带上了事务句柄（TxFromContext），
//     写入体的业务写入必须用 ExecWithin / QueryRowWithin 走这个事务。
//     写入体报错 ⇒ 整个事务回滚 ⇒ 预占与已写的业务数据一起消失——
//     **不得部分写**。
//
// # 反面教材（为什么不能三步写）
//
// 「先读已见最大 → 判断 → 再写入」三步各自独立提交时，两个持有者可以同时
// 读到同一个旧值、双双通过检查，然后先后写入——更旧的那次写入反而后落，
// 覆盖掉更新的。栅栏形同虚设。这条失败形态在 sideeffect 包内有确定性用例
// （TestNonAtomicSinkIsCaught）与共享套件的并发场景双向钉住。
package pgstore

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/oekit/pkg/errors"

	"github.com/yi-nology/goagent-kernel/sideeffect"
)

// txKey 事务句柄在 ctx 里的键（私有：外部造不出带事务的 ctx）。
type txKey struct{}

// Downstream FencedSink 的 PostgreSQL 参考实现。
type Downstream struct {
	pool *pgxpool.Pool
	// tenantScoped 装配期声明（issue #275）：true ⇒ 资源键按 (tenant,
	// resource) 隔离，缺租户上下文拒绝。false ⇒ tenant ''（旧布局），
	// 资源字符串本身的作用域（ActionKeyBuilder.Resource）仍有效。
	tenantScoped bool
}

// NewDownstream 构造下游栅栏（pool 归调用方管理）。
func NewDownstream(pool *pgxpool.Pool) *Downstream {
	return &Downstream{pool: pool}
}

// NewTenantDownstream 构造**租户作用域**的下游栅栏（issue #275）。
//
// 写入 / 读取都从 ctx 取租户；缺租户 ⇒ tenant.ErrNoContext。同一 resource
// 字符串在两个租户下是两条独立的已见令牌记录——跨租户不会互相抬栅栏。
func NewTenantDownstream(pool *pgxpool.Pool) *Downstream {
	return &Downstream{pool: pool, tenantScoped: true}
}

// TenantScoped 报告本实例是否声明了租户作用域（装配期自检用）。
func (d *Downstream) TenantScoped() bool { return d.tenantScoped }

// tenantKey 已解析租户在 ctx 里的键：WriteFenced 把入口解析结果带进写入体，
// WriteMarker / Marker 在写入体内读同一份结论（不再二次解析）。
type tenantKey struct{}

func withResolvedTenant(ctx context.Context, tenantID string) context.Context {
	return context.WithValue(ctx, tenantKey{}, tenantID)
}

func resolvedTenant(ctx context.Context) (string, bool) {
	t, ok := ctx.Value(tenantKey{}).(string)
	return t, ok
}

// TxFromContext 返回写入体所在的事务句柄。
//
// 写入体**必须**通过它（或 ExecWithin / QueryRowWithin）执行业务写入——
// 绕开事务自己开连接写入的数据不受栅栏保护，也无法在失败时一起回滚。
func TxFromContext(ctx context.Context) (pgx.Tx, bool) {
	tx, ok := ctx.Value(txKey{}).(pgx.Tx)
	return tx, ok
}

// WithTx 把事务句柄放进 ctx（Downstream 内部用；写入体拿到的是它的产物）。
func WithTx(ctx context.Context, tx pgx.Tx) context.Context {
	return context.WithValue(ctx, txKey{}, tx)
}

// ExecWithin 在写入体所在的事务里执行一条写入。
//
// 不在事务里（ctx 没带事务句柄）时显式报错：静默落到事务外等于绕开栅栏。
func ExecWithin(ctx context.Context, sql string, args ...any) (int64, error) {
	tx, ok := TxFromContext(ctx)
	if !ok {
		return 0, errors.New("pgstore: 业务写入必须从 WriteFenced 的写入体内执行（用 ExecWithin 走同一事务）")
	}
	tag, err := tx.Exec(ctx, sql, args...)
	if err != nil {
		return 0, errors.Wrap(err, "pgstore: 业务写入失败")
	}
	return tag.RowsAffected(), nil
}

// WriteFenced 在资源上执行一次带栅栏令牌的写入。
//
// 语义（契约逐条对应）：
//  1. presented 小于已见最大 → 不执行写入，返回 ErrFenceStale（此刻零写入）；
//  2. 否则执行写入体，并把已见最大更新为 presented（**等于**也放行——
//     同一代持有者的多次写入合法，如分批提交）；
//  3. 第 1 条的判定与第 2 条的写入（含令牌更新）在同一个事务里。
func (d *Downstream) WriteFenced(ctx context.Context, resource string, presented sideeffect.Fence, write func(ctx context.Context) error) error {
	tenantID, err := resolveTenant(ctx, d.tenantScoped, "")
	if err != nil {
		// 批内租户拒绝 ⇒ 登记失败：这条拒绝发生在**进事务之前** ⇒ 不登记的话，
		// 「失败即作废整批」对栅栏面是假的。
		failBatch(ctx, err)
		return err
	}
	_, err = withinTx(ctx, d.pool, func(tx pgx.Tx) (bool, error) {
		// 一条带条件的 upsert 完成比较与预占：
		//   新资源直接以 presented 建行（首次写入恒通过）；
		//   既有资源仅在 seen_fence <= presented 时更新（小于即陈旧）。
		// (tenant, resource) 是联合键：跨租户同名资源各有各的栅栏。
		tag, err := tx.Exec(ctx,
			`INSERT INTO pgref_downstream (tenant, resource, seen_fence, value) VALUES ($1, $2, $3, '')
			 ON CONFLICT (tenant, resource) DO UPDATE SET seen_fence = EXCLUDED.seen_fence
			 WHERE pgref_downstream.seen_fence <= EXCLUDED.seen_fence`,
			tenantID, resource, int64(presented))
		if err != nil {
			return false, errors.Wrap(err, "pgstore: 栅栏预占失败", "resource", resource)
		}
		if tag.RowsAffected() == 0 {
			return false, errors.Wrap(sideeffect.ErrFenceStale,
				"栅栏令牌已不是最新代（陈旧写入被拒）",
				"resource", resource, "presented", uint64(presented))
		}
		if write != nil {
			if err := write(withResolvedTenant(WithTx(ctx, tx), tenantID)); err != nil {
				// 整体回滚：预占与写入体已写的部分一起消失（不得部分写）。
				return false, err
			}
		}
		return false, nil
	})
	return err
}

// SeenFence 读资源已见过的最大令牌（无记录返回 0）。
func (d *Downstream) SeenFence(ctx context.Context, resource string) (sideeffect.Fence, error) {
	tenantID, err := d.resourceTenant(ctx)
	if err != nil {
		return 0, err
	}
	var seen *int64
	if err := d.pool.QueryRow(ctx,
		`SELECT seen_fence FROM pgref_downstream WHERE tenant = $1 AND resource = $2`,
		tenantID, resource).Scan(&seen); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, nil
		}
		return 0, errors.Wrap(err, "pgstore: 读已见最大令牌失败", "resource", resource)
	}
	if seen == nil {
		return 0, nil
	}
	return sideeffect.Fence(*seen), nil
}

// resourceTenant 资源面的租户解析：写入体里的 ctx 已带入口结论（不做
// 第二次解析），外部调用走同一套 resolveTenant。
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
		failBatch(ctx, err)
		return err
	}
	_, err = ExecWithin(ctx,
		`UPDATE pgref_downstream SET value = value || 'm' WHERE tenant = $1 AND resource = $2`,
		tenantID, resource)
	if err != nil {
		// 「不在写入体内」这类拒绝也发生在**进事务之前** ⇒ 同样登记。
		failBatch(ctx, err)
	}
	return err
}

// Marker 读已落盘的参考业务标记。
func (d *Downstream) Marker(ctx context.Context, resource string) (string, error) {
	tenantID, err := d.resourceTenant(ctx)
	if err != nil {
		return "", err
	}
	var value *string
	if err := d.pool.QueryRow(ctx,
		`SELECT value FROM pgref_downstream WHERE tenant = $1 AND resource = $2`,
		tenantID, resource).Scan(&value); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", nil
		}
		return "", errors.Wrap(err, "pgstore: 读参考业务标记失败", "resource", resource)
	}
	if value == nil {
		return "", nil
	}
	return *value, nil
}
