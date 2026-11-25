// effect.go —— sideeffect.Store 的 SQLite 参考实现：类型、事务骨架与 Acquire。
//
// # 判定口径逐项对位 pgstore（v0.2.0 自 goagent-kernel 迁入）/effect*.go
//
// 同一套契约、同一套判据，只有方言不同：$1 → ?、TIMESTAMPTZ → INTEGER
// （Unix 毫秒）、BYTEA → BLOB、JSONB → TEXT、clock_timestamp() →
// unixepoch('subsec')*1000。判据的**顺序**逐条照搬（见 acquireBody）。
//
// # 两级状态
//
//   - **Key 级**（gak_effect_action）：做过没有（done/result）、结果未知
//     （uncertain/reason）、执行意图（intent_* 四列）、对账占用（recon_* 两列）、
//     动作身份（id_* 三列）。
//   - **资源级**（gak_effect_resource）：谁占着（holder/until）、发第几代令牌
//     （fence）。
//
// 两级状态**不是一回事**（判据也不同）：Key 级答「这个逻辑动作做过没有」，
// 资源级答「这个资源此刻归谁」。Acquire 一次操作同时给两者下结论——拆成两次
// 判定就会出现「租约说该做、幂等说没做过」的窗口，两份真相等于没有真相。
//
// # 原子性来自「BEGIN IMMEDIATE 先拿写锁」
//
// 写句柄的 DSN 带 _txlock=immediate ⇒ 事务一开就拿写锁 ⇒ 校验 → 预占 → 执行
// 三段在同一把锁下 ⇒ 两个执行者同时抢同一动作时，输家在写锁上排队，赢家提交
// 后输家读到的是判定**后**的新状态（契约要求恰好一个赢家）。
//
// PG 侧靠行锁（SELECT … FOR UPDATE）拿同样的原子性，锁序恒为「资源行 → 动作行」。
// SQLite 侧只有一把库级写锁 ⇒ 交叉持锁（死锁）结构性不存在，但**锁序的语义
// 仍按 PG 同序保留**（先资源行、后动作行），两侧判据才能逐条对读。
//
// # 事务内一律走 tx（不绕开）
//
// 写句柄只有一条连接（MaxOpenConns(1)）：事务占用它期间若从写句柄另取连接
// ⇒ 永久等待（实测挂到 ctx 超时）⇒ 事务内的读一律走 tx。事务内**不得**再调
// s.exec.run（那是嵌套事务 ⇒ 外层占着唯一连接、内层永远等）。
package sqliteadapter

import (
	"context"
	"database/sql"
	"time"

	"github.com/oekit/pkg/errors"

	"github.com/oagentkit/goagent-kernel/sideeffect"
)

// EffectStore sideeffect.Store 的 SQLite 参考实现。
//
// 一个实例绑定一个库文件（两个句柄：写句柄 + 独立只读句柄）；并发安全
// （进程内写串行化靠单连接池，跨进程靠 BEGIN IMMEDIATE + 文件锁）。
type EffectStore struct {
	// exec 执行位置：db 是写句柄，read 是只读句柄（独立读路径走它）。
	exec exec
	// store 归属的 Store：面级时钟的兜底来源（见 clockSource）。
	store *Store
	// tenantScoped 装配期声明（issue #275）：true ⇒ 每个入口从 ctx 取租户，
	// 缺租户拒绝（fail-closed）。false ⇒ 旧布局，租户取 Action.Tenant
	// （空 = 单命名空间），不强制上下文。
	tenantScoped bool
}

// 编译期断言：本实现满足副作用闸的两套契约。
var (
	_ sideeffect.Store             = (*EffectStore)(nil)
	_ sideeffect.OwnedResolveStore = (*EffectStore)(nil)
)

// Effects 副作用闸（与其它面共用同一份事务域）。
//
// 独立读路径走**只读句柄**，事务内的读写走写句柄的 tx——路由判据见
// exec.reader。本面**没有**独立读路径（动作面全是事务内读写），但栅栏面有
// （见 effect_sink.go），两者共用同一个 Store 的两个句柄。
func (s *Store) Effects() *EffectStore {
	return &EffectStore{exec: s.execBase(), store: s}
}

// NewEffectStore 构造副作用闸存储（db 归调用方管理）。
//
// 旧布局：存储键是 (tenant, key) / (tenant, resource)，租户取 Action.Tenant
// 自带的值（空 = 单命名空间），不强制租户上下文。
//
// ⚠ 独立构造器只拿到 *sql.DB ⇒ 开不出第二个只读句柄（只读句柄要另开一个
// 连接，见 Store.Open）⇒ 本构造器造出的面**独立读路径与写句柄同源**。
// 需要「读不阻塞写」的宿主请用 Store.Effects()。
func NewEffectStore(db *sql.DB) *EffectStore {
	return &EffectStore{exec: execOf(db)}
}

// UseClock 注入时钟（sidetest.ClockedStore 硬要求）。
//
// 注入的是**本面自己的**时钟；本面没注入时回落到 Store 级时钟
// （Store.UseClock）——两个注入点合流见 clockSource。
func (s *EffectStore) UseClock(now func() time.Time) {
	s.exec.clock = now
}

// clockSource 取本面的时钟来源：面级注入优先，其次 Store 级注入，都没有 ⇒
// nil（nil ⇒ 读存储时钟）。
//
// 为什么两个注入点要合流而不是拷贝一份 func 值：契约套件注入的是**面级**
// 时钟（ClockedStore.UseClock），Store.UseClock 是装配期注入；拷贝会让
// 「先 Effects() 后 Store.UseClock()」这一顺序**静默**退回存储时钟 ⇒ 时钟口径
// 漂移且没有任何信号 ⇒ 这里按指针解析，两个注入点共用一份真相。
func (s *EffectStore) clockSource() func() time.Time {
	if s.exec.clock != nil {
		return s.exec.clock
	}
	if s.store != nil {
		return s.store.clock
	}
	return nil
}

// execIn 造一个事务内的执行位置（共享工厂在 Store 上，见 Store.execIn）。
//
// 这里只做一件事：把**面级**解析出的时钟交给共享工厂——构造点只有一处，
// 「漏 clock」没有发生的地方。
//
// ⚠ 时钟必须由**面**给：契约套件注入的是**面级**时钟，若工厂只取 Store 级
// 时钟 ⇒ 面级注入失效 ⇒ 过期判定读不到注入时钟 ⇒ 套件立刻红。
func (s *EffectStore) execIn(tx *sql.Tx) exec {
	if s.store == nil {
		// 防御分支：EffectStore 由 Store.Effects() 或 NewEffectStore 造 ⇒
		// s.store 只在独立构造器下为 nil。仍按同一口径补上 tx 与 clock
		// （面里不另抄一份构造逻辑）。
		e := s.exec
		e.tx = tx
		e.clock = s.clockSource()
		return e
	}
	e := s.store.execIn(tx, s.clockSource())
	// 失败槽随执行位置走（批次级状态：各面共享同一份）。
	e.failed = s.exec.failed
	return e
}

// prepareAction 入口处的租户解析：把作用域结论落回 Action.Tenant，后续
// 全部 SQL 用 act.Tenant 做联合定位（解析只在入口发生一次）。
//
// 旧布局（默认形态）：不强制租户，租户取 Action.Tenant 自带的值
// （空 = 单命名空间）⇒ 恒等映射。
//
// 租户作用域：每个入口从 ctx 取租户（tenant.Require），缺租户 ⇒ 拒绝
// （fail-closed）；Action.Tenant 与 ctx 不一致 ⇒ ErrTenantMismatch。
//
// ⚠ 解析失败要**登记进失败槽**：本函数在 exec.run **之外**被调（租户拒绝
// 发生在任何写入之前）⇒ 不登记的话，调用方吞掉这个错误时整批仍会提交 ⇒
// 「失败即作废整批」对租户拒绝是假的。
func (s *EffectStore) prepareAction(ctx context.Context, act sideeffect.Action) (sideeffect.Action, error) {
	t, err := resolveTenant(ctx, s.tenantScoped, act.Tenant)
	if err != nil {
		s.exec.fail(err)
		return act, err
	}
	act.Tenant = t
	return act, nil
}

// Acquire 一次原子判定（契约语义逐条对照 sideeffect.Store 的文档）。
//
// 判定顺序是契约里最要紧的一行：**已完成 > 结果未知 > 执行中 > 资源被占 >
// 授予**。顺序写反的代价是重复施加：把「结果未知」排到「资源被占」之后，
// 调用方会拿到 contended（以为「晚点重试就行」），于是重试发生、重复施加
// 随之发生。
//
// 为什么整段在一个事务里：三判合一的每一步都读同一份状态，BEGIN IMMEDIATE
// 持有的写锁保证第二个进程要么读到判定前的旧状态（然后在锁上等），要么读到
// 判定后的新状态——不存在「两个进程同时读到没做过、资源空闲」的窗口。
func (s *EffectStore) Acquire(ctx context.Context, act sideeffect.Action, ttl time.Duration) (sideeffect.Outcome, sideeffect.Lease, sideeffect.Result, error) {
	act, err := s.prepareAction(ctx, act)
	if err != nil {
		return sideeffect.OutcomeContended, sideeffect.Lease{}, sideeffect.Result{}, err
	}
	var (
		outcome sideeffect.Outcome
		lease   sideeffect.Lease
		result  sideeffect.Result
	)
	err = s.exec.run(ctx, func(tx *sql.Tx) error {
		in := s.execIn(tx)
		// 锁序：资源行 → 动作行（与 PG 同序）。
		if err := ensureResourceRow(ctx, tx, act.Tenant, act.Resource); err != nil {
			return err
		}
		if err := ensureActionRow(ctx, tx, act); err != nil {
			return err
		}
		rr, err := lockResource(ctx, tx, act.Tenant, act.Resource)
		if err != nil {
			return err
		}
		a, err := lockAction(ctx, tx, act)
		if err != nil {
			return err
		}
		// 判定时刻：**拿到写锁之后**取（等锁时长不得计入租约寿命）。
		now, err := in.now(ctx, tx)
		if err != nil {
			return err
		}
		outcome, lease, result, err = acquireBody(ctx, tx, act, ttl, now, rr, a)
		return err
	})
	if err != nil {
		return sideeffect.OutcomeContended, sideeffect.Lease{}, sideeffect.Result{}, err
	}
	return outcome, lease, result, nil
}

// acquireBody Acquire 的判定主体（事务内、行已就位）。
//
// 判据顺序不可改：已完成 > 结果未知 > 执行中 > 资源被占 > 授予。
//
// 授予时在同一事务里做三件事：令牌 = 资源已见最大值 + 1；持有者身份由
// sideeffect.MintHolder() 在**同一事务**里铸造；执行意图（本次令牌 + 到期
// 时刻）随授予一并落库。三者分开写就等于没写：中间死掉 ⇒ 意图悬空或没写，
// 重复施加的入口重新打开。
func acquireBody(
	ctx context.Context,
	tx *sql.Tx,
	act sideeffect.Action,
	ttl time.Duration,
	now time.Time,
	rr *resourceRow,
	a *actionRow,
) (sideeffect.Outcome, sideeffect.Lease, sideeffect.Result, error) {
	// 1. 已完成 → 重放既有结果（不再需要令牌）。
	if a.done {
		return sideeffect.OutcomeReplayed, sideeffect.Lease{}, a.resultValue(), nil
	}

	// 2. 结果未知 → 硬阻塞（先对账，不是等一等）。
	if a.uncertain {
		return sideeffect.OutcomeUncertain, sideeffect.Lease{}, sideeffect.Result{}, nil
	}

	// 2b. 对账占用未过期 ⇒ 对账中：仍属未知态一族。过期的占用在这里清掉
	// （可重入：对账者死亡不永久占住这个 Key）。
	if a.hasRecon() {
		if now.Before(a.reconExpiresAt()) {
			return sideeffect.OutcomeUncertain, sideeffect.Lease{}, sideeffect.Result{}, nil
		}
		a.clearRecon()
		if err := writeAction(ctx, tx, act.Tenant, act.Key, a); err != nil {
			return sideeffect.OutcomeContended, sideeffect.Lease{}, sideeffect.Result{}, err
		}
	}

	// 3. 执行中：这次尝试还没裁决 ⇒ 不得重新授予执行权。
	// hasIntent 要求四列齐全；半写行已由 lockAction 的 checkIntentColumns
	// 拒绝（ErrIntentIncomplete），不会走到下面解引用 NULL 的路径。
	// ⇒ 这里**不再**设第二道守卫（与 PG 侧同款：只有 lockAction 里那一道）：
	// hasIntent 为真即意味着四列齐全，本段解引用不会碰到 NULL；一道承重的
	// 守卫胜过一道不可达的——不可达的守卫只是注释与事实相反。
	if a.hasIntent() {
		if now.Before(a.intentExpiresAt()) {
			return sideeffect.OutcomeContended, sideeffect.Lease{}, sideeffect.Result{}, nil
		}
		// 租约过期 ⇒ 这次尝试的结果未知（可能已施加、账没记上）。
		// 意图与资源持有是同一次尝试的两个投影，同刻到期：作废意图时，
		// 若资源闸仍是这次尝试的那份，一并作废（没人接管时令牌没变，
		// 不清的话「尝试已转入未知态」在资源侧看不出来）。
		irResource := a.intentResource.String
		irHolder := a.intentHolder.String
		irFence := a.intentFence.Int64
		a.clearIntent()
		if irResource == act.Resource && rr.holder.Valid && rr.holder.String == irHolder && rr.fence == irFence {
			rr.holder = sql.NullString{}
			if err := writeResource(ctx, tx, act.Tenant, act.Resource, rr); err != nil {
				return sideeffect.OutcomeContended, sideeffect.Lease{}, sideeffect.Result{}, err
			}
		}
		if act.RetrySafety != sideeffect.RetrySafetySafe {
			a.uncertain = true
			a.reason = "上一次尝试的租约已过期，结果未知"
			if err := writeAction(ctx, tx, act.Tenant, act.Key, a); err != nil {
				return sideeffect.OutcomeContended, sideeffect.Lease{}, sideeffect.Result{}, err
			}
			return sideeffect.OutcomeUncertain, sideeffect.Lease{}, sideeffect.Result{}, nil
		}
	}

	// 4. 资源被他人持有且租约未过期 → 可重试的拒绝。
	if rr.held(now) {
		return sideeffect.OutcomeContended, sideeffect.Lease{}, sideeffect.Result{}, nil
	}

	// 5. 授予：令牌单调递增 + 铸造持有者身份 + 登记执行意图，同一事务。
	rr.fence++
	holder := sideeffect.MintHolder()
	rr.holder = sql.NullString{String: holder, Valid: true}
	until := now.Add(ttl)
	rr.until = sql.NullInt64{Int64: until.UnixMilli(), Valid: true}
	if err := writeResource(ctx, tx, act.Tenant, act.Resource, rr); err != nil {
		return sideeffect.OutcomeContended, sideeffect.Lease{}, sideeffect.Result{}, err
	}
	a.intentResource = sql.NullString{String: act.Resource, Valid: true}
	a.intentFence = sql.NullInt64{Int64: rr.fence, Valid: true}
	a.intentHolder = sql.NullString{String: holder, Valid: true}
	a.intentExpires = sql.NullInt64{Int64: until.UnixMilli(), Valid: true}
	if err := writeAction(ctx, tx, act.Tenant, act.Key, a); err != nil {
		return sideeffect.OutcomeContended, sideeffect.Lease{}, sideeffect.Result{}, err
	}
	return sideeffect.OutcomeApplied,
		sideeffect.Lease{Resource: act.Resource, Fence: sideeffect.Fence(rr.fence), Holder: holder, TTL: ttl},
		sideeffect.Result{}, nil
}

// ErrIntentIncomplete 动作行的执行意图四列**部分填充**（半写行）。
//
// 这是存储侧数据损坏 / 仓外写入方只写部分列的形态，不是本包写入路径能
// 产生的状态（本包只写「四列全有」或「四列全空」）。判定遇到它必须
// **fail-closed**：逐列解引用会 panic（库层 panic 直接杀宿主进程），
// 按「无意图」放行会重新授予执行权（重复施加的入口重新打开）。
var ErrIntentIncomplete = errors.New("sqliteadapter: 执行意图列部分填充，拒绝判定")
