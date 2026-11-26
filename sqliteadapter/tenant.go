// tenant.go —— 租户作用域：判据入口、装配面与自检口（对位 pgstore（v0.2.0 自 goagent-kernel 迁入）/tenant.go）。
//
// # 两种形态，一个判据入口
//
//   - **默认（旧布局）**：构造器 NewEffectStore / NewEventStore / NewDownstream
//     不声明租户作用域，租户列取动作 / 事件自带的 Tenant（空 = 单命名空间）。
//     既有单租户部署逐字不变。
//   - **租户作用域**：NewTenantEffectStore / NewTenantEventStore /
//     NewTenantDownstream / NewTenantTailCursorStore 在装配期声明。此后每次
//     调用都从 ctx 取租户：缺租户 ⇒ tenant.ErrNoContext（fail-closed，
//     **不是**「不限」）；动作 / 事件自带的 Tenant 若与 ctx 不一致 ⇒
//     ErrTenantMismatch（同一份写入不能有两个租户结论）。
//
// # 解析只发生在**入口**
//
// 公共方法先把作用域解析成具体租户，再交给 SQL。下游 SQL 一律 (tenant, 业务键)
// 联合定位，不存在「先按键查、再补租户过滤」的两步窗口。
//
// # 哪些面做租户分区
//
// 三张副作用表（gak_effect_action / gak_effect_resource / gak_downstream）、
// 事件面（gak_event / gak_event_watermark）、栅栏面、游标面。
//
// ⚠ **board 面不做**租户分区：gak_board_task 无 tenant 列（与 PG 侧
// pgref_board_task 一致）⇒ 两侧同构，不给 board 表加列（加了反而与 PG 漂移）。
//
// # 对位关系
//
// 形态对位 PG 侧（同名构造器 / 同名自检口 / 同款三段判定），**判据逻辑两包
// 各写一份**——共用会把两个引擎耦在一起，靠同一套契约套件对齐。
package sqliteadapter

import (
	"context"
	"database/sql"

	"github.com/oekit/pkg/errors"

	"github.com/yi-nology/goagent-kernel/tenant"
)

// ErrTenantMismatch 声明的租户与上下文租户不一致（同一次操作两个租户结论）。
//
// 处置：修声明。调用方要么让 Action.Tenant / Event.Tenant 与
// tenant.Context.Tenant 一致，要么不要同时给两份；存储拒绝猜测哪一份是
// 对的——猜错的方向是跨租户串数据。
//
// ⚠ 与 PG 侧的同名哨兵是**两个**（两包各判一套，不共用）。
var ErrTenantMismatch = errors.New("sqliteadapter: 声明的租户与上下文租户不一致")

// resolveTenant 解析本次操作的存储租户（三段，判定顺序不可改）。
//
//  1. scoped=false ⇒ 返回 declared（空 = 旧布局 / 单租户命名空间）；
//  2. scoped=true ⇒ ctx 必须带租户（tenant.Require，缺 ⇒ tenant.ErrNoContext
//     ——fail-closed，不回落默认命名空间）；
//  3. scoped=true 且 declared 非空且与 ctx 不一致 ⇒ ErrTenantMismatch。
//
// ⚠ 第三个参数是「**声明的租户**」，不是「回落值」：scoped=false 时它才是
// 回落值（默认形态取动作/事件自带的 Tenant）。
func resolveTenant(ctx context.Context, scoped bool, declared string) (string, error) {
	if !scoped {
		return declared, nil
	}
	c, err := tenant.Require(ctx)
	if err != nil {
		return "", err
	}
	if declared != "" && declared != c.Tenant {
		return "", errors.Wrap(ErrTenantMismatch,
			"写入声明的租户与上下文租户不一致（拒绝猜测）",
			"declared", declared, "context", c.Tenant)
	}
	return c.Tenant, nil
}

// NewTenantEffectStore 构造**租户作用域**的副作用闸存储（对位 PG 侧同名构造器）。
//
// 所有入口从 ctx 取租户（tenant.Require），缺租户 ⇒ tenant.ErrNoContext，
// 写入被拒；Action.Tenant 可与 ctx 一致（推荐显式带上，便于排障），不一致
// ⇒ ErrTenantMismatch。
//
// 存储键是 (tenant, key) / (tenant, resource)：跨租户同 Key 互不命中。
//
// ⚠ 独立构造器只拿到 *sql.DB ⇒ 开不出第二个只读句柄（只读句柄要另开一个
// 连接，见 Store.Open）⇒ 本构造器造出的面**独立读路径与写句柄同源**。
// 需要「读不阻塞写」的宿主请用 Store.TenantEffects()。
func NewTenantEffectStore(db *sql.DB) *EffectStore {
	return &EffectStore{exec: execOf(db), tenantScoped: true}
}

// NewTenantEventStore 构造**租户作用域**的事件面存储（对位 PG 侧同名构造器）。
//
// 写读都从 ctx 取租户，缺租户 fail-closed；Event.Tenant 与 ctx 不一致 ⇒
// ErrTenantMismatch。每个租户有自己的水位行与 ID 空间（1..committed），因此
// 租户流仍满足 TailStore 的「无空洞推进」——过滤共享 ID 空间会产生永久空洞。
//
// ⚠ 同 NewTenantEffectStore：独立读路径与写句柄同源。
func NewTenantEventStore(db *sql.DB) *EventStore {
	return &EventStore{exec: execOf(db), tenantScoped: true}
}

// NewTenantDownstream 构造**租户作用域**的下游栅栏（对位 PG 侧同名构造器）。
//
// 写入 / 读取都从 ctx 取租户；缺租户 ⇒ tenant.ErrNoContext。同一 resource
// 字符串在两个租户下是两条独立的已见令牌记录——跨租户不会互相抬栅栏。
//
// ⚠ 同 NewTenantEffectStore：独立读路径与写句柄同源。
func NewTenantDownstream(db *sql.DB) *Downstream {
	return &Downstream{exec: execOf(db), tenantScoped: true}
}

// NewTenantTailCursorStore 构造**租户作用域**的尾随游标存储（对位 PG 侧同名构造器）。
//
// 每次读写都从 ctx 取租户（缺 ⇒ tenant.ErrNoContext）；consumer 键由
// tenantCursorKey(tenant, consumer) 派生（长度前缀 ⇒ 单射，不同
// (tenant, consumer) 组合拼不出同一个键）。
//
// consumer 不得为空：空键会让所有尾随者共享一条游标 ⇒ 后跑的推过对方整段
// 窗口 ⇒ 订阅侧静默漏帧。fail fast 在构造点（与 PG 侧同口径）。
//
// ⚠ 独立构造器只拿到 *sql.DB ⇒ 开不出第二个只读句柄 ⇒ 独立读路径与写句柄
// 同源。需要「读不阻塞写」的宿主请用 Store.TenantTailCursors()。
func NewTenantTailCursorStore(db *sql.DB, consumer string) *TenantTailCursorStore {
	if consumer == "" {
		panic("sqliteadapter: TenantTailCursorStore 需要 consumer key（空键会让所有尾随者共享一条游标）")
	}
	return &TenantTailCursorStore{exec: execOf(db), consumer: consumer}
}

// TenantEffects 租户作用域的副作用闸（门面入口：自带独立只读句柄）。
//
// 与 NewTenantEffectStore 的差别只有一个：这里开得出只读句柄 ⇒ 写事务进行中
// 独立读路径照样能查（不排队到超时）。
func (s *Store) TenantEffects() *EffectStore {
	return &EffectStore{exec: s.execBase(), store: s, tenantScoped: true}
}

// TenantEvents 租户作用域的事件面（门面入口：自带独立只读句柄）。
func (s *Store) TenantEvents() *EventStore {
	return &EventStore{exec: s.execBase(), tenantScoped: true}
}

// TenantDownstream 租户作用域的下游栅栏（门面入口：自带独立只读句柄）。
func (s *Store) TenantDownstream() *Downstream {
	return &Downstream{exec: s.execBase(), tenantScoped: true}
}

// EffectStore.TenantScoped 报告本实例是否声明了租户作用域（装配期自检用）。
//
// 用途（PG 侧注释原话）：把「忘记装配」变成**启动期失败**，而不是运行期的
// 跨租户可见。
func (s *EffectStore) TenantScoped() bool { return s.tenantScoped }

// EventStore.TenantScoped 报告本实例是否声明了租户作用域（装配期自检用）。
//
// 用途同 EffectStore.TenantScoped。
func (s *EventStore) TenantScoped() bool { return s.tenantScoped }

// Downstream.TenantScoped 报告本实例是否声明了租户作用域（装配期自检用）。
//
// 用途同 EffectStore.TenantScoped。
func (d *Downstream) TenantScoped() bool { return d.tenantScoped }
