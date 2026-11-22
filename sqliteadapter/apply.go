// apply.go —— 跨面 Apply：各面的变更收进**同一次提交**。
//
// # 为什么要有它
//
// 各面各自提交 ⇒ 「任务做完了但没留痕」「留痕了但没做完」两类半截状态
// 结构性地可能。Apply 把各面的变更收进**同一次提交** ⇒ 跨面不会出现
// 「一半已提交」。
//
// # ⚠ 嵌套事务会死锁（表现为 ctx 超时，不是干净报错）
//
// 写句柄 MaxOpenConns(1)；exec.run 只在 e.tx == nil 时才 BeginTx；Store.exec()
// **永远造 tx-less 的 exec** ⇒ **各面各开事务 ⇒ 第二个面等第一个面的连接
// ⇒ 永久等 ⇒ 挂到 ctx 超时**。
//
// ⇒ 正确形态：**Apply 开一个事务，把同一个 tx 贯穿各面**（tx.Tasks() 等访问器
// 造的面都挂在这一条 tx 上）。
// ⇒ **禁止**在 Apply 的事务内调任何走 s.exec() / s.exec().run(...) 的路径
// （那是嵌套事务 ⇒ 挂死，不是干净报错）——**包括 s.Downstream()**：
// 栅栏面也有批内形态（tx.Downstream()），用门面的那个会自开事务 ⇒ 挂死。
//
// # 失败即作废整批（即使调用方吞掉了内层的错误）
//
// 内层任一面失败都会被记进**共享失败槽**（exec.failed）：调用方吞掉错误继续
// 写别的面时，Apply 仍会发现「本批已失败」⇒ 整批回滚。缺这条，「任务做完了
// 但没留痕」与「留痕了但没做完」两类半截状态就会重现——这正是本包存在的理由。
//
// 失败槽是**记录点**，不是判定点：判定只有一处（下面的提交闸）。两处都判 ⇒
// 同一个判断有两个主人 ⇒ 改坏一处测试看不出区别。
//
// # 承诺只覆盖**写**
//
// 失败槽登记的是**写路径**的失败：① exec.run 的事务内分支（回调返回的错误
// 一律登记）；② **进 exec.run 之前**的写前拒绝（租户解析拒绝、快照的落库前
// 校验、对账的入参校验）——这些显式调 exec.fail 登记。
//
// ⚠ **读路径**的失败**不进**失败槽（List / Load / MaxID / ListAfter /
// SeenFence / Marker）：读不产生状态变更，「全有或全无」的承诺针对写。
// ⇒ 调用方吞掉一条读错误时，批次仍会提交（写仍是全有或全无）；读失败的处置
// 留给调用方。
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
//
// # ⚠ Apply **不把 tx 放进 ctx**
//
// 批内直接调 ExecWithin(ctx, …) 会报「业务写入必须从 WriteFenced 的写入体内
// 执行」——因为事务句柄只在栅栏面的**写入体**里（WithTx 注入）。
// ⇒ 批内要写业务数据，走 tx.Downstream().WriteFenced 的写入体（那里才有
// 事务句柄）；其余各面用自己的批内方法。
//
// # 租户作用域在批内
//
// 批内也有两种形态：默认形态（租户取动作/事件自带的值）与**租户作用域形态**
// （tx.Tenant* 访问器）：后者每次读写都从 ctx 取租户（缺 ⇒ tenant.ErrNoContext
// ——fail-closed），且「声明的租户 ≠ ctx 租户 ⇒ ErrTenantMismatch」的第三段
// **在批内才真正生效**。
//
// ⇒ 批内事件面写 ctx 租户的流、批外按 ctx 租户读 ⇒ 两侧一致（默认形态的读面
// 固定读 "" 流 ⇒ 与批内写的租户流**不一致**，这是刻意的分区效果）。
//
// # 事务内的读一律走 tx
//
// 事务内的读走 exec.reader 的路由（tx != nil ⇒ 用 tx）：读-判-写原子性靠这条
// 保证；绕开 tx 用 db 直查会与占着唯一连接的写事务抢连接 ⇒ 挂到 ctx 超时。
//
// # 时钟
//
// 批内新建的面实例拿不到「调用方注入到某个面实例上的面级时钟」（面级 clock
// 是注入到**那个面实例**上的）⇒ 批内按各面 clockSource() 解析（面级 →
// Store 级 → nil）。
//
// ⚠ **面级时钟注入不跨 Apply 边界**：s.Board().UseClock(f) 注入的时钟在
// tx.Tasks() 造的新面实例上取不到（那是个新实例）。要批内也用注入时钟，
// 在 Store 级注入（Store.UseClock）——这是本任务明确记录的取舍，不是漏了。
package sqliteadapter

import (
	"context"
	"database/sql"

	"github.com/oekit/pkg/errors"
)

// Tx 一次事务的各面（共享同一份状态与同一次提交）。
//
// ⚠ 各面共用**同一个** tx ⇒ 同一次提交；不是一个面一个事务（那会死锁，
// 见包头「嵌套事务会死锁」）。
type Tx struct {
	// tx 本批的事务句柄（各面共用同一个）。
	tx *sql.Tx
	// store 归属的 Store（面实例的时钟来源与只读句柄的来源）。
	store *Store
	// failed 本批第一次失败（共享失败槽；nil ⇒ 该面不在批次里）。
	failed *error
}

// exec 造一个事务内的执行位置，并挂上本批的失败槽。
//
// 时钟传 nil：批内按各面 clockSource() 解析（面级 → Store 级 → nil），
// 详见包头「时钟」。
//
// ⚠ **失败槽在这一层挂上** ⇒ 各面（含租户作用域形态）**共享同一份槽** ⇒
// 任一面失败，其余面都看得见（「失败即作废整批」的前提）。
func (tx *Tx) exec() exec {
	e := tx.store.execIn(tx.tx, nil)
	e.failed = tx.failed
	return e
}

// Tasks 任务面（事务内形态）。
//
// 事务内的写走本批的 tx（不再自开事务）；读走 exec.reader 的路由（tx != nil
// ⇒ 用 tx ⇒ 读-判-写原子）。
func (tx *Tx) Tasks() *BoardStore {
	return &BoardStore{exec: tx.exec(), store: tx.store}
}

// TenantTasks 任务面（事务内形态 · 租户作用域）。
//
// ⚠ **任务面没有租户维度**（gak_board_task 无 tenant 列，与 PG 侧
// pgref_board_task 一致）⇒ 本访问器与 Tasks() **同款**。
//
// 保留它的理由是**API 对称**：宿主按「本进程跑在租户作用域」这一维度切换
// 各面的批内形态时，不必对任务面特判（否则「忘了特判」就落进默认形态）。
// 它**不是**一个开关：两个形态都真在批内、都继承批内失败槽。
func (tx *Tx) TenantTasks() *BoardStore {
	return &BoardStore{exec: tx.exec(), store: tx.store}
}

// Effects 副作用面（事务内形态）。
func (tx *Tx) Effects() *EffectStore {
	return &EffectStore{exec: tx.exec(), store: tx.store}
}

// TenantEffects 副作用面（事务内形态 · 租户作用域）。
//
// 与 Effects() 的差别只有一处：**租户解析**。本形态每次调用都从 ctx 取租户
// （缺 ⇒ tenant.ErrNoContext，fail-closed）；动作自带的 Tenant 与 ctx 不一致
// ⇒ ErrTenantMismatch（**第三段在批内才生效**——默认形态的批内面租户取
// 动作自带值，永远不会不一致）。
//
// ⇒ 多租户宿主在批内写副作用时，**没有第二条路**能把 A 租户的数据写进
// B 租户或 "" 命名空间：要么声明与 ctx 一致，要么被拒。
func (tx *Tx) TenantEffects() *EffectStore {
	return &EffectStore{exec: tx.exec(), store: tx.store, tenantScoped: true}
}

// Snapshots 进度快照面（事务内形态）。
//
// ⚠ 与门面形态的关键差别：SaveIfNewer 在门面形态下**自开事务**，在批内形态下
// 复用本批的 tx（⇒ 快照与各面的变更同生共死）。
func (tx *Tx) Snapshots() *SnapStore {
	return &SnapStore{exec: tx.exec()}
}

// TenantSnapshots 进度快照面（事务内形态 · 租户作用域）。
//
// ⚠ **快照面没有租户维度**（gak_snapshot 的主键是 run_id，无 tenant 列，
// 与 PG 侧一致）⇒ 本访问器与 Snapshots() **同款**。
//
// 保留它的理由同 TenantTasks（API 对称）；它**不是**一个开关：两个形态都真
// 在批内、都继承批内失败槽。
func (tx *Tx) TenantSnapshots() *SnapStore {
	return &SnapStore{exec: tx.exec()}
}

// Events 事件面（事务内形态）。
//
// ⚠ 批内请用 Insert：它经 exec.run ⇒ 复用本批的 tx，且失败会记进失败槽。
// PublishInTx 需要调用方自己拿 *sql.Tx（Tx 不暴露它），批内用 Insert 等价
// （两者落同一张表、同一套水位线，差别只在「谁负责提交」）。
func (tx *Tx) Events() *EventStore {
	return &EventStore{exec: tx.exec()}
}

// TenantEvents 事件面（事务内形态 · 租户作用域）。
//
// 与 Events() 的差别只有一处：**租户解析**。本形态的写落在 ctx 租户的流上
// （水位行按租户分行 ⇒ 每个租户一条独立 ID 空间），读面也按 ctx 读 ⇒ 批内
// 写与批外读**一致**（默认形态的读面固定读 "" 流 ⇒ 与租户流不一致，这是
// 刻意的分区效果）。
func (tx *Tx) TenantEvents() *EventStore {
	return &EventStore{exec: tx.exec(), tenantScoped: true}
}

// Downstream 下游栅栏面（事务内形态）。
//
// ⚠ **必须用批内形态**：门面形态 s.Downstream() 的 WriteFenced 会**自开
// 事务** ⇒ 在批内调它 = 嵌套事务 ⇒ 外层 tx 占着唯一连接、内层永远等 ⇒
// 挂到 ctx 超时（不是干净报错）。
//
// 批内形态的 WriteFenced 复用本批的 tx ⇒ 栅栏预占与写入体的业务数据一起随
// 批提交/回滚（不得部分写这条契约在批内同样成立）。
func (tx *Tx) Downstream() *Downstream {
	return &Downstream{exec: tx.exec()}
}

// TenantDownstream 下游栅栏面（事务内形态 · 租户作用域）。
//
// 与 Downstream() 的差别只有一处：**租户解析**。本形态的资源键是
// (ctx 租户, resource) ⇒ 同一 resource 在两个租户下是两条独立记录；缺租户
// ⇒ tenant.ErrNoContext（fail-closed，且该拒绝会被登记进失败槽 ⇒ 整批作废）。
func (tx *Tx) TenantDownstream() *Downstream {
	return &Downstream{exec: tx.exec(), tenantScoped: true}
}

// Apply 在**一个事务**里提交各面的变更。
//
// 语义：
//   - 回调返回错误 ⇒ 整批丢弃，该错误**原样**传出（不被包装）；
//   - 回调返回 nil，但批内任一面失败过 ⇒ 整批仍丢弃，并报出那条失败
//     （失败被调用方吞掉时，返回 nil 等于谎报「整批提交了」）；
//   - 回调返回 nil 且批内无失败 ⇒ 各面的变更作为**一次提交**落库。
//
// ⚠ 错误身份**不对称**（见包头）：回调自己的错误等值传出，被吞掉的失败
// errors.Wrap 传出 ⇒ **按 errors.Is 分流**，按 == 分流会漏第二种。
//
// ⚠ **禁止**在回调里调任何走 s.exec() / s.exec().run(...) 的路径（如
// s.Board().Claim、s.Downstream().WriteFenced）：那是嵌套事务 ⇒ 外层 tx 占着
// 唯一连接、内层永远等 ⇒ 挂到 ctx 超时（不是干净报错）。批内一律用
// tx.Tasks() / tx.Downstream() 等事务内形态。
//
// ⚠ Apply 不可重入（回调里再 Apply ⇒ 同样的嵌套事务死锁）。
func (s *Store) Apply(ctx context.Context, fn func(tx *Tx) error) error {
	if fn == nil {
		return errors.New("sqliteadapter: Apply 的回调不得为 nil（空回调会提交一个空批）")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return errors.Wrap(err, "sqliteadapter: 开事务失败")
	}
	// 回滚兜底：回调 panic 时事务必须被释放——写句柄只有一条连接，泄漏连接
	// = 后续所有写挂死。提交成功后这句返回 sql.ErrTxDone（忽略即可）。
	defer func() { _ = tx.Rollback() }()

	// 失败槽：各面共享**同一份** ⇒ 任一面失败，其余面都看得见。
	var failed error
	inner := &Tx{tx: tx, store: s, failed: &failed}

	cbErr := fn(inner)
	switch {
	case cbErr != nil:
		// 回调自己的错误：原样传出（调用方按自己的哨兵分流）。
		_ = tx.Rollback()
		return cbErr
	case failed != nil:
		// 内层失败被调用方吞掉了 ⇒ 整批仍必须作废，并把失败报出来。
		_ = tx.Rollback()
		return errors.Wrap(failed, "sqliteadapter: 本批有一面失败，整批作废")
	}
	if err := tx.Commit(); err != nil {
		return errors.Wrap(err, "sqliteadapter: 提交事务失败")
	}
	return nil
}
