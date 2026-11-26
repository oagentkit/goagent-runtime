// control_seed.go —— ControlStore 的装载入口（从 control.go 拆出，只为守住
// 「单文件 400 行」的结构阈值；判据归属不变）。
package pgstore

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/oekit/pkg/errors"

	"github.com/yi-nology/goagent-kernel/controlplane"
	"github.com/yi-nology/goagent-kernel/taskboard"
)

// SeedRun 摆一条运行记录与它的任务（参考实现的装载入口，契约里没有
// 「创建运行」——拓扑是宿主资产）。
//
// 任务行落在 pgref_board_task（**与 BoardStore 同一张表**），run_id 建立
// 归属。同 id 重摆覆盖内容（测试每个子测试从全新 schema 起步）。
func (c *ControlStore) SeedRun(ctx context.Context, id controlplane.RunID, status string, gen uint64, tasks ...taskboard.Task) error {
	return runTx(ctx, c.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`INSERT INTO pgref_run (id, status, gen) VALUES ($1, $2, $3)
			 ON CONFLICT (id) DO UPDATE SET status = EXCLUDED.status, gen = EXCLUDED.gen`,
			string(id), status, int64(gen)); err != nil {
			return errors.Wrap(err, "pgstore: 摆运行记录失败", "run", string(id))
		}
		for _, t := range tasks {
			if t.ID == "" {
				continue
			}
			if _, err := tx.Exec(ctx,
				`INSERT INTO pgref_board_task
				     (id, title, deps, state, owner, token, fence, until, seq, run_id)
				 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
				 ON CONFLICT (id) DO UPDATE SET
				     title = EXCLUDED.title, deps = EXCLUDED.deps, state = EXCLUDED.state,
				     owner = EXCLUDED.owner, token = EXCLUDED.token, fence = EXCLUDED.fence,
				     until = EXCLUDED.until, seq = EXCLUDED.seq, run_id = EXCLUDED.run_id`,
				string(t.ID), t.Title, depIDs(t.Deps), string(t.State),
				nullableString(string(t.Owner)), nullableString(t.Lease.Token),
				int64(t.Lease.Fence), nullableTime(t.Lease.Until), int64(t.Seq), string(id)); err != nil {
				return errors.Wrap(err, "pgstore: 摆任务失败", "task", string(t.ID), "run", string(id))
			}
		}
		return nil
	})
}

// SeedEffect 摆一条效果裁决账（SnapshotEffect / CommitEffect 的装载入口）。
func (c *ControlStore) SeedEffect(ctx context.Context, key string, st controlplane.EffectState) error {
	_, err := c.pool.Exec(ctx,
		`INSERT INTO pgref_control_effect (key, uncertain, unsafe, resolved)
		 VALUES ($1, $2, $3, $4)
		 ON CONFLICT (key) DO UPDATE SET
		     uncertain = EXCLUDED.uncertain, unsafe = EXCLUDED.unsafe, resolved = EXCLUDED.resolved`,
		key, st.Uncertain, st.Unsafe, string(st.Resolved))
	if err != nil {
		return errors.Wrap(err, "pgstore: 摆效果账失败", "key", key)
	}
	return nil
}

// nullableString 空串 → NULL（任务表的 owner / token 在非认领态是 NULL，
// 统一在这里归一，避免「空串」与「NULL」两种无值形态）。
func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// nullableTime 零值时刻 → NULL（Until 零值 = 不限期）。
func nullableTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}
