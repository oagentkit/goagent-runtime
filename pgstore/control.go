// control.go —— controlplane.Store 的 PostgreSQL 参考实现（issue #278）。
//
// # 它补的是哪一段
//
// 控制面此前只有进程内 MemStore：两个副本各持一份状态，A 的强制释放 /
// 裁决 / 终止在 B 的视图里不可见（#189）。更危险的是宿主被迫自己写一份
// 可写的任务表——控制面与 taskboard 各改各的，人工强释只改了一个投影
// （#268）。本实现把 Store 落到 PostgreSQL，并**复用 pgref_board_task 作为
// 唯一的可写任务状态**：
//
//   - 任务快照 / 提交只读写 pgref_board_task（与 taskboard.Board 同一张表）；
//   - 运行记录 pgref_run 只存 status / gen，任务成员由 task_ id 的 run_id
//     列单源表达——不复制一份任务清单；
//   - 强制释放的权威接线仍是 NewAuthoritativeStorePlane + BoardStore
//     （#268）：控制面不绕过任务的令牌 / 代次判据。ControlStore 的
//     CommitTask 只是 **Store 契约**的后端落点（供非权威装配或测试使用）。
//
// # 三条硬契约的落库形态
//
//   - **快照一致**：三个 Snapshot* 都在一个 REPEATABLE READ 只读事务里读
//     版本锚与全部相关行（任务集合不是边改边读的流）；
//   - **版本锚**：pgref_control_version 单行全局修订号；Commit* 先
//     SELECT … FOR UPDATE 校验 expect，不符即 ErrStoreConflict 且什么都不写；
//     提交成功推进修订号；
//   - **留痕与状态同事务方向**：Auditor.Append 在版本行锁下、状态写入之前
//     调用；留痕失败 ⇒ 事务回滚，状态一字不动。宿主 Auditor 若落在另一个
//     数据库，跨库原子性不成立——这是宿主账本的边界，与 MemStore 同口径
//     （内核要求的是「留痕失败不得改状态」，不是替宿主实现两阶段提交）。
//
// # 同源声明（issue #268）
//
// ControlStore 与 BoardStore 都实现 controlplane.TaskSource，来源标识由
// pool 的 host/port/database/search_path 派生：同一 schema 的两者返回相同
// 值，NewAuthoritativeStorePlane 据此在装配期判定「同一张任务表」；不同
// schema / 不同库的来源不同 ⇒ ErrAuthorityMismatch。这是不靠类型猜同源的
// 可机判落点。
package pgstore

import (
	"context"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/oekit/pkg/errors"

	"github.com/oagentkit/goagent-kernel/controlplane"
	"github.com/oagentkit/goagent-kernel/taskboard"
)

// poolSourceID 存储来源标识：同一连接池配置（host/port/database/schema）
// 的 BoardStore 与 ControlStore 报同一值。schema 取 search_path——同一库的
// 不同 schema 是两套互不可见的表，不能算同源。
func poolSourceID(pool *pgxpool.Pool) string {
	if pool == nil {
		return ""
	}
	cfg := pool.Config().ConnConfig
	return "pgstore:" + cfg.Host + ":" + strconv.Itoa(int(cfg.Port)) + "/" +
		cfg.Database + "/" + cfg.RuntimeParams["search_path"]
}

// ControlStore controlplane.Store 的 PostgreSQL 参考实现。
//
// 一个实例绑定一个 schema；并发安全（提交在 pgref_control_version 行锁上
// 串行化）。aud 为 nil 时所有 Commit* 返回 controlplane.ErrNoAuditor
// （留痕面缺位 ⇒ 介入一律拒绝，与 MemStore 同口径）。
type ControlStore struct {
	pool *pgxpool.Pool
	aud  controlplane.Auditor
	src  string
}

// NewControlStore 构造控制面持久化存储。aud 由宿主注入（可为 nil，但那样
// 只能读不能提交）；pool 归调用方管理。
func NewControlStore(pool *pgxpool.Pool, aud controlplane.Auditor) *ControlStore {
	return &ControlStore{pool: pool, aud: aud, src: poolSourceID(pool)}
}

// 编译期契约断言：Store 契约 + 同源声明 + BoardStore 的任务权威面。
var (
	_ controlplane.Store         = (*ControlStore)(nil)
	_ controlplane.TaskSource    = (*ControlStore)(nil)
	_ controlplane.TaskSource    = (*BoardStore)(nil)
	_ controlplane.TaskAuthority = (*BoardStore)(nil)
)

// TaskSourceID 实现 controlplane.TaskSource（见 poolSourceID）。
func (c *ControlStore) TaskSourceID() string { return c.src }

// readTx 一致快照的只读事务（REPEATABLE READ）：版本锚与任务集合必须在
// 同一瞬间读到，否则「快照之后目标被改过 ⇒ 提交必失败」的判据会漏。
func (c *ControlStore) readTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	tx, err := c.pool.BeginTx(ctx, pgx.TxOptions{
		IsoLevel:   pgx.RepeatableRead,
		AccessMode: pgx.ReadOnly,
	})
	if err != nil {
		return errors.Wrap(err, "pgstore: 开只读快照事务失败")
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return errors.Wrap(err, "pgstore: 提交只读快照事务失败")
	}
	return nil
}

func readControlVersion(ctx context.Context, q queryRower) (controlplane.Version, error) {
	var v int64
	if err := q.QueryRow(ctx,
		`SELECT ver FROM pgref_control_version WHERE id = 1`).Scan(&v); err != nil {
		return 0, errors.Wrap(err, "pgstore: 读控制面版本锚失败（EnsureSchema 建表了吗？）")
	}
	return controlplane.Version(v), nil
}

func lockControlVersion(ctx context.Context, tx pgx.Tx) (controlplane.Version, error) {
	var v int64
	if err := tx.QueryRow(ctx,
		`SELECT ver FROM pgref_control_version WHERE id = 1 FOR UPDATE`).Scan(&v); err != nil {
		return 0, errors.Wrap(err, "pgstore: 锁控制面版本锚失败")
	}
	return controlplane.Version(v), nil
}

// taskRowByID 读一行任务（快照用；提交路径自己带 FOR UPDATE）。
func taskRowByID(ctx context.Context, q queryRower, id taskboard.TaskID) (*boardRow, error) {
	r, err := scanBoardRow(q.QueryRow(ctx,
		`SELECT `+boardCols+` FROM pgref_board_task WHERE id = $1`, string(id)))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, errors.Wrap(controlplane.ErrNotFound, "任务不存在: "+string(id))
		}
		return nil, errors.Wrap(err, "pgstore: 读任务行失败", "task", string(id))
	}
	return r, nil
}

// runTasks 一个运行的全部任务（按 id 排序，确定性输出）。
func runTasks(ctx context.Context, tx pgx.Tx, runID string) ([]taskboard.Task, error) {
	rows, err := tx.Query(ctx,
		`SELECT `+boardCols+` FROM pgref_board_task WHERE run_id = $1 ORDER BY id`, runID)
	if err != nil {
		return nil, errors.Wrap(err, "pgstore: 读运行任务失败", "run", runID)
	}
	defer rows.Close()
	var out []taskboard.Task
	for rows.Next() {
		r, err := scanBoardRow(rows)
		if err != nil {
			return nil, errors.Wrap(err, "pgstore: 解析任务行失败", "run", runID)
		}
		out = append(out, r.task())
	}
	if err := rows.Err(); err != nil {
		return nil, errors.Wrap(err, "pgstore: 读运行任务失败", "run", runID)
	}
	return out, nil
}

// SnapshotTask 实现 controlplane.Store：任务 + 所属 run 的全部任务（同一
// 只读事务内取，快照一致）。
func (c *ControlStore) SnapshotTask(ctx context.Context, id taskboard.TaskID) (controlplane.TaskSnapshot, error) {
	var snap controlplane.TaskSnapshot
	err := c.readTx(ctx, func(tx pgx.Tx) error {
		v, err := readControlVersion(ctx, tx)
		if err != nil {
			return err
		}
		r, err := taskRowByID(ctx, tx, id)
		if err != nil {
			return err
		}
		snap.Version = v
		snap.RunID = controlplane.RunID(r.runID)
		snap.Task = r.task()
		if r.runID == "" {
			// 未归属运行的任务仍可被控制面看到（单任务视图），不伪装成不存在。
			snap.RunTasks = []taskboard.Task{snap.Task}
			return nil
		}
		tasks, err := runTasks(ctx, tx, r.runID)
		if err != nil {
			return err
		}
		snap.RunTasks = tasks
		return nil
	})
	if err != nil {
		return controlplane.TaskSnapshot{}, err
	}
	return snap, nil
}

// SnapshotRun 实现 controlplane.Store：运行 + 全部任务（同一只读事务）。
func (c *ControlStore) SnapshotRun(ctx context.Context, id controlplane.RunID) (controlplane.RunSnapshot, error) {
	var snap controlplane.RunSnapshot
	err := c.readTx(ctx, func(tx pgx.Tx) error {
		v, err := readControlVersion(ctx, tx)
		if err != nil {
			return err
		}
		var gen int64
		if err := tx.QueryRow(ctx,
			`SELECT status, gen FROM pgref_run WHERE id = $1`, string(id)).Scan(&snap.Status, &gen); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return errors.Wrap(controlplane.ErrNotFound, "运行不存在: "+string(id))
			}
			return errors.Wrap(err, "pgstore: 读运行记录失败", "run", string(id))
		}
		tasks, err := runTasks(ctx, tx, string(id))
		if err != nil {
			return err
		}
		snap.Version = v
		snap.RunID = id
		snap.Gen = uint64(gen)
		snap.Tasks = tasks
		return nil
	})
	if err != nil {
		return controlplane.RunSnapshot{}, err
	}
	return snap, nil
}

// SnapshotEffect 实现 controlplane.Store：效果键裁决账（同一只读事务）。
func (c *ControlStore) SnapshotEffect(ctx context.Context, key string) (controlplane.EffectSnapshot, error) {
	var snap controlplane.EffectSnapshot
	err := c.readTx(ctx, func(tx pgx.Tx) error {
		v, err := readControlVersion(ctx, tx)
		if err != nil {
			return err
		}
		var resolved string
		if err := tx.QueryRow(ctx,
			`SELECT uncertain, unsafe, resolved FROM pgref_control_effect WHERE key = $1`,
			key).Scan(&snap.Effect.Uncertain, &snap.Effect.Unsafe, &resolved); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return errors.Wrap(controlplane.ErrNotFound, "效果键不存在: "+key)
			}
			return errors.Wrap(err, "pgstore: 读效果账失败", "key", key)
		}
		snap.Version = v
		snap.Key = key
		snap.Effect.Resolved = controlplane.Verdict(resolved)
		return nil
	})
	if err != nil {
		return controlplane.EffectSnapshot{}, err
	}
	return snap, nil
}

// appendControlAudit 调宿主留痕面并隔离 panic（与内核 controlplane 的同名
// 接缝同口径：panic 转 ErrAuditorPanic，状态一字不动）。
func appendControlAudit(ctx context.Context, aud controlplane.Auditor, e controlplane.AuditEntry) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = errors.Wrap(controlplane.ErrAuditorPanic, "留痕实现 panic（pgstore 已隔离，状态未变）")
		}
	}()
	return aud.Append(ctx, e)
}

// commit 三个 Commit* 的共用骨架：版本锚 → 留痕 → 状态写入 → 推进版本，
// 全部在一个事务里。留痕失败 ⇒ 整体回滚（状态一字不动）。
func (c *ControlStore) commit(ctx context.Context, expect controlplane.Version, entry controlplane.AuditEntry, fn func(tx pgx.Tx) error) error {
	if c.aud == nil {
		return controlplane.ErrNoAuditor
	}
	return runTx(ctx, c.pool, func(tx pgx.Tx) error {
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
		if _, err := tx.Exec(ctx,
			`UPDATE pgref_control_version SET ver = ver + 1 WHERE id = 1`); err != nil {
			return errors.Wrap(err, "pgstore: 推进控制面版本锚失败")
		}
		return nil
	})
}

// CommitTask 实现 controlplane.Store：原子提交任务状态 + 留痕。
//
// 状态写在 pgref_board_task（与 taskboard.Board **同一张可写表**）；
// 不存在的任务 ⇒ ErrNotFound 且不推进版本。
func (c *ControlStore) CommitTask(ctx context.Context, expect controlplane.Version, next taskboard.Task, entry controlplane.AuditEntry) error {
	return c.commit(ctx, expect, entry, func(tx pgx.Tx) error {
		var one int
		if err := tx.QueryRow(ctx,
			`SELECT 1 FROM pgref_board_task WHERE id = $1 FOR UPDATE`, string(next.ID)).Scan(&one); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return errors.Wrap(controlplane.ErrNotFound, "任务不存在: "+string(next.ID))
			}
			return errors.Wrap(err, "pgstore: 锁任务行失败", "task", string(next.ID))
		}
		if _, err := tx.Exec(ctx,
			`UPDATE pgref_board_task
			    SET title = $2, deps = $3, state = $4, owner = $5, token = $6,
			        fence = $7, until = $8, seq = $9
			  WHERE id = $1`,
			string(next.ID), next.Title, depIDs(next.Deps), string(next.State),
			nullableString(string(next.Owner)), nullableString(next.Lease.Token),
			int64(next.Lease.Fence), nullableTime(next.Lease.Until), int64(next.Seq)); err != nil {
			return errors.Wrap(err, "pgstore: 写任务行失败", "task", string(next.ID))
		}
		return nil
	})
}

// CommitRun 实现 controlplane.Store：原子提交运行状态/代次 + 留痕。
//
// TaskIDs 非 nil 时同步任务归属（run_id），成员只在这张任务表上表达；
// pgref_run 不存任务清单。nil = 保持原集合不变。
func (c *ControlStore) CommitRun(ctx context.Context, expect controlplane.Version, id controlplane.RunID, next controlplane.RunState, entry controlplane.AuditEntry) error {
	return c.commit(ctx, expect, entry, func(tx pgx.Tx) error {
		var one int
		if err := tx.QueryRow(ctx,
			`SELECT 1 FROM pgref_run WHERE id = $1 FOR UPDATE`, string(id)).Scan(&one); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return errors.Wrap(controlplane.ErrNotFound, "运行不存在: "+string(id))
			}
			return errors.Wrap(err, "pgstore: 锁运行行失败", "run", string(id))
		}
		if next.TaskIDs != nil {
			ids := make([]string, 0, len(next.TaskIDs))
			for _, tid := range next.TaskIDs {
				ids = append(ids, string(tid))
			}
			if len(ids) == 0 {
				if _, err := tx.Exec(ctx,
					`UPDATE pgref_board_task SET run_id = '' WHERE run_id = $1`, string(id)); err != nil {
					return errors.Wrap(err, "pgstore: 清空运行任务归属失败", "run", string(id))
				}
			} else {
				// 先移出不在新集合里的任务，再移入新集合：两步在同一事务里，
				// 中间态对外不可见。
				if _, err := tx.Exec(ctx,
					`UPDATE pgref_board_task SET run_id = ''
					  WHERE run_id = $1 AND NOT (id = ANY($2))`, string(id), ids); err != nil {
					return errors.Wrap(err, "pgstore: 移出运行任务失败", "run", string(id))
				}
				if _, err := tx.Exec(ctx,
					`UPDATE pgref_board_task SET run_id = $1 WHERE id = ANY($2)`, string(id), ids); err != nil {
					return errors.Wrap(err, "pgstore: 移入运行任务失败", "run", string(id))
				}
			}
		}
		if _, err := tx.Exec(ctx,
			`UPDATE pgref_run SET status = $2, gen = $3 WHERE id = $1`,
			string(id), next.Status, int64(next.Gen)); err != nil {
			return errors.Wrap(err, "pgstore: 写运行记录失败", "run", string(id))
		}
		return nil
	})
}

// CommitEffect 实现 controlplane.Store：原子提交裁决账 + 留痕。
func (c *ControlStore) CommitEffect(ctx context.Context, expect controlplane.Version, key string, next controlplane.EffectState, entry controlplane.AuditEntry) error {
	return c.commit(ctx, expect, entry, func(tx pgx.Tx) error {
		var one int
		if err := tx.QueryRow(ctx,
			`SELECT 1 FROM pgref_control_effect WHERE key = $1 FOR UPDATE`, key).Scan(&one); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return errors.Wrap(controlplane.ErrNotFound, "效果键不存在: "+key)
			}
			return errors.Wrap(err, "pgstore: 锁效果账失败", "key", key)
		}
		if _, err := tx.Exec(ctx,
			`UPDATE pgref_control_effect
			    SET uncertain = $2, unsafe = $3, resolved = $4
			  WHERE key = $1`,
			key, next.Uncertain, next.Unsafe, string(next.Resolved)); err != nil {
			return errors.Wrap(err, "pgstore: 写效果账失败", "key", key)
		}
		return nil
	})
}
