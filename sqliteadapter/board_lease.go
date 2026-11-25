// board_lease.go —— 租约的写路径：认领（Claim）与租约收场
// （Release / Complete / Renew）。
//
// # 认领的原子性靠「写锁先行」
//
// 写句柄的 DSN 带 _txlock=immediate ⇒ 事务一开就拿写锁 ⇒ 读-判-写三段在
// 同一把锁下 ⇒ 两个执行者同时认领同一任务时，输家在写锁上排队，赢家提交后
// 输家读到 claimed 状态 ⇒ ErrTaken（契约要求恰好一个赢家）。
//
// # 校验与写入必须同处一个事务
//
// 续期（Renew）的「资格校验」与「到期时刻推后」在同一个事务里完成——分开做
// 会留下「校验通过」与「续期生效」之间的窗口，接管者与续期者可同时得手。
//
// 判定口径逐项对位 pgstore（v0.2.0 自 goagent-kernel 迁入）/board.go（同一套契约、同一套判据）。
package sqliteadapter

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"

	"github.com/oekit/pkg/errors"

	"github.com/oagentkit/goagent-kernel/taskboard"
)

// Claim 认领一个任务（读-判-写不可分割：一个事务完成）。
//
// 流程：BEGIN IMMEDIATE（先拿写锁）→ 读任务行 → 判（词表外 / 终态 / 依赖 /
// 被活持有）→ 铸造令牌与接管代次 → 条件更新 → 提交。
func (b *BoardStore) Claim(ctx context.Context, id taskboard.TaskID, agent taskboard.AgentID, req taskboard.ClaimRequest) (taskboard.Task, error) {
	var out taskboard.Task
	err := b.exec.run(ctx, func(tx *sql.Tx) error {
		in := b.execIn(tx)
		// 判定时刻：写锁拿到之后取（等锁时长不得计入租约寿命）。
		now, err := in.now(ctx, tx)
		if err != nil {
			return err
		}
		r, err := scanBoardRow(tx.QueryRowContext(ctx,
			`SELECT `+boardCols+` FROM gak_board_task WHERE id = ?`, string(id)))
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return errors.Wrap(taskboard.ErrNotFound, "sqliteadapter: 任务不存在", "task", string(id))
			}
			return errors.Wrap(err, "sqliteadapter: 读任务行失败", "task", string(id))
		}
		// 状态可认领判定统一走 taskboard.State.Claimable（issue #360）：
		// 词表外 → ErrUnknownState，终态 → ErrTerminal；不得只判 Terminal()
		// （未知状态不是终态，只看终态会让脏状态行照常领到租约）。
		if cerr := r.state.Claimable(); cerr != nil {
			return errors.Wrap(cerr, "sqliteadapter: 任务状态不可认领",
				"task", string(id), "state", string(r.state))
		}
		if err := depsDone(ctx, tx, r.deps); err != nil {
			return err
		}
		if r.held(now) {
			return errors.Wrap(taskboard.ErrTaken, "sqliteadapter: 任务已被他人认领", "task", string(id))
		}
		// 铸一枚新令牌 + 接管代次，与状态写**同一次落库**（同源）：
		// 令牌跨进程不复用（随机），代次在同一任务上严格递增。
		token, err := mintToken()
		if err != nil {
			return err
		}
		fence := r.fence + 1
		var until sql.NullInt64
		if req.TTL > 0 {
			until = sql.NullInt64{Int64: now.Add(req.TTL).UnixMilli(), Valid: true}
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE gak_board_task SET state = ?, owner = ?, token = ?, fence = ?, until = ?, seq = seq + 1
			 WHERE id = ?`,
			string(taskboard.StateClaimed), string(agent), token, fence, until, string(id)); err != nil {
			return errors.Wrap(err, "sqliteadapter: 认领写回失败", "task", string(id))
		}
		r.state = taskboard.StateClaimed
		r.owner = string(agent)
		r.token = token
		r.fence = fence
		r.until = until
		r.seq++
		out = r.task()
		return nil
	})
	if err != nil {
		return taskboard.Task{}, err
	}
	return out, nil
}

// depsDone 判依赖是否全部完成（不存在的依赖视为未完成）。
//
// 依赖「完成」的判据 = 状态是 done。⚠ **failed 不算完成**（issue #375：
// failed 是非终态、可重做，但它也**不是**「已完成」）。
func depsDone(ctx context.Context, tx *sql.Tx, deps []taskboard.TaskID) error {
	for _, d := range deps {
		var state string
		err := tx.QueryRowContext(ctx,
			`SELECT state FROM gak_board_task WHERE id = ?`, string(d)).Scan(&state)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return errors.Wrap(taskboard.ErrBlocked, "sqliteadapter: 依赖不存在", "dep", string(d))
			}
			return errors.Wrap(err, "sqliteadapter: 读依赖失败", "dep", string(d))
		}
		if taskboard.State(state) != taskboard.StateDone {
			return errors.Wrap(taskboard.ErrBlocked, "sqliteadapter: 依赖未完成", "dep", string(d))
		}
	}
	return nil
}

// mintToken 铸一枚随机令牌（跨进程不复用不重合 ⇒ ABA 封死）。
func mintToken() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", errors.Wrap(err, "sqliteadapter: 铸造令牌失败")
	}
	return "tok-" + hex.EncodeToString(buf[:]), nil
}

// releaseOrComplete 释放与完成共用的条件更新：只认令牌，不认内容。
func releaseOrComplete(ctx context.Context, tx *sql.Tx, id taskboard.TaskID, token, state string) error {
	res, err := tx.ExecContext(ctx,
		`UPDATE gak_board_task
		    SET state = ?, owner = NULL, token = NULL, until = NULL, seq = seq + 1
		  WHERE id = ? AND token = ? AND state = ?`,
		state, string(id), token, string(taskboard.StateClaimed))
	if err != nil {
		return errors.Wrap(err, "sqliteadapter: 归还/完成写回失败", "task", string(id))
	}
	if n, _ := res.RowsAffected(); n == 1 {
		return nil
	}
	// 没命中：区分「任务不存在」与「令牌不匹配」——两者的处置不同。
	var got string
	err = tx.QueryRowContext(ctx,
		`SELECT state FROM gak_board_task WHERE id = ?`, string(id)).Scan(&got)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errors.Wrap(taskboard.ErrNotFound, "sqliteadapter: 任务不存在", "task", string(id))
		}
		return errors.Wrap(err, "sqliteadapter: 读任务行失败", "task", string(id))
	}
	return errors.Wrap(taskboard.ErrLeaseLost, "sqliteadapter: 租约令牌不匹配",
		"task", string(id), "state", got)
}

// Release 归还认领（凭令牌）：非持有者释放一律被拒，不许静默成功。
func (b *BoardStore) Release(ctx context.Context, id taskboard.TaskID, token string) error {
	return b.exec.run(ctx, func(tx *sql.Tx) error {
		return releaseOrComplete(ctx, tx, id, token, string(taskboard.StateOpen))
	})
}

// Complete 标记完成（凭令牌）：终态不带 owner，避免「谁做完的」与「谁还占着」混在一起。
func (b *BoardStore) Complete(ctx context.Context, id taskboard.TaskID, token string) error {
	return b.exec.run(ctx, func(tx *sql.Tx) error {
		return releaseOrComplete(ctx, tx, id, token, string(taskboard.StateDone))
	})
}

// Renew 续期并确认持有资格（taskboard.RenewingBoard）。
//
// 校验（令牌是当前租约的 + 租约未过期）与延长在**同一个事务**里——原子；
// 校验通过与续期生效之间不能有窗口（否则接管者与续期者可同时得手）。
// 续期不换令牌、不递增代次（续期不是接管）；seq 前进（Until 变了，这是一次
// 任务变更）。
func (b *BoardStore) Renew(ctx context.Context, id taskboard.TaskID, token string, req taskboard.ClaimRequest) (taskboard.Task, error) {
	var out taskboard.Task
	err := b.exec.run(ctx, func(tx *sql.Tx) error {
		in := b.execIn(tx)
		now, err := in.now(ctx, tx)
		if err != nil {
			return err
		}
		r, err := scanBoardRow(tx.QueryRowContext(ctx,
			`SELECT `+boardCols+` FROM gak_board_task WHERE id = ?`, string(id)))
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return errors.Wrap(taskboard.ErrNotFound, "sqliteadapter: 任务不存在", "task", string(id))
			}
			return errors.Wrap(err, "sqliteadapter: 读任务行失败", "task", string(id))
		}
		if r.state != taskboard.StateClaimed || r.token != token {
			return errors.Wrap(taskboard.ErrLeaseLost, "sqliteadapter: 租约令牌不匹配", "task", string(id))
		}
		// 租约已过期 ⇒ 资格在到期时刻终止，不许给死者续命。
		if !r.held(now) {
			return errors.Wrap(taskboard.ErrLeaseLost, "sqliteadapter: 租约已过期，资格终止", "task", string(id))
		}
		var until sql.NullInt64
		if req.TTL > 0 {
			until = sql.NullInt64{Int64: now.Add(req.TTL).UnixMilli(), Valid: true}
		}
		res, err := tx.ExecContext(ctx,
			`UPDATE gak_board_task SET until = ?, seq = seq + 1 WHERE id = ? AND token = ?`,
			until, string(id), token)
		if err != nil {
			return errors.Wrap(err, "sqliteadapter: 续期写回失败", "task", string(id))
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return errors.Wrap(taskboard.ErrLeaseLost, "sqliteadapter: 租约令牌不匹配", "task", string(id))
		}
		r.until = until
		r.seq++
		out = r.task()
		return nil
	})
	if err != nil {
		return taskboard.Task{}, err
	}
	return out, nil
}
