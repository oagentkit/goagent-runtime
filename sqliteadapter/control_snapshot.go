// control_snapshot.go —— 控制面的**读面**：一致快照（三个 Snapshot*）。
//
// 为什么独立成文件：控制面的读面与写面各有硬契约（读=同一瞬间，写=同一次
// 事务），分开写才不会互相掩盖；也与本包其它面的分文件形态一致
// （board.go / board_lease.go、effect.go / effect_rows.go）。
//
// # 一致快照怎么来的
//
// SQLite 没有 REPEATABLE READ 的等价物，但**延迟（deferred）读事务**在首次
// 读时取快照，之后所有读都落在同一快照上 ⇒ 版本锚与任务集合来自同一瞬间。
//
// ⚠ 快照读走**只读句柄**（独立连接池 + _txlock=deferred）：写句柄只有一条
// 连接且 BEGIN IMMEDIATE 会拿写锁 ⇒ 在它上面开事务会把读变成写锁排队。
package sqliteadapter

import (
	"context"
	"database/sql"

	"github.com/oekit/pkg/errors"

	"github.com/yi-nology/goagent-kernel/controlplane"
	"github.com/yi-nology/goagent-kernel/taskboard"
)

// readSnapshotTx 开一个**一致快照**的只读事务（延迟读事务）。
func (c *ControlStore) readSnapshotTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := c.exec.read.BeginTx(ctx, nil)
	if err != nil {
		return errors.Wrap(err, "sqliteadapter: 开控制面只读快照事务失败")
	}
	defer func() { _ = tx.Rollback() }()
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return errors.Wrap(err, "sqliteadapter: 提交控制面只读快照事务失败")
	}
	return nil
}

// taskRowByID 读一行任务（快照用；提交路径自己判存在性）。
func taskRowByID(ctx context.Context, q queryer, id taskboard.TaskID) (*boardRow, error) {
	r, err := scanBoardRow(q.QueryRowContext(ctx,
		`SELECT `+boardCols+` FROM gak_board_task WHERE id = ?`, string(id)))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errors.Wrap(controlplane.ErrNotFound, "任务不存在: "+string(id))
		}
		return nil, errors.Wrap(err, "sqliteadapter: 读任务行失败", "task", string(id))
	}
	return r, nil
}

// runTasks 一个运行的全部任务（按 id 排序，确定性输出）。
func runTasks(ctx context.Context, q queryer, runID string) ([]taskboard.Task, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT `+boardCols+` FROM gak_board_task WHERE run_id = ? ORDER BY id`, runID)
	if err != nil {
		return nil, errors.Wrap(err, "sqliteadapter: 读运行任务失败", "run", runID)
	}
	defer func() { _ = rows.Close() }()
	var out []taskboard.Task
	for rows.Next() {
		r, err := scanBoardRow(rows)
		if err != nil {
			return nil, errors.Wrap(err, "sqliteadapter: 解析任务行失败", "run", runID)
		}
		out = append(out, r.task())
	}
	if err := rows.Err(); err != nil {
		return nil, errors.Wrap(err, "sqliteadapter: 读运行任务失败", "run", runID)
	}
	return out, nil
}

// SnapshotTask 实现 controlplane.Store：任务 + 所属 run 的全部任务
// （同一只读快照事务内取 ⇒ 快照一致）。
func (c *ControlStore) SnapshotTask(ctx context.Context, id taskboard.TaskID) (controlplane.TaskSnapshot, error) {
	var snap controlplane.TaskSnapshot
	err := c.readSnapshotTx(ctx, func(tx *sql.Tx) error {
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

// SnapshotRun 实现 controlplane.Store：运行 + 全部任务（同一只读快照事务）。
func (c *ControlStore) SnapshotRun(ctx context.Context, id controlplane.RunID) (controlplane.RunSnapshot, error) {
	var snap controlplane.RunSnapshot
	err := c.readSnapshotTx(ctx, func(tx *sql.Tx) error {
		v, err := readControlVersion(ctx, tx)
		if err != nil {
			return err
		}
		var gen int64
		if err := tx.QueryRowContext(ctx,
			`SELECT status, gen FROM gak_run WHERE id = ?`, string(id)).Scan(&snap.Status, &gen); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return errors.Wrap(controlplane.ErrNotFound, "运行不存在: "+string(id))
			}
			return errors.Wrap(err, "sqliteadapter: 读运行记录失败", "run", string(id))
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

// SnapshotEffect 实现 controlplane.Store：效果键裁决账（同一只读快照事务）。
func (c *ControlStore) SnapshotEffect(ctx context.Context, key string) (controlplane.EffectSnapshot, error) {
	var snap controlplane.EffectSnapshot
	err := c.readSnapshotTx(ctx, func(tx *sql.Tx) error {
		v, err := readControlVersion(ctx, tx)
		if err != nil {
			return err
		}
		var resolved string
		if err := tx.QueryRowContext(ctx,
			`SELECT uncertain, unsafe, resolved FROM gak_control_effect WHERE key = ?`,
			key).Scan(&snap.Effect.Uncertain, &snap.Effect.Unsafe, &resolved); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return errors.Wrap(controlplane.ErrNotFound, "效果键不存在: "+key)
			}
			return errors.Wrap(err, "sqliteadapter: 读效果账失败", "key", key)
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
