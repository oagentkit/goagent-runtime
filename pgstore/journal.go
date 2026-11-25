// journal.go —— invocation.Store 的 PostgreSQL 参考实现（工具调用账本）。
//
// 契约（goagent-kernel/invocation）：身份一经落账不可变 + 状态机只有一条路径
// + 「推进」必须在存储侧原子判定。
//
// # 三个动作的落库形态
//
//   - Plan：一条 INSERT … ON CONFLICT DO NOTHING。冲突时回读已落的那条，
//     比对身份（tool + args_digest）：一致 ⇒ 幂等无操作；不一致 ⇒
//     ErrIdentityConflict（同款口径见 sideeffect.ErrActionConflict）。
//     序号由**存储侧铸造**（IDENTITY 列）——调用方自带序号等于没有序号。
//   - Advance：一条**带条件**的 UPDATE（WHERE state = ANY(前驱集)）——
//     「比较与写入」在同一条语句里完成，不存在「先读后写」的窗口。前驱集由
//     内核的 invocation.Predecessors 给出（宿主自己反推转移表就是第二份
//     真相）。影响行数为 0 ⇒ 回读一次判「已是目标态（幂等）」还是
//     「非法转移」——两种都要给明确结论，不许静默放行。
//   - Get / List：普通读；不存在 ⇒ ErrNotFound（**不得**返回零值 + nil——
//     那会被上游误判成「这次调用从没发生过」⇒ 模型重新生成调用 ⇒ 重复施加）。
//
// # ⚠ 账本面**刻意不参与跨面批次**
//
// 账本必须在副作用**之前**落库（这是 P0-09 闭合的前提：崩溃后按账本续跑，
// 不再问模型要那次调用）。而跨面批次（Store.Apply）的提交点在批次**之后**
// ⇒ 账本若参与批次，它的落库就晚于副作用 ⇒ 崩溃窗口重新打开。
//
// ⇒ 本面的写一律走**自己的事务**（ownTx），不复用 ctx 里的批次事务。
//   代价：账本的写入与同批其它面的变更**不同生共死**——这是刻意的取舍：
//   同生共死会让「账本晚于副作用」变成常态（重复施加）；不同生共死只会
//   在「账本落了、别的事故没落」时留下一条**多记**的账目（恢复时据它对账，
//   不会重复施加）。
package pgstore

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/oekit/pkg/errors"

	"github.com/oagentkit/goagent-kernel/invocation"
)

// JournalStore 工具调用账本的 PostgreSQL 实现（invocation.Store）。
type JournalStore struct {
	pool *pgxpool.Pool
}

// NewJournalStore 构造账本存储。pool 归调用方管理（Close 由它负责）。
func NewJournalStore(pool *pgxpool.Pool) *JournalStore {
	return &JournalStore{pool: pool}
}

// journalCols 账目的列（读侧与写侧共用一份 ⇒ 两处不会漂）。
const journalCols = `session_id, call_id, tool, args_digest, args, state, result, reason, seq`

// ownTx 在**自己的事务**里跑一次操作（不复用 ctx 里的批次事务——见包头）。
func (s *JournalStore) ownTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return errors.Wrap(err, "pgstore: 开账目事务失败")
	}
	// 回滚兜底：回调 panic 时事务必须被释放（连接泄漏 = 后续写全挂）。
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return errors.Wrap(err, "pgstore: 提交账目事务失败")
	}
	return nil
}

// Plan 落一条待执行调用（身份一经落账不可变）。
//
// 幂等：同一 (会话, 调用) 且身份一致 ⇒ 幂等无操作（恢复重放同一计划时
// 不得报错）；身份不一致 ⇒ ErrIdentityConflict。
func (s *JournalStore) Plan(ctx context.Context, e invocation.Entry) error {
	if e.SessionID == "" || e.CallID == "" {
		return errors.New("pgstore: 账目身份不完整（会话与调用 ID 不得为空）")
	}
	if !invocation.Known(e.State) {
		return errors.New("pgstore: 账目状态不在内核词表内（读不懂的状态不许落库）")
	}
	return s.ownTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`INSERT INTO pgref_invocation (session_id, call_id, tool, args_digest, args, state, result, reason)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			 ON CONFLICT (session_id, call_id) DO NOTHING`,
			e.SessionID, e.CallID, e.Tool, e.ArgsDigest, e.Args, string(e.State), e.Result, e.Reason)
		if err != nil {
			return errors.Wrap(err, "pgstore: 账目落库失败", "session_id", e.SessionID, "call_id", e.CallID)
		}
		if tag.RowsAffected() > 0 {
			return nil
		}
		// 冲突 ⇒ 回读已落的那条，比对身份。
		var tool, digest string
		if err := tx.QueryRow(ctx,
			`SELECT tool, args_digest FROM pgref_invocation WHERE session_id = $1 AND call_id = $2`,
			e.SessionID, e.CallID).Scan(&tool, &digest); err != nil {
			return errors.Wrap(err, "pgstore: 账目冲突后回读失败", "call_id", e.CallID)
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
	var (
		e   invocation.Entry
		st  string
		seq int64
	)
	err := s.pool.QueryRow(ctx,
		`SELECT `+journalCols+` FROM pgref_invocation WHERE session_id = $1 AND call_id = $2`,
		sessionID, callID).Scan(&e.SessionID, &e.CallID, &e.Tool, &e.ArgsDigest, &e.Args, &st, &e.Result, &e.Reason, &seq)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return invocation.Entry{}, errors.Wrap(invocation.ErrNotFound,
				"账目不存在", "session_id", sessionID, "call_id", callID)
		}
		return invocation.Entry{}, errors.Wrap(err, "pgstore: 账目读取失败", "call_id", callID)
	}
	e.State = invocation.State(st)
	e.Seq = uint64(seq)
	return e, nil
}

// List 列出会话的全部账目（按序号升序）。
func (s *JournalStore) List(ctx context.Context, sessionID string) ([]invocation.Entry, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+journalCols+` FROM pgref_invocation WHERE session_id = $1 ORDER BY seq`, sessionID)
	if err != nil {
		return nil, errors.Wrap(err, "pgstore: 账目列举失败", "session_id", sessionID)
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
			return nil, errors.Wrap(err, "pgstore: 账目行扫描失败", "session_id", sessionID)
		}
		e.State = invocation.State(st)
		e.Seq = uint64(seq)
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, errors.Wrap(err, "pgstore: 账目行读取失败", "session_id", sessionID)
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
	err := s.ownTx(ctx, func(tx pgx.Tx) error {
		// 一条带条件的 UPDATE：比较与写入在同一条语句里完成。
		// 前驱集来自内核（宿主自己反推转移表就是第二份真相）。
		//
		// ⚠ 前驱集为空（没有任何状态能转到目标态）⇒ **不得**执行这条 UPDATE
		// （否则条件退化成「只按键匹配」，会把非法转移放行）。
		if len(preds) > 0 {
			names := make([]string, 0, len(preds))
			for _, p := range preds {
				names = append(names, string(p))
			}
			tag, err := tx.Exec(ctx,
				`UPDATE pgref_invocation
				 SET state = $1, result = $2, reason = $3
				 WHERE session_id = $4 AND call_id = $5 AND state = ANY($6)`,
				string(to), u.Result, u.Reason, sessionID, callID, names)
			if err != nil {
				return errors.Wrap(err, "pgstore: 账目推进失败", "call_id", callID)
			}
			if tag.RowsAffected() > 0 {
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
func (s *JournalStore) getInTx(ctx context.Context, tx pgx.Tx, sessionID, callID string) (invocation.Entry, error) {
	var (
		e   invocation.Entry
		st  string
		seq int64
	)
	err := tx.QueryRow(ctx,
		`SELECT `+journalCols+` FROM pgref_invocation WHERE session_id = $1 AND call_id = $2`,
		sessionID, callID).Scan(&e.SessionID, &e.CallID, &e.Tool, &e.ArgsDigest, &e.Args, &st, &e.Result, &e.Reason, &seq)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return invocation.Entry{}, errors.Wrap(invocation.ErrNotFound,
				"账目不存在", "session_id", sessionID, "call_id", callID)
		}
		return invocation.Entry{}, errors.Wrap(err, "pgstore: 账目读取失败", "call_id", callID)
	}
	e.State = invocation.State(st)
	e.Seq = uint64(seq)
	return e, nil
}
