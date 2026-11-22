// control.go —— controlplane.Store 的 SQLite 参考实现（对位 pgstore（v0.2.0 自 goagent-kernel 迁入）/control.go）。
//
// # 它补的是哪一段
//
// 控制面此前只有进程内 MemStore：两个副本各持一份状态，A 的强制释放 /
// 裁决 / 终止在 B 的视图里不可见（issue #189）。本实现把 Store 落到真
// SQLite，并**复用 gak_board_task 作为唯一的可写任务状态**：
//
//   - 任务快照 / 提交只读写 gak_board_task（与 taskboard.Board 同一张表）；
//   - 运行记录 gak_run 只存 status / gen，任务成员由任务表的 run_id 列
//     单源表达——不复制一份任务清单；
//   - 强制释放的权威接线仍是 NewAuthoritativeStorePlane + BoardStore
//     （issue #268）：控制面不绕过任务的令牌 / 代次判据。CommitTask 只是
//     **Store 契约**的后端落点（供非权威装配或测试使用）。
//
// # 三条硬契约的落库形态
//
//   - **快照一致**：三个 Snapshot* 都在一个**只读快照事务**里读版本锚与
//     全部相关行（任务集合不是边改边读的流）——读面在 control_snapshot.go；
//   - **版本锚**：gak_control_version 单行全局修订号；Commit* 先校验
//     expect，不符即 ErrStoreConflict 且什么都不写；提交成功推进修订号；
//     ⚠ **仅由控制面自身的提交推进**——任务板侧的写入（Claim / Complete /
//     Release / Renew）不推进它 ⇒ 「快照之后目标被改过 ⇒ 提交必失败」这条
//     对**控制面之外的写入**检测不到（与 PG 侧 pgref_control_version 逐字
//     同款，不是本实现引入的偏差）⇒ 权威模式下强释走任务板（令牌 / 代次
//     在那里判）、不经过 CommitTask ⇒ 该边界不影响权威路径的判定。
//   - **留痕与状态同事务方向**：Auditor.Append 在版本校验之后、状态写入
//     之前调用；留痕失败 ⇒ 事务回滚，状态一字不动。宿主 Auditor 若落在
//     另一个数据库，跨库原子性不成立——这是宿主账本的边界，与 MemStore
//     同口径（内核要求的是「留痕失败不得改状态」，不是替宿主做两阶段提交）。
//
// # SQLite 与 PG 的两处方言差异（形态同构，机制不同）
//
//   - **一致快照**：PG 用 REPEATABLE READ 只读事务；SQLite 没有等价物 ⇒
//     用**延迟（deferred）读事务**：读事务在首次读时取快照，后续读同一
//     快照（同一瞬间 ⇒ 版本锚与任务集合不会混读）。
//   - **版本锚的行锁**：PG 用 SELECT … FOR UPDATE；SQLite 靠写句柄的
//     BEGIN IMMEDIATE 已持的**写锁**（整个库一把写锁 ⇒ 提交天然串行）。
//
// # ⚠ 嵌套事务会死锁（ctx 超时，不是干净报错）
//
// 写句柄 MaxOpenConns(1)；exec.run 只在 e.tx == nil 时才 BeginTx ⇒ 事务内
// 再调 s.exec().run(...) 就是嵌套事务 ⇒ 外层 tx 占着唯一连接、内层永远等。
// ⇒ 控制面的「校验 → 留痕 → 写状态 → 推进版本」**全在同一个事务**里，
// 面内执行位置用 Store.execIn(tx, clock) 造（唯一的事务内执行位置工厂，
// AST 门禁钉住），**绝不**用 s.exec()。
//
// # 分文件
//
//   - control.go（本文件）：类型 / 装配 / 同源声明 + **写面**（三个 Commit*）；
//   - control_snapshot.go：**读面**（三个 Snapshot*，一致快照事务）；
//   - control_seed.go：**装载入口**（SeedRun / SeedEffect，对位 PG 侧
//     control_seed.go）。
package sqliteadapter

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"github.com/oekit/pkg/errors"

	"github.com/yi-nology/goagent-kernel/controlplane"
	"github.com/yi-nology/goagent-kernel/taskboard"
)

// ControlStore controlplane.Store 的 SQLite 参考实现。
//
// 一个实例绑定一个数据文件；并发安全（提交在版本锚上串行：写事务的写锁把
// 提交排成一队）。aud 为 nil 时所有 Commit* 返回 controlplane.ErrNoAuditor
// （留痕面缺位 ⇒ 介入一律拒绝，与 MemStore 同口径）。
type ControlStore struct {
	// exec 执行位置：db 是写句柄，read 是只读句柄（快照读走它）。
	exec exec
	// aud 留痕面（nil ⇒ 提交一律拒绝）。
	aud controlplane.Auditor
	// src 来源标识（controlplane.TaskSource）：同一数据文件的面报同一值。
	src string
	// store 归属的 Store：面内执行位置的**唯一**工厂来源（Store.execIn）。
	//
	// 为什么必须有它：事务内执行位置的构造点只允许出现在定义 Store.execIn
	// 的那个文件（AST 门禁）⇒ 本包不能手搓 exec 字面量 ⇒ 只能持有一份
	// *Store 去调用共享工厂（与 BoardStore / EffectStore 同款形态）。
	store *Store
}

// 编译期契约断言：Store 契约 + 同源声明 + 权威任务面。
var (
	_ controlplane.Store      = (*ControlStore)(nil)
	_ controlplane.TaskSource = (*ControlStore)(nil)
	// 权威接线（issue #268）：任务板必须同时是 TaskAuthority（强制释放经
	// 它落地，令牌 / 代次在**同一张表**上判）与 TaskSource（装配期同源
	// 校验）——**两侧都要有**：内核的 checkTaskSource 是「双方都实现才
	// 比对」，只实现一方 ⇒ 装配期直接 ErrAuthorityMismatch。
	_ controlplane.TaskAuthority = (*BoardStore)(nil)
	_ controlplane.TaskSource    = (*BoardStore)(nil)
)

// Control 控制面（门面入口：自带独立只读句柄 ⇒ 快照读不与写事务抢连接）。
func (s *Store) Control(aud controlplane.Auditor) *ControlStore {
	return &ControlStore{exec: s.execBase(), aud: aud, src: s.sourceID(), store: s}
}

// TaskSourceID 实现 controlplane.TaskSource（issue #268/#278）：同一数据
// 文件的任务板与控制面报同一值 ⇒ NewAuthoritativeStorePlane 据此在装配期
// 判定「同一张任务表」；不同数据文件 ⇒ 不同来源 ⇒ 装配期拒绝。
func (c *ControlStore) TaskSourceID() string { return c.src }

// clockSource 控制面的时钟来源（Store 级注入）。
//
// 控制面不参与任何到期判定（没有租约 / 意图语义）⇒ 时钟只是**透传**给面内
// 执行位置，不参与判定；仍按「面级 → Store 级 → nil」解析，与其它面同口径。
func (c *ControlStore) clockSource() func() time.Time {
	if c.store == nil {
		return nil
	}
	return c.store.clock
}

// readControlVersion 读版本锚（单行全局修订号）。
func readControlVersion(ctx context.Context, q queryer) (controlplane.Version, error) {
	var v int64
	if err := q.QueryRowContext(ctx,
		`SELECT ver FROM gak_control_version WHERE id = 1`).Scan(&v); err != nil {
		return 0, errors.Wrap(err, "sqliteadapter: 读控制面版本锚失败（EnsureSchema 建表了吗？）")
	}
	return controlplane.Version(v), nil
}

// lockControlVersion 读版本锚（提交路径）。
//
// ⚠ SQLite 没有 SELECT … FOR UPDATE：**写锁**（BEGIN IMMEDIATE 已持有）就是
// 这把锁——整个库一把写锁 ⇒ 两个提交不可能同时进入临界区 ⇒「读-比对-推进」
// 天然是原子的。
func lockControlVersion(ctx context.Context, tx *sql.Tx) (controlplane.Version, error) {
	return readControlVersion(ctx, tx)
}

// appendControlAudit 调宿主留痕面并隔离 panic（与内核 controlplane 的同名
// 接缝同口径：panic 转 ErrAuditorPanic，状态一字不动）。
func appendControlAudit(ctx context.Context, aud controlplane.Auditor, e controlplane.AuditEntry) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = errors.Wrap(controlplane.ErrAuditorPanic,
				"留痕实现 panic（sqliteadapter 已隔离，状态未变）")
		}
	}()
	return aud.Append(ctx, e)
}

// commit 三个 Commit* 的共用骨架：版本锚 → 留痕 → 状态写入 → 推进版本，
// **全部在同一个事务里**。
//
// 留痕失败 ⇒ 整体回滚（状态一字不动）；版本不匹配 ⇒ 什么都不写（含留痕：
// 失败的 CAS 不是一次介入）。
//
// ⚠ 事务边界是这一层的全部要害：写句柄的 BEGIN IMMEDIATE 已持写锁 ⇒ 版本
// 锚的「读-比对-推进」在锁上串行；事务内的读一律走 tx（面内执行位置由
// Store.execIn 造 ⇒ 不手搓 exec 字面量，见 AST 门禁）。
//
// ⚠ **宿主的留痕面不得走同一个写句柄**：写句柄只有一条连接，而本事务已
// 占着它 ⇒ 留痕面再从写句柄取连接 ⇒ 永久等 ⇒ 挂到 ctx 超时（不是干净报错）。
// ⇒ 留痕面要么在进程内（MemAuditor）、要么走另一个句柄 / 另一个库
// （跨库则跨库原子性不成立——宿主账本的边界，与 PG 侧同口径）。
func (c *ControlStore) commit(ctx context.Context, expect controlplane.Version,
	entry controlplane.AuditEntry, fn func(tx *sql.Tx) error) error {
	if c.aud == nil {
		return controlplane.ErrNoAuditor
	}
	if c.store == nil {
		return errors.New("sqliteadapter: 控制面未绑定 Store（造不出事务内执行位置）")
	}
	// ⚠ **为什么这里不直接用 exec.run**：exec.run 的独立形态（tx == nil）
	// 会**自己开一个事务并提交** ⇒ 若在控制面的事务内调它 ⇒ 嵌套事务 ⇒
	// 外层 tx 占着写句柄的唯一连接、内层永远等 ⇒ 挂到 ctx 超时（不是干净
	// 报错）⇒ 事务边界只能由**本层自管**（BeginTx / Commit）；事务内的一
	// 切经下面那个**事务内**执行位置（execIn 造 ⇒ tx != nil ⇒ exec.run
	// 只跑回调，不开事务、不提交）。
	tx, err := c.exec.db.BeginTx(ctx, nil)
	if err != nil {
		return errors.Wrap(err, "sqliteadapter: 开控制面提交事务失败")
	}
	// 回滚兜底：写句柄只有一条连接，事务泄漏 = 后续所有写挂死。
	defer func() { _ = tx.Rollback() }()

	// 面内执行位置：事务内的读一律走 tx（路由判据见 exec.reader）。
	in := c.store.execIn(tx, c.clockSource())
	if err := in.run(ctx, func(tx *sql.Tx) error {
		v, err := lockControlVersion(ctx, tx)
		if err != nil {
			return err
		}
		if v != expect {
			return errors.Wrap(controlplane.ErrStoreConflict,
				"控制面状态在快照之后已被另一个副本修改")
		}
		if err := appendControlAudit(ctx, c.aud, entry); err != nil {
			return err
		}
		if err := fn(tx); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE gak_control_version SET ver = ver + 1 WHERE id = 1`); err != nil {
			return errors.Wrap(err, "sqliteadapter: 推进控制面版本锚失败")
		}
		return nil
	}); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return errors.Wrap(err, "sqliteadapter: 提交控制面事务失败")
	}
	return nil
}

// CommitTask 实现 controlplane.Store：原子提交任务状态 + 留痕。
//
// 状态写在 gak_board_task（与 taskboard.Board **同一张可写表**）；不存在的
// 任务 ⇒ ErrNotFound 且不推进版本。
func (c *ControlStore) CommitTask(ctx context.Context, expect controlplane.Version,
	next taskboard.Task, entry controlplane.AuditEntry) error {
	return c.commit(ctx, expect, entry, func(tx *sql.Tx) error {
		var one int
		if err := tx.QueryRowContext(ctx,
			`SELECT 1 FROM gak_board_task WHERE id = ?`, string(next.ID)).Scan(&one); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return errors.Wrap(controlplane.ErrNotFound, "任务不存在: "+string(next.ID))
			}
			return errors.Wrap(err, "sqliteadapter: 锁任务行失败", "task", string(next.ID))
		}
		deps, err := encodeDeps(next.Deps)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE gak_board_task
			    SET title = ?, deps = ?, state = ?, owner = ?, token = ?,
			        fence = ?, until = ?, seq = ?
			  WHERE id = ?`,
			next.Title, deps, string(next.State),
			nullableString(string(next.Owner)), nullableString(next.Lease.Token),
			int64(next.Lease.Fence), nullableMillis(next.Lease.Until),
			int64(next.Seq), string(next.ID)); err != nil {
			return errors.Wrap(err, "sqliteadapter: 写任务行失败", "task", string(next.ID))
		}
		return nil
	})
}

// CommitRun 实现 controlplane.Store：原子提交运行状态/代次 + 留痕。
//
// TaskIDs 非 nil 时同步任务归属（run_id），成员只在这张任务表上表达；gak_run
// 不存任务清单。nil = 保持原集合不变。
func (c *ControlStore) CommitRun(ctx context.Context, expect controlplane.Version,
	id controlplane.RunID, next controlplane.RunState, entry controlplane.AuditEntry) error {
	return c.commit(ctx, expect, entry, func(tx *sql.Tx) error {
		var one int
		if err := tx.QueryRowContext(ctx,
			`SELECT 1 FROM gak_run WHERE id = ?`, string(id)).Scan(&one); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return errors.Wrap(controlplane.ErrNotFound, "运行不存在: "+string(id))
			}
			return errors.Wrap(err, "sqliteadapter: 锁运行行失败", "run", string(id))
		}
		if next.TaskIDs != nil {
			if err := syncRunMembers(ctx, tx, id, next.TaskIDs); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE gak_run SET status = ?, gen = ? WHERE id = ?`,
			next.Status, int64(next.Gen), string(id)); err != nil {
			return errors.Wrap(err, "sqliteadapter: 写运行记录失败", "run", string(id))
		}
		return nil
	})
}

// CommitEffect 实现 controlplane.Store：原子提交裁决账 + 留痕。
func (c *ControlStore) CommitEffect(ctx context.Context, expect controlplane.Version,
	key string, next controlplane.EffectState, entry controlplane.AuditEntry) error {
	return c.commit(ctx, expect, entry, func(tx *sql.Tx) error {
		var one int
		if err := tx.QueryRowContext(ctx,
			`SELECT 1 FROM gak_control_effect WHERE key = ?`, key).Scan(&one); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return errors.Wrap(controlplane.ErrNotFound, "效果键不存在: "+key)
			}
			return errors.Wrap(err, "sqliteadapter: 锁效果账失败", "key", key)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE gak_control_effect
			    SET uncertain = ?, unsafe = ?, resolved = ?
			  WHERE key = ?`,
			next.Uncertain, next.Unsafe, string(next.Resolved), key); err != nil {
			return errors.Wrap(err, "sqliteadapter: 写效果账失败", "key", key)
		}
		return nil
	})
}

// syncRunMembers 同步运行的任务归属（只改任务表的 run_id，不另存清单）。
//
// 两步在同一事务里：先移出不在新集合里的，再移入新集合 ⇒ 中间态对外不可见。
func syncRunMembers(ctx context.Context, tx *sql.Tx, id controlplane.RunID, ids []taskboard.TaskID) error {
	if len(ids) == 0 {
		// 空集合：直接清空（PG 侧靠 ANY('{}') 的语义，SQLite 没有空 IN 列表）。
		if _, err := tx.ExecContext(ctx,
			`UPDATE gak_board_task SET run_id = '' WHERE run_id = ?`, string(id)); err != nil {
			return errors.Wrap(err, "sqliteadapter: 清空运行任务归属失败", "run", string(id))
		}
		return nil
	}
	placeholders := make([]string, 0, len(ids))
	args := make([]any, 0, len(ids))
	for _, tid := range ids {
		placeholders = append(placeholders, "?")
		args = append(args, string(tid))
	}
	in := strings.Join(placeholders, ",")
	if _, err := tx.ExecContext(ctx,
		`UPDATE gak_board_task SET run_id = ''
		  WHERE run_id = ? AND id NOT IN (`+in+`)`,
		append([]any{string(id)}, args...)...); err != nil {
		return errors.Wrap(err, "sqliteadapter: 移出运行任务失败", "run", string(id))
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE gak_board_task SET run_id = ? WHERE id IN (`+in+`)`,
		append([]any{string(id)}, args...)...); err != nil {
		return errors.Wrap(err, "sqliteadapter: 移入运行任务失败", "run", string(id))
	}
	return nil
}

// nullableString 空串 → NULL（owner / token 在非认领态是 NULL，统一在这里归一，
// 避免「空串」与「NULL」两种无值形态）。
func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// nullableMillis 零值时刻 → NULL（until 零值 = 不限期）。
func nullableMillis(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UnixMilli()
}
