// store.go —— 跨面 Apply：PG 参考实现的门面（11 个面访问器 + 同一次提交）。
//
// # 为什么要有它
//
// 各面各自 Begin/Commit ⇒ 「任务做完了但没留痕」「留痕了但没做完」两类半截
// 状态结构性地可能。Apply 把各面的变更收进**同一次提交** ⇒ 跨面不会出现
// 「一半已提交」。
//
// # ⚠ 与 SQLite 侧的表层形态**必然不同**（机制决定，不是漏做）
//
//   - SQLite 侧（sqliteadapter（v0.2.0 自 goagent-kernel 迁入））：Apply(ctx, fn func(tx *Tx) error) error
//     —— 它的面**在构造时绑定 tx**（execIn 造一次面内执行位置）⇒ 一次绑定
//     即可，不必给 30 个方法各写 InTx 变体。
//   - PG 侧（本文件）：Apply(ctx, fn func(ctx context.Context) error) error
//     —— 它的面**按调用**从 ctx 里取 tx（面本身只持有 pool）⇒ 若也用
//     fn func(tx *Tx) error，调用方还得把 tx 塞回**每次方法调用**的 ctx
//     ⇒ 等于回到「逐方法开变体」（计划明令避免的）。
//
// ⇒ 两边都不必逐方法开变体，但表层形态必然不同：读到这里的人不必去另一侧
// 找「漏掉的 InTx 变体」。
//
// # ⚠ PG 侧的嵌套事务**不会挂死**，但会**破坏原子性**
//
// SQLite 侧嵌套事务 = 外层占着唯一连接、内层永远等 ⇒ 挂到 ctx 超时（响亮
// 失败）。PG 侧 pool 有多条连接 ⇒ 内层拿到**另一条连接** ⇒ **独立提交**
// ⇒ 外层回滚时内层已落库 ⇒ 同样是「一半已提交」，而且**一声不吭**。
//
// ⇒ 批内**禁止面自开事务**：runTx 认 ctx 里已有的 tx（复用：不开、不提交），
// 拿不到本批事务句柄时**拒绝自开**；Apply 不可重入（嵌套 Apply 会独立提交）。
//
// # 失败即作废整批（即使调用方吞掉了内层的错误）
//
// 内层任一面失败都被记进**共享失败槽**（ctx 里的 failSlot）：调用方吞掉错误
// 继续写别的面时，Apply 仍会发现「本批已失败」⇒ 整批回滚。缺这条，「任务做
// 完了但没留痕」与「留痕了但没做完」两类半截状态就会重现——这正是本门面存在
// 的理由。
//
// 失败槽是**记录点**，不是判定点：判定只有一处（Apply 的提交闸）。两处都判
// ⇒ 同一个判断有两个主人 ⇒ 改坏一处测试看不出区别。
//
// # 承诺只覆盖**写**
//
// 失败槽登记的是**写路径**的失败（事务骨架 runTx 内的失败 + 各面进事务
// 之前的写前拒绝）。**读路径**的失败不进失败槽（List / Load / MaxID /
// ListAfter / SeenFence / Marker）：读不产生状态变更，「全有或全无」的承诺
// 针对写。
//
// # ⚠ 批内的读**看不见**本批的未提交写（与 SQLite 侧不同）
//
// PG 的面读走 pool（MVCC 已提交快照）⇒ 批内读面读到的是**提交前**的状态；
// SQLite 的读走 exec.reader 的路由（tx != nil ⇒ 用 tx）⇒ 看得见本批未提交写。
// ⇒ 批内要读自己刚写的内容，走公开接缝 TxFromContext 取事务句柄自己查。
//
// # 错误身份**不对称**（按 == 分流的调用方会漏）
//
//   - **回调自己的错误**：**等值**传出（err == sentinel 可判）；
//   - **被吞掉的失败**：errors.Wrap 传出（errors.Is 可判、== 不可判）——
//     包装带上「本批有一面失败，整批作废」的现场；且它本身是面级错误
//     （已被面级包装过），== 本来就判不到。
//
// ⇒ **按 errors.Is 分流的调用方两种都判得到**；按 == 分流的调用方只会判到
// 第一种（这是刻意的：批内失败的现场不该被抹掉）。
package pgstore

import (
	"context"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/oekit/pkg/errors"

	"github.com/yi-nology/goagent-kernel/controlplane"
)

// Store 聚合 PG 参考实现的 11 个面访问器，供跨面原子提交用。
//
// 这 11 个面共用**同一个 pool** ⇒ 同一个数据库、同一个 schema ⇒ 跨面事务有落点。
type Store struct {
	pool *pgxpool.Pool
}

// NewStore 构造门面。pool 不得为 nil（装配期编程错误 fail fast）。
func NewStore(pool *pgxpool.Pool) *Store {
	if pool == nil {
		panic("pgstore: Store 需要连接池（nil pool 无从读写）")
	}
	return &Store{pool: pool}
}

// Board 任务面。
func (s *Store) Board() *BoardStore { return NewBoardStore(s.pool) }

// Effects 副作用面。
func (s *Store) Effects() *EffectStore { return NewEffectStore(s.pool) }

// Snapshots 进度快照面。
func (s *Store) Snapshots() *SnapStore { return NewSnapStore(s.pool) }

// Journal 工具调用账本面（invocation.Store）。
//
// ⚠ 这个面**刻意不参与 Apply 批次**：账本必须在副作用**之前**落库
// （P0-09 闭合的前提：崩溃后按账本续跑，不再问模型要那次调用），而批次
// 的提交点在批次**之后** ⇒ 参与批次就等于把「账本晚于副作用」变成常态
// ⇒ 重复施加的窗口重新打开。它走自己的事务（见 journal.go 的包头）。
//
// 两侧能力面对位：SQLite 侧同款入口是 sqliteadapter.Store.Journal()。
func (s *Store) Journal() *JournalStore { return NewJournalStore(s.pool) }

// Events 事件面。
func (s *Store) Events() *EventStore { return NewEventStore(s.pool) }

// # 下面 7 个访问器：评审 I-3 的**追加项**（brief 的 Produces 只列了上面四个）
//
// 为什么补：Task 9 的选型门禁要比对**门面可达性**——判「宿主只持门面时能做
// 什么」。而 PG 侧 `Store.pool` **未导出** ⇒ 宿主只持 `*pgstore.Store` 时
// 构造不出租户作用域面、栅栏面、游标面、控制面 ⇒ 两侧门面能力面**不等价**，
// 而「符号可解析」那条判据**看不见这个不对称**（典型的「门禁绿 = 没判」）。
//
// ⇒ 补齐后两侧门面暴露**同款集合** ⇒ 对位成为**事实**，而不是口径上的声明。
//
// 形态差异只有一处：**租户解析**。租户作用域形态每次读写都从 ctx 取租户
// （缺 ⇒ tenant.ErrNoContext，fail-closed）；默认形态不解析租户（缺租户
// 照常可用）。其余语义与各自的独立构造器逐字相同。
//
// TenantEffects 租户作用域的副作用面。
func (s *Store) TenantEffects() *EffectStore { return NewTenantEffectStore(s.pool) }

// TenantEvents 租户作用域的事件面。
func (s *Store) TenantEvents() *EventStore { return NewTenantEventStore(s.pool) }

// Downstream 下游栅栏面。
func (s *Store) Downstream() *Downstream { return NewDownstream(s.pool) }

// TenantDownstream 租户作用域的下游栅栏面。
func (s *Store) TenantDownstream() *Downstream { return NewTenantDownstream(s.pool) }

// TailCursors 尾随游标面（consumer key 不得为空——空键会让所有尾随者共享
// 一条游标 ⇒ 后跑的推过对方整段窗口 ⇒ 订阅侧静默漏帧；fail fast 在构造点）。
func (s *Store) TailCursors(consumer string) *TailCursorStore {
	return NewTailCursorStore(s.pool, consumer)
}

// TenantTailCursors 租户作用域的尾随游标面（consumer key 不得为空，同上）。
func (s *Store) TenantTailCursors(consumer string) *TenantTailCursorStore {
	return NewTenantTailCursorStore(s.pool, consumer)
}

// Control 控制面（审计器由宿主注入；为 nil 时只能读不能提交，见 NewControlStore）。
func (s *Store) Control(aud controlplane.Auditor) *ControlStore {
	return NewControlStore(s.pool, aud)
}

// Apply 在**一个事务**里提交各面的变更（门面暴露的 11 个面都在同一次提交里）。
//
// 语义：
//   - 回调返回错误 ⇒ 整批丢弃，该错误**原样**传出（不被包装）；
//   - 回调返回 nil，但批内任一面失败过 ⇒ 整批仍丢弃，并报出那条失败
//     （失败被调用方吞掉时，返回 nil 等于谎报「整批提交了」）；
//   - 回调返回 nil 且批内无失败 ⇒ 各面的变更作为**一次提交**落库。
//
// 实现：开一个 pgx.Tx 塞进 ctx（pgstore.WithTx，既有接缝），各面方法经
// withinTx/withinTx2/withinTx3 → runTx 认到它 ⇒ 复用同一个 tx ⇒ 跨面变更
// 落进同一次提交。
//
// ⚠ 错误身份**不对称**（见包头）：回调自己的错误等值传出，被吞掉的失败
// errors.Wrap 传出 ⇒ **按 errors.Is 分流**，按 == 分流会漏第二种。
//
// ⚠ Apply **不可重入**（嵌套 Apply ⇒ 内层从 pool 拿另一条连接 ⇒ 独立提交
// ⇒ 破坏跨面原子性 ⇒ 显式拒绝）。
//
// ⚠ **Seed 装载入口不在批内**（它没有 ctx，自带一次提交）⇒ 任务拓扑的初始
// 装载必须在批外做；批内只做认领 / 归还 / 完成 / 续期。
func (s *Store) Apply(ctx context.Context, fn func(ctx context.Context) error) error {
	if fn == nil {
		return errors.New("pgstore: Apply 的回调不得为 nil（空回调会提交一个空批）")
	}
	if batchFrom(ctx) != nil {
		// 嵌套 Apply：内层会从 pool 拿**另一条连接**开事务 ⇒ 独立提交 ⇒
		// 外层回滚时内层已落库 ⇒ 跨面原子性静默失效（且没有报错）。
		err := errors.New("pgstore: Apply 不可重入（嵌套 Apply 会独立提交，破坏跨面原子性）")
		// 登记进外层的失败槽：调用方若吞掉这个拒绝，外层批次也必须作废。
		failBatch(ctx, err)
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return errors.Wrap(err, "pgstore: 开事务失败")
	}
	// 回滚兜底：回调 panic 时事务必须被释放（连接泄漏 = 后续写全挂）。
	// 提交成功后这句返回「事务已关闭」类错误（忽略即可）。
	defer func() { _ = tx.Rollback(ctx) }()

	// 失败槽：各面共享**同一份** ⇒ 任一面失败，其余面都看得见。
	slot := &failSlot{}
	inner := withBatch(WithTx(ctx, tx), slot)

	cbErr := fn(inner)
	switch {
	case cbErr != nil:
		// 回调自己的错误：原样传出（调用方按自己的哨兵分流）。
		_ = tx.Rollback(ctx)
		return cbErr
	case slot.load() != nil:
		// 内层失败被调用方吞掉了 ⇒ 整批仍必须作废，并把失败报出来。
		_ = tx.Rollback(ctx)
		return errors.Wrap(slot.load(), "pgstore: 本批有一面失败，整批作废")
	}
	if err := tx.Commit(ctx); err != nil {
		return errors.Wrap(err, "pgstore: 提交事务失败")
	}
	return nil
}

// batchKey 批次状态在 ctx 里的键（私有：外部造不出「在批次里」的 ctx）。
type batchKey struct{}

// withBatch 把本批的失败槽放进 ctx（Apply 内部用）。
func withBatch(ctx context.Context, slot *failSlot) context.Context {
	return context.WithValue(ctx, batchKey{}, slot)
}

// batchFrom 读本批的失败槽；nil ⇒ 不在任何批次里。
func batchFrom(ctx context.Context) *failSlot {
	slot, _ := ctx.Value(batchKey{}).(*failSlot)
	return slot
}

// failBatch 登记本批的一次失败（不在批次里 ⇒ 空操作）。
//
// 为什么要有这个入口：失败可能发生在**进事务之前**（租户解析拒绝、快照的
// 落库前校验、对账的入参校验）⇒ 那些路径也得能登记，否则「失败即作废整批」
// 对它们是假的。
func failBatch(ctx context.Context, err error) {
	if slot := batchFrom(ctx); slot != nil {
		slot.record(err)
	}
}

// failSlot 本批第一次失败（共享失败槽）。
//
// 指针语义：各面共享**同一份**槽 ⇒ 任一面失败，其余面都看得见。
//
// 为什么用互斥锁而不是裸 *error：PG 的批次里允许并发调用（pool 多连接）⇒
// 裸指针会被并发写撕裂（race 门禁会报）。SQLite 侧用裸 *error 是因为它的写
// 句柄只有一条连接，批次内不可能并发——两边的刻度差异来自引擎，不是随意。
type failSlot struct {
	mu  sync.Mutex
	err error
}

// record 登记本批的一次失败（只记**第一次**）。
func (s *failSlot) record(err error) {
	if err == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err == nil {
		s.err = err
	}
}

// load 读本批登记的失败（没有则 nil）。
func (s *failSlot) load() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}
