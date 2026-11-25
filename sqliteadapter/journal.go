// journal.go —— invocation.Store 的 SQLite 参考实现（工具调用账本）。
//
// 契约与判据逐条对位 pgstore/journal.go（同一套契约、同一套判据，只有方言
// 不同：$1 → ?、BIGINT → INTEGER、BYTEA → BLOB、IDENTITY → AUTOINCREMENT）。
//
// # 三个动作的落库形态
//
//   - Plan：一条 INSERT … ON CONFLICT DO NOTHING。冲突时回读已落的那条，
//     比对身份（tool + args_digest）：一致 ⇒ 幂等无操作；不一致 ⇒
//     ErrIdentityConflict。
//   - Advance：一条**带条件**的 UPDATE（WHERE state IN (前驱集)）——「比较
//     与写入」在同一条语句里完成。前驱集由内核的 invocation.Predecessors
//     给出（宿主自己反推转移表就是第二份真相）。影响行数为 0 ⇒ 回读一次判
//     「已是目标态（幂等）」还是「非法转移」。
//   - Get / List：普通读；不存在 ⇒ ErrNotFound（**不得**返回零值 + nil）。
//
// # 原子性来自「BEGIN IMMEDIATE 先拿写锁」
//
// 写句柄的 DSN 带 _txlock=immediate ⇒ 事务一开就拿写锁 ⇒ 校验 → 写入在
// 同一把锁下 ⇒ 两个执行者同时推进同一条账目时，输家在写锁上排队，赢家提交
// 后输家读到的是判定**后**的新状态（契约要求恰好一个赢家）。
//
// # ⚠ 账本面**刻意不参与跨面批次**
//
// 账本必须在副作用**之前**落库（这是 P0-09 闭合的前提），而跨面批次的提交
// 点在批次**之后** ⇒ 账本若参与批次，它的落库就晚于副作用 ⇒ 崩溃窗口重新
// 打开。本面一律走**自己的事务**（exec.tx == nil ⇒ exec.run 自开事务）。
package sqliteadapter

import (
	"context"
	"database/sql"
	"strings"

	"github.com/oekit/pkg/errors"

	"github.com/oagentkit/goagent-kernel/invocation"
)

// JournalStore 工具调用账本的 SQLite 实现（invocation.Store）。
//
// 一个实例绑定一个库文件（与其它面同款：写句柄 + 只读句柄）。
type JournalStore struct {
	exec exec
}

// NewJournalStore 构造账本存储。db 归调用方管理（Close 由它负责）。
func NewJournalStore(db *sql.DB) *JournalStore {
	return &JournalStore{exec: execOf(db)}
}

// Journal 工具调用账本面（invocation.Store）。
//
// ⚠ 这个面**刻意不参与 Apply 批次**：账本必须在副作用**之前**落库
// （P0-09 闭合的前提：崩溃后按账本续跑，不再问模型要那次调用），而批次
// 的提交点在批次**之后** ⇒ 参与批次就等于把「账本晚于副作用」变成常态
// ⇒ 重复施加的窗口重新打开。它走自己的事务（见本文件包头）。
//
// 两侧能力面对位：PG 侧同款入口是 pgstore.Store.Journal()。
func (s *Store) Journal() *JournalStore {
	return &JournalStore{exec: s.execBase()}
}

// journalCols 账目的列（读侧与写侧共用一份 ⇒ 两处不会漂）。
const journalCols = `session_id, call_id, tool, args_digest, args, state, result, reason, seq`

// Plan 落一条待执行调用（身份一经落账不可变）。
//
// 幂等：同一 (会话, 调用) 且身份一致 ⇒ 幂等无操作；身份不一致 ⇒
// ErrIdentityConflict。
func (s *JournalStore) Plan(ctx context.Context, e invocation.Entry) error {
	if e.SessionID == "" || e.CallID == "" {
		return errors.New("sqliteadapter: 账目身份不完整（会话与调用 ID 不得为空）")
	}
	if !invocation.Known(e.State) {
		return errors.New("sqliteadapter: 账目状态不在内核词表内（读不懂的状态不许落库）")
	}
	return s.exec.run(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`INSERT INTO gak_invocation (session_id, call_id, tool, args_digest, args, state, result, reason)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			 ON CONFLICT (session_id, call_id) DO NOTHING`,
			e.SessionID, e.CallID, e.Tool, e.ArgsDigest, e.Args, string(e.State), e.Result, e.Reason)
		if err != nil {
			return errors.Wrap(err, "sqliteadapter: 账目落库失败", "session_id", e.SessionID, "call_id", e.CallID)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return errors.Wrap(err, "sqliteadapter: 账目落库行数读取失败", "call_id", e.CallID)
		}
		if n > 0 {
			return nil
		}
		// 冲突 ⇒ 回读已落的那条，比对身份。
		var tool, digest string
		if err := tx.QueryRowContext(ctx,
			`SELECT tool, args_digest FROM gak_invocation WHERE session_id = ? AND call_id = ?`,
			e.SessionID, e.CallID).Scan(&tool, &digest); err != nil {
			return errors.Wrap(err, "sqliteadapter: 账目冲突后回读失败", "call_id", e.CallID)
		}
		if tool != e.Tool || digest != e.ArgsDigest {
			return errors.Wrap(invocation.ErrIdentityConflict,
				"同一调用的身份与首次登记不一致（那是另一个动作）",
				"session_id", e.SessionID, "call_id", e.CallID)
		}
		return nil // 幂等：同身份重放不报错
	})
}

// Get 取一条账目；不存在 ⇒ ErrNotFound。
func (s *JournalStore) Get(ctx context.Context, sessionID, callID string) (invocation.Entry, error) {
	q, err := s.exec.reader()
	if err != nil {
		return invocation.Entry{}, err
	}
	var (
		e   invocation.Entry
		st  string
		seq int64
	)
	err = q.QueryRowContext(ctx,
		`SELECT `+journalCols+` FROM gak_invocation WHERE session_id = ? AND call_id = ?`,
		sessionID, callID).Scan(&e.SessionID, &e.CallID, &e.Tool, &e.ArgsDigest, &e.Args, &st, &e.Result, &e.Reason, &seq)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return invocation.Entry{}, errors.Wrap(invocation.ErrNotFound,
				"账目不存在", "session_id", sessionID, "call_id", callID)
		}
		return invocation.Entry{}, errors.Wrap(err, "sqliteadapter: 账目读取失败", "call_id", callID)
	}
	e.State = invocation.State(st)
	e.Seq = uint64(seq)
	return e, nil
}

// List 列出会话的全部账目（按序号升序）。
func (s *JournalStore) List(ctx context.Context, sessionID string) ([]invocation.Entry, error) {
	q, err := s.exec.reader()
	if err != nil {
		return nil, err
	}
	rows, err := q.QueryContext(ctx,
		`SELECT `+journalCols+` FROM gak_invocation WHERE session_id = ? ORDER BY seq`, sessionID)
	if err != nil {
		return nil, errors.Wrap(err, "sqliteadapter: 账目列举失败", "session_id", sessionID)
	}
	defer rows.Close()
	out := make([]invocation.Entry, 0, 8)
	for rows.Next() {
		var (
			e   invocation.Entry
			st  string
			seq int64
		)
		if err := rows.Scan(&e.SessionID, &e.CallID, &e.Tool, &e.ArgsDigest, &e.Args, &st, &e.Result, &e.Reason, &seq); err != nil {
			return nil, errors.Wrap(err, "sqliteadapter: 账目行扫描失败", "session_id", sessionID)
		}
		e.State = invocation.State(st)
		e.Seq = uint64(seq)
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, errors.Wrap(err, "sqliteadapter: 账目行读取失败", "session_id", sessionID)
	}
	return out, nil
}

// Advance 把账目推进到目标状态（存储侧原子校验「当前状态允许该转移」）。
//
// 非法转移 ⇒ ErrIllegalTransition；目标状态已是当前状态 ⇒ 幂等无操作。
func (s *JournalStore) Advance(ctx context.Context, sessionID, callID string, to invocation.State, u invocation.Update) (invocation.Entry, error) {
	if !invocation.Known(to) {
		return invocation.Entry{}, errors.Wrap(invocation.ErrIllegalTransition,
			"目标状态不在内核词表内（读不懂的目标不许放行）", "call_id", callID)
	}
	preds := invocation.Predecessors(to)
	var out invocation.Entry
	err := s.exec.run(ctx, func(tx *sql.Tx) error {
		// 一条带条件的 UPDATE：比较与写入在同一条语句里完成。
		//
		// ⚠ 前驱集为空（没有任何状态能转到目标态）⇒ **不得**执行这条 UPDATE
		// （否则条件退化成「只按键匹配」，会把非法转移放行）。
		if len(preds) > 0 {
			ph := make([]string, 0, len(preds))
			args := []any{string(to), u.Result, u.Reason, sessionID, callID}
			for _, p := range preds {
				ph = append(ph, "?")
				args = append(args, string(p))
			}
			res, err := tx.ExecContext(ctx,
				`UPDATE gak_invocation SET state = ?, result = ?, reason = ?
				 WHERE session_id = ? AND call_id = ? AND state IN (`+strings.Join(ph, ",")+`)`,
				args...)
			if err != nil {
				return errors.Wrap(err, "sqliteadapter: 账目推进失败", "call_id", callID)
			}
			n, err := res.RowsAffected()
			if err != nil {
				return errors.Wrap(err, "sqliteadapter: 账目推进行数读取失败", "call_id", callID)
			}
			if n > 0 {
				cur, err := s.getInTx(ctx, tx, sessionID, callID)
				if err != nil {
					return err
				}
				out = cur
				return nil
			}
		}
		// 没有行被更新：两种可能——已是目标态（幂等）或非法转移。回读判。
		cur, err := s.getInTx(ctx, tx, sessionID, callID)
		if err != nil {
			return err
		}
		if cur.State == to {
			out = cur // 幂等：推进到当前状态
			return nil
		}
		return errors.Wrap(invocation.ErrIllegalTransition,
			"当前状态不允许该转移", "from", string(cur.State), "to", string(to))
	})
	if err != nil {
		return invocation.Entry{}, err
	}
	return out, nil
}

// getInTx 在事务内回读一条账目。
func (s *JournalStore) getInTx(ctx context.Context, tx *sql.Tx, sessionID, callID string) (invocation.Entry, error) {
	var (
		e   invocation.Entry
		st  string
		seq int64
	)
	err := tx.QueryRowContext(ctx,
		`SELECT `+journalCols+` FROM gak_invocation WHERE session_id = ? AND call_id = ?`,
		sessionID, callID).Scan(&e.SessionID, &e.CallID, &e.Tool, &e.ArgsDigest, &e.Args, &st, &e.Result, &e.Reason, &seq)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return invocation.Entry{}, errors.Wrap(invocation.ErrNotFound,
				"账目不存在", "session_id", sessionID, "call_id", callID)
		}
		return invocation.Entry{}, errors.Wrap(err, "sqliteadapter: 账目读取失败", "call_id", callID)
	}
	e.State = invocation.State(st)
	e.Seq = uint64(seq)
	return e, nil
}
