// effect.go —— sideeffect.Store 的 PostgreSQL 参考实现：类型、事务骨架与行读写。
//
// # 三条判据在库里的落法（与内存版 effectStore 逐条对应）
//
//  1. **三项联查**：Complete / Release / Abandon / Renew / MarkUncertain 都在
//     同一个事务里校验「令牌仍是最新代 + 持有者身份一致 + 租约与动作的资源
//     一致」。校验与写入在同一事务 ⇒ 任何一步失败都发生在**任何状态被改之前**
//     ——这正是 MarkUncertain「标记写失败必须保住执行意图」的落库形态。
//  2. **执行意图**：Acquire 的授予（令牌 + 持有者身份）与意图登记写在
//     同一事务里；意图过期未裁决 ⇒ 同一 Key 转入未知态而非重新授予。
//  3. **对账占用**：BeginReconcile 与裁决分表字段，占用过期可重入。
//
// # 原子性来自哪里
//
// 每个方法 = 一个事务 = 若干条 `SELECT … FOR UPDATE` + 条件写。行锁把
// 「读状态 → 判 → 写」钉成跨进程原子：第二个进程在第一个提交前读到的
// 永远是旧状态，且会在行锁上等待。这与「一条带条件的 UPDATE」等价，
// 是契约点名认可的两种形态之一。
//
// 锁序恒为「资源行 → 动作行」：所有方法按同一顺序加锁，两个并发事务
// 不可能交叉持锁（交叉死锁在锁序一致时不存在）。
package pgstore

import (
	"context"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/oekit/pkg/errors"

	"github.com/oagentkit/goagent-kernel/sideeffect"
)

// EffectStore sideeffect.Store 的 PostgreSQL 参考实现。
//
// 一个实例绑定一个 schema（全部表在其中）；并发安全。
type EffectStore struct {
	pool *pgxpool.Pool
	// tenantScoped 装配期声明（issue #275）：true ⇒ 每个入口从 ctx 取租户，
	// 缺租户拒绝（fail-closed）。false ⇒ 旧布局，租户取 Action.Tenant
	// （空 = ''），不强制上下文。
	tenantScoped bool

	// mu 保护 now：UseClock 与各方法的时钟读取互斥（套件在测试里注入时钟）。
	mu  sync.Mutex
	now func() time.Time
}

// NewEffectStore 构造副作用闸存储。pool 归调用方管理（Close 由它负责）。
//
// schema 必须已建好表（EnsureSchema）；search_path 已被连接池指到该 schema
// （见 NewEffectStores），故本包的 SQL 不写 schema 前缀。
func NewEffectStore(pool *pgxpool.Pool) *EffectStore {
	// 默认（未注入）：**不使用进程本地时钟**——now 为 nil，nowIn 读数据库
	// 的 clock_timestamp。租约是跨节点共享的事实，判定时刻必须来自同
	// 一个权威时钟源：A 节点时钟慢 10s、B 节点快 10s 时，用各自 time.Now
	// 判对方写的租约，15s 的 TTL 会「从一开始就被判过期」⇒ B 提前接管
	// A 仍在执行的资源（评审第三轮 P0）。注入（sidetest 契约套件）时
	// 用注入值——套件的过期推进语义不变。
	return &EffectStore{pool: pool, now: nil}
}

// NewTenantEffectStore 构造**租户作用域**的副作用闸存储（issue #275）。
//
// 与 coord.NewTenantBoardAdapter 同款装配期声明：所有方法从 ctx 取租户
// （tenant.Require），缺租户 ⇒ tenant.ErrNoContext，写入被拒；Action.Tenant
// 可与 ctx 一致（推荐显式带上，便于排障），不一致 ⇒ ErrTenantMismatch。
//
// 存储键是 (tenant, key) / (tenant, resource)：跨租户同 Key 互不命中。
func NewTenantEffectStore(pool *pgxpool.Pool) *EffectStore {
	return &EffectStore{pool: pool, tenantScoped: true}
}

// TenantScoped 报告本实例是否声明了租户作用域（装配期自检用）。
func (s *EffectStore) TenantScoped() bool { return s.tenantScoped }

// prepareAction 入口处的租户解析：把作用域结论落回 Action.Tenant，后续
// 全部 SQL 用 act.Tenant 做联合定位（解析只在入口发生一次）。
func (s *EffectStore) prepareAction(ctx context.Context, act sideeffect.Action) (sideeffect.Action, error) {
	t, err := resolveTenant(ctx, s.tenantScoped, act.Tenant)
	if err != nil {
		// 批内租户拒绝 ⇒ 登记失败：调用方**可能吞掉**它 ⇒ 本批必须作废。
		// （八个入口共用这一处 ⇒ 覆盖面不缺。）
		failBatch(ctx, err)
		return act, err
	}
	act.Tenant = t
	return act, nil
}

// UseClock 注入时钟（sidetest 契约套件用）。
//
// 过期判定必须读它，否则过期场景恒不触发（套件的硬要求）。
// 注入值代表**系统时间**的测试替身（整个判定链的唯一时间源），不是
// 「某个 worker 的本地时钟」——修复后的实现里 worker 根本不提供时间，
// 这正是「租约统一数据库时钟」要达成的状态。
func (s *EffectStore) UseClock(now func() time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if now != nil {
		s.now = now
	}
}

// nowIn 事务内的当前时刻：注入了测试时钟（sidetest 契约套件）→ 用注入值；
// 否则读**数据库时间**（clock_timestamp——跨节点判定的权威时钟源）。
//
// 调用位置有硬要求：必须在**取得行锁之后**。CURRENT_TIMESTAMP 固定在事务
// 开始时刻，事务若在行锁上等了几秒，用它（或事务开头取的任何时间）判过期
// 就会把「等锁时长」算进租约寿命里——判定基准必须锚在「状态已锁定」那一刻。
func (s *EffectStore) nowIn(ctx context.Context, tx pgx.Tx) (time.Time, error) {
	s.mu.Lock()
	injected := s.now
	s.mu.Unlock()
	if injected != nil {
		return injected(), nil
	}
	var t time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&t); err != nil {
		return time.Time{}, errors.Wrap(err, "pgstore: 读数据库时间失败")
	}
	return t, nil
}

// runTx 事务骨架：fn 报错即整体回滚（不落任何一行），成功才提交。
//
// # ⚠ 通用规则：ctx 里已有 tx ⇒ 复用它（**这条规则不是 Apply 专属**）
//
// ctx 里若已有 tx（**任何**调用方用 pgstore.WithTx 放进去的），runTx 就
// **复用它**——不新开事务、**也不提交**（提交权归放 tx 的那一方）。否则照旧
// 自己开一个（自开自提交）。
//
// 这一处改动让门面暴露的各个面（**尾随游标面除外**）支持跨面原子提交：
// Apply 开一个 tx 塞进 ctx，各面方法经 withinTx/withinTx2/withinTx3 → runTx
// 拿到同一个 tx ⇒ 跨面变更落进同一次提交。
//
// ⚠ **尾随游标面为什么被排除**：它的 SaveCursor 走 pool.Exec ⇒ 独立提交
// ⇒ 批内写游标会越过被回滚的事件 ⇒ 重启后跳读 ⇒ **静默丢帧** ⇒ 批内
// **显式拒绝**（rejectBatchCursorWrite；租户作用域尾随游标同理）——是**响亮
// 拒绝**，不是静默绕过。
//
// ⚠ **对既有 WithTx 使用者是语义变化**（仓外宿主需知）：
//   - 此前：runTx 一律自开事务 ⇒ 宿主经 WithTx 放进去的 tx **被面忽略**
//     （面写进自己那条独立事务并自行提交）⇒ 宿主的事务与面的写**毫无关系**。
//   - 现在：面 **join 宿主那笔 tx**，提交权归宿主 ⇒ 宿主的事务才真的罩得住
//     面的写。
//   - 仓内**零回归**：pgstore.WithTx 在本仓只有一处调用（sink.go 的栅栏
//     写入体）——它本来就该与预占同事务（契约要求「不得部分写」）。
//
// ⚠ **改的是 runTx，不是 withinTx**：withinTx/withinTx2/withinTx3 是
// **泛型变体族**（按返回值个数分，见下方），三者的骨架都是 runTx ⇒ 只改
// runTx 一处，三个变体全部受益 ⇒ 不必给 30 个方法各写 InTx 变体。
//
// # ⚠ PG 侧的嵌套事务不会挂死，但会**破坏原子性**
//
// pool 有多条连接 ⇒ 面若在批内自开事务，内层拿到的是**另一条连接** ⇒
// 独立提交 ⇒ 外层回滚时内层已落库 ⇒ 同样是「一半已提交」，而且**没有报错**
// （SQLite 侧同形态是挂到 ctx 超时——响亮失败；PG 侧是静默半提交，更坏）。
// ⇒ 批内拿不到本批事务句柄时**拒绝自开事务**：响亮失败好过静默半提交。
//
// # 失败登记
//
// 批内 fn 报错 ⇒ 登记进共享失败槽（failBatch）：调用方**可能吞掉**这个错误
// ⇒ 本批必须作废。批外自开事务时没有批次可作废 ⇒ 登记是空操作。
func runTx(ctx context.Context, pool *pgxpool.Pool, fn func(tx pgx.Tx) error) error {
	if tx, ok := TxFromContext(ctx); ok && tx != nil {
		// 批内：复用本批的 tx（不开事务、不提交——提交权在最外层 Apply）。
		if err := fn(tx); err != nil {
			failBatch(ctx, err)
			return err
		}
		return nil
	}
	if slot := batchFrom(ctx); slot != nil {
		// 批内却没有本批的事务句柄 ⇒ 面自开事务 ⇒ 独立提交 ⇒ 破坏跨面原子性。
		// 拒绝（并登记失败 ⇒ 整批作废）。
		err := errors.New("pgstore: 批次内禁止面自开事务（独立提交会破坏跨面原子性）")
		slot.record(err)
		return err
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return errors.Wrap(err, "pgstore: 开事务失败")
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return errors.Wrap(err, "pgstore: 提交事务失败")
	}
	return nil
}

// withinTx / withinTx2 / withinTx3 事务骨架的按返回值个数分的变体
// （Go 泛型变参表达不了「N 个返回值 + error」，拆具名变体最直白）。
//
// 「事务」是本实现全部原子性的来源——契约允许「一条语句**或**一个事务」，
// 本实现选事务：Acquire 的三判合一读多写多，一条语句表达不了。
func withinTx[T any](ctx context.Context, pool *pgxpool.Pool, fn func(tx pgx.Tx) (T, error)) (T, error) {
	var zero T
	err := runTx(ctx, pool, func(tx pgx.Tx) error {
		out, err := fn(tx)
		if err != nil {
			return err
		}
		zero = out
		return nil
	})
	return zero, err
}

func withinTx2[T1, T2 any](ctx context.Context, pool *pgxpool.Pool, fn func(tx pgx.Tx) (T1, T2, error)) (T1, T2, error) {
	var zero1 T1
	var zero2 T2
	err := runTx(ctx, pool, func(tx pgx.Tx) error {
		out1, out2, err := fn(tx)
		if err != nil {
			return err
		}
		zero1, zero2 = out1, out2
		return nil
	})
	return zero1, zero2, err
}

func withinTx3[T1, T2, T3 any](ctx context.Context, pool *pgxpool.Pool, fn func(tx pgx.Tx) (T1, T2, T3, error)) (T1, T2, T3, error) {
	var zero1 T1
	var zero2 T2
	var zero3 T3
	err := runTx(ctx, pool, func(tx pgx.Tx) error {
		out1, out2, out3, err := fn(tx)
		if err != nil {
			return err
		}
		zero1, zero2, zero3 = out1, out2, out3
		return nil
	})
	return zero1, zero2, zero3, err
}
