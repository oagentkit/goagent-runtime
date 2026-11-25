// control_seed.go —— 控制面的**装载入口**（对位 pgstore（v0.2.0 自 goagent-kernel 迁入）/control_seed.go）。
//
// 契约里没有「创建运行」——拓扑是宿主资产 ⇒ 装载入口是**参考实现的补充**，
// 不是 controlplane.Store 的一部分（对位 PG 侧：同样放在 control_seed.go，
// 不混进契约实现）。
//
// # 一批一个事务
//
// PG 侧 SeedRun 是整批一个事务；本实现同款 ⇒ 装载中途失败不留半截（任务
// 行挂到了不存在的运行上，比「整批没装」更难排查）。
package sqliteadapter

import (
	"context"

	"github.com/oekit/pkg/errors"

	"github.com/oagentkit/goagent-kernel/controlplane"
	"github.com/oagentkit/goagent-kernel/taskboard"
)

// SeedRun 摆一条运行记录与它的任务（装载入口）。
//
// 任务行落在 gak_board_task（**与 BoardStore 同一张表**），run_id 建立归属。
// 同 id 重摆覆盖内容；⚠ **seq 不覆盖**（同 id 重摆时 seq = 既有 + 1）——
// seq 是「任务变更的序号」，单调递增是它的契约（与本包 Seed 同口径）。
//
// ⚠ **装载入口丢弃租约字段**：owner / token / fence / until 一律置 NULL / 0
// （PG 侧 control_seed.go 保留调用方给的租约字段）⇒ 与本包 BoardStore.Seed
// 同口径：**租约由存储在 Claim 时铸造**，装载入口不造租约（免得把一条过期
// 或伪造的租约带进库里）。装载入口是参考实现的补充，不是契约的一部分。
func (c *ControlStore) SeedRun(ctx context.Context, id controlplane.RunID, status string,
	gen uint64, tasks ...taskboard.Task) error {
	tx, err := c.exec.db.BeginTx(ctx, nil)
	if err != nil {
		return errors.Wrap(err, "sqliteadapter: 开控制面装载事务失败")
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO gak_run (id, status, gen) VALUES (?, ?, ?)
		 ON CONFLICT (id) DO UPDATE SET status = excluded.status, gen = excluded.gen`,
		string(id), status, int64(gen)); err != nil {
		return errors.Wrap(err, "sqliteadapter: 摆运行记录失败", "run", string(id))
	}
	for _, t := range tasks {
		if t.ID == "" {
			continue
		}
		deps, err := encodeDeps(t.Deps)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO gak_board_task
			     (id, title, deps, state, owner, token, fence, until, seq, run_id)
			 VALUES (?, ?, ?, ?, NULL, NULL, 0, NULL, 1, ?)
			 ON CONFLICT (id) DO UPDATE SET
			     title = excluded.title, deps = excluded.deps, state = excluded.state,
			     owner = NULL, token = NULL, until = NULL,
			     seq = gak_board_task.seq + 1, run_id = excluded.run_id`,
			string(t.ID), t.Title, deps, string(t.State), string(id)); err != nil {
			return errors.Wrap(err, "sqliteadapter: 摆任务失败",
				"task", string(t.ID), "run", string(id))
		}
	}
	if err := tx.Commit(); err != nil {
		return errors.Wrap(err, "sqliteadapter: 提交控制面装载事务失败")
	}
	return nil
}

// SeedEffect 摆一条效果裁决账（SnapshotEffect / CommitEffect 的装载入口）。
func (c *ControlStore) SeedEffect(ctx context.Context, key string, st controlplane.EffectState) error {
	if _, err := c.exec.db.ExecContext(ctx,
		`INSERT INTO gak_control_effect (key, uncertain, unsafe, resolved)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT (key) DO UPDATE SET
		     uncertain = excluded.uncertain, unsafe = excluded.unsafe,
		     resolved = excluded.resolved`,
		key, st.Uncertain, st.Unsafe, string(st.Resolved)); err != nil {
		return errors.Wrap(err, "sqliteadapter: 摆效果账失败", "key", key)
	}
	return nil
}
