// event.go —— eventbus 可靠事件面（Outbox 形态）的 PostgreSQL 参考实现。
//
// # 形态声明（与 eventbus.TailStore 的契约文字一致，issue #264）
//
// eventbus 的尾随读侧契约把「无空洞推进」收口为：**被承诺支持的生产形态
// 只有 Outbox**。本实现就是那个形态的落库样板：
//
//   - **写侧（事件行与业务状态同事务）**：PublishInTx 在调用方事务内落
//     事件行；业务写入与它共用同一个事务——提交则同现，回滚则同灭。
//   - **分配串行化（水位行）**：ID 由单行水位表 pgref_event_watermark 铸造
//     ——UPDATE … RETURNING 拿号的同时持行锁到事务提交，下一个发布者在
//     锁上等待 ⇒ **铸造序 == 提交序**，乱序提交结构性地不存在（issue #264
//     实证的丢帧序列「小 ID 在途、大 ID 先提交」在此形态下无法发生）。
//   - **回滚不烧号**：拿号、插行、业务写入全在同一事务，回滚一并消失
//     ⇒ 序列空洞结构性地不存在（对比 BIGSERIAL：回滚烧号 ⇒ 永久空洞，
//     水位线读法将在那里永久停摆）。
//   - **读侧（已提交水位线读法）**：ListAfter 的返回段以水位线为上界
//     （id > cursor AND id ≤ 水位线）——水位线之上哪怕已可见也扣住不返
//     （fail-closed 方向是扣住，不是放行）。无空洞推进由机制保证，不靠
//     观测截断。
//
// 明确不支持：用 BIGSERIAL + WHERE id > cursor 直读实现本面——那是
// eventbus.TailStore 契约点名的违约形态（哨兵响但帧已永久丢失；回滚烧号
// 造成永久停摆）。宿主接自己的自增表时，先读契约再决定，不要抄这个面
// 之外的捷径。
//
// # 与 DurablePublisher 的分工
//
// 与业务状态无原子性要求的事件仍走 eventbus.DurablePublisher + 本实现的
// Insert（独立事务形态，满足 eventbus.Store）；要求「事件与业务状态同生
// 共死」的发布走 PublishInTx——宿主在自己的业务事务里调它，**提交之后**
// 再广播（广播在事务外，回滚后的幻影帧由此排除）。两入口落同一张表、同一
// 套水位线，读侧（Tailer）无差别。
//
// # 串行化的代价（写明，不藏）
//
// 全部事件发布在水位行锁上串行——与 DurablePublisher 持锁串行 Insert 同款
// 取舍（事件是人的可读留痕而非高频指标，不是高频指标的形态）。水位行锁持
// 有到事务提交：业务事务较长时把 PublishInTx 放在事务末段，别让「拿号」
// 挡住其他副本的发布。
package pgstore

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/oekit/pkg/errors"

	"github.com/yi-nology/goagent-kernel/eventbus"
)

// EventStore eventbus 可靠事件面（Outbox 形态）的 PostgreSQL 参考实现：
// 写侧 PublishInTx / Insert（eventbus.Store），读侧 MaxID / ListAfter
// （eventbus.TailStore）与 CommittedWatermark（eventbus.WatermarkTailStore）。
//
// 一个实例绑定一个 schema（表由 EnsureSchema 建，水位行由建表语句种子化）；
// 并发安全（发布在水位行锁上串行，见文件头「串行化的代价」）。
type EventStore struct {
	pool *pgxpool.Pool
	// tenantScoped 装配期声明（issue #275）：true ⇒ 发布与读取都从 ctx
	// 取租户（缺租户 ⇒ tenant.ErrNoContext），每个租户一条独立 ID 空间与
	// 水位线——订阅侧按租户过滤由**存储流**保证，不靠载荷里自筛。
	tenantScoped bool
}

// NewEventStore 构造事件面存储。pool 归调用方管理（Close 由它负责）。
//
// 默认形态 = 旧布局 / 单租户命名空间（tenant ”）；事件行仍按 Event.Tenant
// 落租户列，但读取面固定读 ” 流。多租户部署用 NewTenantEventStore。
func NewEventStore(pool *pgxpool.Pool) *EventStore { return &EventStore{pool: pool} }

// NewTenantEventStore 构造**租户作用域**的事件面存储（issue #275）。
//
// 与 EffectStore 同款：所有写读从 ctx 取租户（tenant.Require），缺租户
// fail-closed；Event.Tenant 与 ctx 不一致 ⇒ ErrTenantMismatch。每个租户有
// 自己的水位行与 ID 空间（1..committed），因此租户流仍满足 TailStore 的
// 「无空洞推进」——过滤共享 ID 空间会产生永久空洞，不能这么实现。
func NewTenantEventStore(pool *pgxpool.Pool) *EventStore {
	return &EventStore{pool: pool, tenantScoped: true}
}

// TenantScoped 报告本实例是否声明了租户作用域（装配期自检用）。
func (s *EventStore) TenantScoped() bool { return s.tenantScoped }

// queryRower pgx 的 pool 与 tx 共有的最小读面（水位线读法在两者上都成立）。
type queryRower interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// normalizeEvent 与 DurablePublisher.Publish 同款的零值归一（Level / Payload），
// 归一发生在落库前——落库行与广播帧同一内容。OccurredAt 的零值补齐不在
// Go 侧做：本包时间源纪律是**数据库时钟**（跨节点共享的事实，见 doc.go
// 「时钟」段），零值经 INSERT 的 COALESCE 落到 clock_timestamp()（见
// publishInTx）。输入侧 ID 在 publishInTx 里另行清零（游标由水位行铸造，
// 与 Store 契约同口径）；Payload nil 归一为空对象（列 NOT NULL，往返不
// 引入 NULL 态）。
func normalizeEvent(ev eventbus.Event) eventbus.Event {
	if ev.Level == "" {
		ev.Level = "info"
	}
	if ev.Payload == nil {
		ev.Payload = map[string]any{}
	}
	return ev
}

// stepIDArg Event.StepID（*uint64）→ BIGINT 参数（pgx 整形绑定走 int64）。
func stepIDArg(p *uint64) any {
	if p == nil {
		return nil
	}
	return int64(*p)
}

// PublishInTx 在调用方事务内发布一条事件（Outbox 形态的写侧入口，#264）。
//
// 事件行与调用方在同一事务里的业务写入**同生共死**：提交则同现、回滚则
// 同灭（水位线推进一并回退，ID 不烧号——下一次发布复用同一 ID）。返回的
// 权威游标 ID 供提交后的广播携带（提交之后再广播，广播在事务外）。
//
// 输入事件的 ev.ID 一律忽略（游标由水位行铸造）；Level / Payload 的零值
// 归一与 DurablePublisher 同口径（见 normalizeEvent）；OccurredAt 零值由
// 数据库时钟补齐（本包时间源纪律，见 doc.go「时钟」段）。
func (s *EventStore) PublishInTx(ctx context.Context, tx pgx.Tx, ev eventbus.Event) (uint64, error) {
	return s.publishInTx(ctx, tx, ev)
}

// publishInTx 事务内发布核心：拿号（水位行锁）→ 插事件行。本函数不提交
// 不回滚——事务的生死归调用方（这正是 Outbox 形态的「同事务」含义）。
func (s *EventStore) publishInTx(ctx context.Context, tx pgx.Tx, ev eventbus.Event) (uint64, error) {
	ev = normalizeEvent(ev)
	ev.ID = 0 // 权威游标由水位行铸造：输入侧 ID 一律忽略（防御陈旧游标流入）

	// 租户作用域解析（入口一次）：缺租户拒绝；声明与 ctx 不一致拒绝。
	tenantID, err := resolveTenant(ctx, s.tenantScoped, ev.Tenant)
	if err != nil {
		return 0, err
	}
	ev.Tenant = tenantID

	// 拿号即持锁：按租户 UPSERT 水位行的 committed + 1 RETURNING 新值——
	// 行锁持有到本事务提交，同一租户的下一个发布者在此等待 ⇒ 铸造序 ==
	// 提交序；回滚则本次拿号一并消失（不烧号）⇒ 该租户序列空洞结构性地
	// 不存在。不同租户各锁各的行，互不阻塞、各有一条 1..committed 的流。
	var n int64
	if err := tx.QueryRow(ctx,
		`INSERT INTO pgref_event_watermark (tenant, committed) VALUES ($1, 1)
		 ON CONFLICT (tenant) DO UPDATE
		   SET committed = pgref_event_watermark.committed + 1
		 RETURNING committed`, tenantID).Scan(&n); err != nil {
		return 0, errors.Wrap(err, "pgstore: 事件水位线拿号失败（水位行就绪了吗？EnsureSchema）", "tenant", tenantID)
	}
	id := uint64(n) // 水位线从 0 单调递增，非负

	// OccurredAt 零值 → NULL → 数据库 clock_timestamp() 补齐：事件是跨节点
	// 共享的事实，发生时刻读唯一权威时钟源（与本包租约/认领同一条纪律，
	// pg_clock_test.go 判着）。调用方带了时刻则原样落库（不改写语义字段）。
	var occurred any
	if !ev.OccurredAt.IsZero() {
		occurred = ev.OccurredAt
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO pgref_event (tenant, id, session_id, case_id, step_id, type, level, payload, occurred_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, COALESCE($9, clock_timestamp()))`,
		tenantID, n, ev.SessionID, ev.CaseID, stepIDArg(ev.StepID), ev.Type, ev.Level, ev.Payload, occurred); err != nil {
		return 0, errors.Wrap(err, "pgstore: 事件行写入失败", "id", id, "tenant", tenantID)
	}
	return id, nil
}

// Insert eventbus.Store 的独立事务形态（与业务状态无原子性要求的事件）：
// 内部自开事务，失败整体回滚。DurablePublisher 可直接接本方法。
func (s *EventStore) Insert(ctx context.Context, ev eventbus.Event) (uint64, error) {
	return withinTx(ctx, s.pool, func(tx pgx.Tx) (uint64, error) {
		return s.publishInTx(ctx, tx, ev)
	})
}

// readWatermark 读已提交水位线（≤ 此值的游标 ID 已全部提交且可见）。
// MaxID 与 CommittedWatermark 共用这一读法——初始化读法与逐拍读法共用
// 同一条无空洞边界（#264 可机判口径第三条），不分裂成两个真相。
func readWatermark(ctx context.Context, q queryRower, tenantID string) (uint64, error) {
	var n int64
	if err := q.QueryRow(ctx,
		`SELECT committed FROM pgref_event_watermark WHERE tenant = $1`, tenantID).Scan(&n); err != nil {
		return 0, errors.Wrap(err, "pgstore: 读事件水位线失败（水位行就绪了吗？EnsureSchema）", "tenant", tenantID)
	}
	return uint64(n), nil // 水位线从 0 单调递增，非负
}

// readTenant 读路径的租户解析：租户作用域实例从 ctx 取（缺租户拒绝），
// 旧布局实例固定读 ” 流（与写路径同一条 resolveTenant）。
func (s *EventStore) readTenant(ctx context.Context) (string, error) {
	return resolveTenant(ctx, s.tenantScoped, "")
}

// MaxID 当前权威游标空间的**无空洞边界**（eventbus.TailStore，空表 = 0）。
//
// 不回裸 max(id)：水位线之上的行（宿主绕过写入协议落的行）不得把初始化
// 游标推过无空洞边界——否则初始化本身就把在途小 ID 跳过（#264）。Outbox
// 形态下无空洞边界就是已提交水位线（与 CommittedWatermark 同值同源）。
func (s *EventStore) MaxID(ctx context.Context) (uint64, error) {
	t, err := s.readTenant(ctx)
	if err != nil {
		return 0, err
	}
	return readWatermark(ctx, s.pool, t)
}

// CommittedWatermark 已提交水位线读法（eventbus.WatermarkTailStore，#264
// 的实现声明面）：实现本方法 = 声明本存储是 Outbox 形态，可机判口径见
// eventbus 包的接口文档。
func (s *EventStore) CommittedWatermark(ctx context.Context) (uint64, error) {
	t, err := s.readTenant(ctx)
	if err != nil {
		return 0, err
	}
	return readWatermark(ctx, s.pool, t)
}

// ListAfter 列出 id > cursor 的事件（eventbus.TailStore，升序，limit 为单拍
// 上限；limit 由 Tailer 保证为正——DefaultTailLimit / WithTailBatchLimit）。
//
// 返回段被已提交水位线封顶：水位线之上的行哪怕已提交可见（宿主绕过写入
// 协议落的行）也扣住不返，下一拍水位线推进后再读——fail-closed 方向是扣住
// （#264 可机判口径第二条）。水位线机制保证 (cursor, 水位线] 内连续无洞：
// 水位线只在「对应事件行提交」的那笔事务里推进（同生共死），故对任何可见
// 快照，≤ 水位线的行全部已提交。读失败不得返回部分结果（契约第四条）：
// 行扫描中途出错即整体作废。
func (s *EventStore) ListAfter(ctx context.Context, cursor uint64, limit int) ([]eventbus.Event, error) {
	tenantID, err := s.readTenant(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `
		SELECT tenant, id, session_id, case_id, step_id, type, level, payload, occurred_at
		  FROM pgref_event
		 WHERE tenant = $1
		   AND id > $2
		   AND id <= (SELECT committed FROM pgref_event_watermark WHERE tenant = $1)
		 ORDER BY id
		 LIMIT $3`, tenantID, int64(cursor), limit)
	if err != nil {
		return nil, errors.Wrap(err, "pgstore: 读事件段失败")
	}
	defer rows.Close()
	var out []eventbus.Event
	for rows.Next() {
		var (
			n      int64
			stepID *int64
			ev     eventbus.Event
		)
		if err := rows.Scan(&ev.Tenant, &n, &ev.SessionID, &ev.CaseID, &stepID, &ev.Type, &ev.Level, &ev.Payload, &ev.OccurredAt); err != nil {
			return nil, errors.Wrap(err, "pgstore: 解析事件行失败")
		}
		ev.ID = uint64(n) // 水位线之下的事件 ID 从 1 单调递增，非负
		if stepID != nil {
			step := uint64(*stepID)
			ev.StepID = &step
		}
		out = append(out, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, errors.Wrap(err, "pgstore: 读事件段失败")
	}
	return out, nil
}
