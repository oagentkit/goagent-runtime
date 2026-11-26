// event.go —— eventbus 可靠事件面（Outbox 形态）的 SQLite 参考实现。
//
// # 形态声明（与 eventbus.TailStore 的契约文字一致，issue #264）
//
// eventbus 的尾随读侧契约把「无空洞推进」收口为：**被承诺支持的生产形态
// 只有 Outbox**。本实现就是那个形态的落库样板：
//
//   - **写侧（事件行与业务状态同事务）**：PublishInTx 在调用方事务内落事件行；
//     业务写入与它共用同一个事务——提交则同现，回滚则同灭。
//   - **分配串行化（水位行）**：ID 由水位表 gak_event_watermark（每租户一行）
//     铸造——UPSERT + RETURNING 拿号的同时持行锁到事务提交，下一个发布者在
//     锁上等待 ⇒ **铸造序 == 提交序**，乱序提交结构性地不存在（issue #264
//     实证的丢帧序列「小 ID 在途、大 ID 先提交」在此形态下无法发生）。
//   - **回滚不烧号**：拿号、插行、业务写入全在同一事务，回滚一并消失
//     ⇒ 序列空洞结构性地不存在（对比自增序列：回滚烧号 ⇒ 永久空洞，水位线
//     读法将在那里永久停摆）。
//   - **读侧（已提交水位线读法）**：ListAfter 的返回段以水位线为上界
//     （id > cursor AND id ≤ 水位线）——水位线之上哪怕已可见也扣住不返
//     （fail-closed 方向是扣住，不是放行）。无空洞推进由机制保证，不靠
//     观测截断。
//
// 明确不支持：用自增表 + WHERE id > cursor 直读实现本面——那是
// eventbus.TailStore 契约点名的违约形态（哨兵响但帧已永久丢失；回滚烧号
// 造成永久停摆）。宿主接自己的自增表时，先读契约再决定。
//
// # 方言映射（PG → SQLite）
//
//   - 占位符 $1/$2 → ?；TIMESTAMPTZ → INTEGER（Unix 毫秒）；JSONB → TEXT
//     （json.Marshal + json.Unmarshal）；BIGSERIAL → 水位行铸造。
//   - 时钟：PG 的 clock_timestamp() → SQLite 的 unixepoch('subsec')*1000
//     （**毫秒级**，实测最小非零增量 0.9999ms；同事务内不冻结）。
//
// # ⚠ 已提交水位线 ≠ 事件表 MAX(id)
//
// 水位线读**水位行**，MAX(id) 读**事件行**——两个不同的数。上界关系是
// 「**水位线 ≤ MAX(id)**」：正常协议写入下两者相等；宿主绕过写入协议落的行
// 把 MAX(id) 顶高，但**不得**推进水位线（否则无空洞边界被推到在途小 ID 之上
// ⇒ 初始化即跳帧）。MaxID 与 CommittedWatermark 都读水位行（同值同源），
// **不回裸 max(id)**。
//
// # 租户作用域
//
// 两种形态（判据入口见 tenant.go 的 resolveTenant，三段）：
//
//   - **默认形态**（NewEventStore / Store.Events）：写路径按事件自带的 Tenant
//     分区（空 = ""），读路径固定读 "" 流——与 PG 侧 NewEventStore 同款。
//   - **租户作用域**（NewTenantEventStore / Store.TenantEvents）：每次读写
//     都从 ctx 取租户（缺租户 ⇒ tenant.ErrNoContext，fail-closed）；事件自带
//     的 Tenant 与 ctx 不一致 ⇒ ErrTenantMismatch。
//
// 同一 ctx 只解析一次（入口解析，后续 SQL 一律用这一份结论）⇒ 同一个事件
// 不会在两处得出不同租户。
package sqliteadapter

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/oekit/pkg/errors"

	"github.com/yi-nology/goagent-kernel/eventbus"
)

// EventStore eventbus 可靠事件面（Outbox 形态）的 SQLite 参考实现：
// 写侧 PublishInTx / Insert（eventbus.Store），读侧 MaxID / ListAfter
// （eventbus.TailStore）与 CommittedWatermark（eventbus.WatermarkTailStore）。
//
// 并发安全（发布在水位行锁上串行，见文件头「分配串行化」）。
type EventStore struct {
	// exec 执行位置：db 是写句柄，read 是只读句柄（独立读路径走它）。
	exec exec
	// tenantScoped 装配期声明（issue #275）：true ⇒ 发布与读取都从 ctx 取租户
	//（缺租户 ⇒ tenant.ErrNoContext），每个租户一条独立 ID 空间与水位线。
	//
	// 默认形态（NewEventStore / Store.Events）恒为 false：租户取事件自带的
	// Tenant（空 = 单命名空间），读面固定读 "" 流——与 PG 侧 NewEventStore
	// 同款。租户作用域形态见 NewTenantEventStore / Store.TenantEvents。
	tenantScoped bool
}

// 编译期断言：本实现满足事件面的三套契约。
var (
	_ eventbus.Store              = (*EventStore)(nil)
	_ eventbus.TailStore          = (*EventStore)(nil)
	_ eventbus.WatermarkTailStore = (*EventStore)(nil)
)

// Events 事件面（与其它面共用同一份事务域）。
func (s *Store) Events() *EventStore { return &EventStore{exec: s.execBase()} }

// normalizeEvent 与 DurablePublisher.Publish 同款的零值归一（Level / Payload），
// 归一发生在落库前——落库行与广播帧同一内容。
//
// OccurredAt 的零值补齐**不在** Go 侧做：本包时间源纪律是**存储时钟**
// （unixepoch('subsec')，跨进程共享的事实），零值由 INSERT 的 COALESCE 落到
// 存储时钟上。输入侧 ID 一律清零（游标由水位行铸造）。
func normalizeEvent(ev eventbus.Event) eventbus.Event {
	if ev.Level == "" {
		ev.Level = "info"
	}
	if ev.Payload == nil {
		ev.Payload = map[string]any{}
	}
	return ev
}

// stepIDArg Event.StepID（*uint64）→ INTEGER 参数（NULL 走 nil）。
func stepIDArg(p *uint64) any {
	if p == nil {
		return nil
	}
	return int64(*p)
}

// encodeEventPayload 载荷序列化成 JSON 文本（列是 TEXT NOT NULL）。
func encodeEventPayload(p map[string]any) (string, error) {
	b, err := json.Marshal(p)
	if err != nil {
		return "", errors.Wrap(err, "sqliteadapter: 事件载荷序列化失败")
	}
	return string(b), nil
}

// PublishInTx 在调用方事务内发布一条事件（Outbox 形态的写侧入口，#264）。
//
// 事件行与调用方在同一事务里的业务写入**同生共死**：提交则同现、回滚则同灭
// （水位线推进一并回退，ID 不烧号——下一次发布复用同一 ID）。返回的权威游标
// ID 供提交后的广播携带（提交之后再广播，广播在事务外）。
//
// 输入事件的 ev.ID 一律忽略（游标由水位行铸造）。
func (s *EventStore) PublishInTx(ctx context.Context, tx *sql.Tx, ev eventbus.Event) (uint64, error) {
	id, err := s.publishInTx(ctx, tx, ev)
	if err != nil {
		// 失败登记：PublishInTx 不经 exec.run（调用方自带事务）⇒ 失败必须
		// 显式登记，否则调用方吞掉它时整批仍会提交。
		s.exec.fail(err)
	}
	return id, err
}

// publishInTx 事务内发布核心：拿号（水位行锁）→ 插事件行。本函数不提交
// 不回滚——事务的生死归调用方（这正是 Outbox 形态的「同事务」含义）。
func (s *EventStore) publishInTx(ctx context.Context, tx *sql.Tx, ev eventbus.Event) (uint64, error) {
	ev = normalizeEvent(ev)
	ev.ID = 0 // 权威游标由水位行铸造：输入侧 ID 一律忽略（防御陈旧游标流入）

	// 租户分区键：入口解析一次（默认形态取事件自带的 Tenant，空 = 单命名
	// 空间——与 PG 侧 NewEventStore 同款；租户作用域形态从 ctx 取，缺租户
	// 拒绝、声明与 ctx 不一致拒绝）。
	tenantID, err := resolveTenant(ctx, s.tenantScoped, ev.Tenant)
	if err != nil {
		return 0, err
	}
	ev.Tenant = tenantID

	// 拿号即持锁：按租户 UPSERT 水位行的 committed + 1 RETURNING 新值——
	// 行锁持有到本事务提交，同一租户的下一个发布者在此等待 ⇒ 铸造序 ==
	// 提交序；回滚则本次拿号一并消失（不烧号）⇒ 该租户序列空洞结构性地
	// 不存在。不同租户各锁各的行，互不阻塞、各有一条 1..committed 的流。
	//
	// ⚠ 为什么是 UPSERT 而不是裸 UPDATE：水位行是**惰性建出**的（PG 侧在建表
	// 时种下 tenant='' 的那行；SQLite 侧首次发布时才建）⇒ 裸 UPDATE 在空库
	// 上无行可更新 ⇒ 铸不出第一号。
	var n int64
	if err := tx.QueryRowContext(ctx,
		`INSERT INTO gak_event_watermark (tenant, committed) VALUES (?, 1)
		 ON CONFLICT (tenant) DO UPDATE
		   SET committed = gak_event_watermark.committed + 1
		 RETURNING committed`, tenantID).Scan(&n); err != nil {
		return 0, errors.Wrap(err, "sqliteadapter: 事件水位线拿号失败", "tenant", tenantID)
	}
	id := uint64(n) // 水位线从 0 单调递增，非负

	// OccurredAt 零值 → NULL → 存储时钟 unixepoch('subsec') 补齐：事件是
	// 跨进程共享的事实，发生时刻读唯一权威时钟源（与本包租约/认领同一条
	// 纪律）。调用方带了时刻则原样落库（不改写语义字段）。
	var occurred any
	if !ev.OccurredAt.IsZero() {
		occurred = ev.OccurredAt.UnixMilli()
	}
	payload, err := encodeEventPayload(ev.Payload)
	if err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO gak_event (tenant, id, session_id, case_id, step_id, type, level, payload, occurred_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?,
		         COALESCE(?, CAST(unixepoch('subsec') * 1000 AS INTEGER)))`,
		tenantID, n, ev.SessionID, ev.CaseID, stepIDArg(ev.StepID),
		ev.Type, ev.Level, payload, occurred); err != nil {
		return 0, errors.Wrap(err, "sqliteadapter: 事件行写入失败", "id", id, "tenant", tenantID)
	}
	return id, nil
}

// Insert eventbus.Store 的独立事务形态（与业务状态无原子性要求的事件）：
// 内部自开事务，失败整体回滚。DurablePublisher 可直接接本方法。
func (s *EventStore) Insert(ctx context.Context, ev eventbus.Event) (uint64, error) {
	var id uint64
	err := s.exec.run(ctx, func(tx *sql.Tx) error {
		var err error
		id, err = s.publishInTx(ctx, tx, ev)
		return err
	})
	if err != nil {
		return 0, err
	}
	return id, nil
}

// readTenant 读路径的租户：默认形态固定读 "" 流（与 PG 侧 NewEventStore 同款）；
// 租户作用域形态按 ctx 读（缺租户 ⇒ tenant.ErrNoContext，fail-closed）。
//
// 事件行仍按 Event.Tenant 分区（不会静默并进 "" 流），但默认形态的读面只读
// 单命名空间。
func (s *EventStore) readTenant(ctx context.Context) (string, error) {
	return resolveTenant(ctx, s.tenantScoped, "")
}

// readWatermark 读已提交水位线（≤ 此值的游标 ID 已全部提交且可见）。
//
// MaxID 与 CommittedWatermark 共用这一读法——初始化读法与逐拍读法共用同一条
// 无空洞边界（#264 可机判口径第三条），不分裂成两个真相。
//
// ⚠ 缺行 = 零事件 = 水位线 0（COALESCE）：水位行是惰性建出的，空库上根本
// 没有那一行——契约要求 MaxID 在空表上返回 0（PG 侧靠建表时种行达成同一效果）。
func (s *EventStore) readWatermark(ctx context.Context, q queryer, tenantID string) (uint64, error) {
	var n int64
	if err := q.QueryRowContext(ctx,
		`SELECT COALESCE((SELECT committed FROM gak_event_watermark WHERE tenant = ?), 0)`,
		tenantID).Scan(&n); err != nil {
		return 0, errors.Wrap(err, "sqliteadapter: 读事件水位线失败", "tenant", tenantID)
	}
	if n < 0 {
		return 0, errors.New("sqliteadapter: 事件水位线为负（数据损坏）")
	}
	return uint64(n), nil
}

// MaxID 当前权威游标空间的**无空洞边界**（eventbus.TailStore，空表 = 0）。
//
// 不回裸 max(id)：水位线之上的行（宿主绕过写入协议落的行）不得把初始化游标
// 推过无空洞边界——否则初始化本身就把在途小 ID 跳过（#264）。Outbox 形态下
// 无空洞边界就是已提交水位线（与 CommittedWatermark 同值同源）。
func (s *EventStore) MaxID(ctx context.Context) (uint64, error) {
	t, err := s.readTenant(ctx)
	if err != nil {
		return 0, err
	}
	q, err := s.exec.reader()
	if err != nil {
		return 0, err
	}
	return s.readWatermark(ctx, q, t)
}

// CommittedWatermark 已提交水位线读法（eventbus.WatermarkTailStore，#264 的
// 实现声明面）：实现本方法 = 声明本存储是 Outbox 形态。
func (s *EventStore) CommittedWatermark(ctx context.Context) (uint64, error) {
	t, err := s.readTenant(ctx)
	if err != nil {
		return 0, err
	}
	q, err := s.exec.reader()
	if err != nil {
		return 0, err
	}
	return s.readWatermark(ctx, q, t)
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
//
// ⚠ 上界与取段在**同一条语句**里（子查询读水位行）⇒ 一个快照，不存在
// 「读完水位线、事件行又推进了」的窗口。
func (s *EventStore) ListAfter(ctx context.Context, cursor uint64, limit int) ([]eventbus.Event, error) {
	t, err := s.readTenant(ctx)
	if err != nil {
		return nil, err
	}
	q, err := s.exec.reader()
	if err != nil {
		return nil, err
	}
	rows, err := q.QueryContext(ctx, `
		SELECT tenant, id, session_id, case_id, step_id, type, level, payload, occurred_at
		  FROM gak_event
		 WHERE tenant = ?
		   AND id > ?
		   AND id <= COALESCE((SELECT committed FROM gak_event_watermark WHERE tenant = ?), 0)
		 ORDER BY id
		 LIMIT ?`, t, int64(cursor), t, limit)
	if err != nil {
		return nil, errors.Wrap(err, "sqliteadapter: 读事件段失败")
	}
	defer func() { _ = rows.Close() }()
	var out []eventbus.Event
	for rows.Next() {
		var (
			n          int64
			stepID     sql.NullInt64
			ev         eventbus.Event
			payload    string
			occurredAt int64
		)
		if err := rows.Scan(&ev.Tenant, &n, &ev.SessionID, &ev.CaseID, &stepID,
			&ev.Type, &ev.Level, &payload, &occurredAt); err != nil {
			return nil, errors.Wrap(err, "sqliteadapter: 解析事件行失败")
		}
		ev.ID = uint64(n) // 水位线之下的事件 ID 从 1 单调递增，非负
		if stepID.Valid {
			step := uint64(stepID.Int64)
			ev.StepID = &step
		}
		if payload != "" {
			if err := json.Unmarshal([]byte(payload), &ev.Payload); err != nil {
				return nil, errors.Wrap(err, "sqliteadapter: 解析事件载荷失败", "id", ev.ID)
			}
		}
		ev.OccurredAt = occurredAtToTime(occurredAt)
		out = append(out, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, errors.Wrap(err, "sqliteadapter: 读事件段失败")
	}
	return out, nil
}

// occurredAtToTime 存储时刻（Unix 毫秒）→ time.Time（UTC）。
//
// 零值时刻（未补齐的遗留行）落成 Unix 纪元而不是 time.Time 零值：后者是
// 「有没有这个字段」的哨兵语义，两者混用会让读侧判错。
func occurredAtToTime(ms int64) time.Time {
	return time.UnixMilli(ms).UTC()
}
