// snapshot.go —— checkpoint.StoreCAS 的 SQLite 参考实现。
//
// 契约两条，落库形态一条：
//
//   - SaveIfNewer：一条带条件的 upsert（WHERE 已持久化序号 < 本次序号），
//     影响行数为 0 ⇒ 判陈旧（ErrStaleSeq），**不写入**；Seq == 0 一律先拒
//     （它不是一次真实提交，放它落库等于给了它唯一能覆盖既有快照的写）；
//     序号超过 MaxInt64 一律先拒（序号列是 INTEGER，即有符号 64 位 ⇒ 高位
//     被截断成**负数** ⇒ 单调比较反向 ⇒ 恢复点可被任意覆盖）。
//     并发提交同一 run 时，两个写者的条件更新在行锁上排队，只有序号更大的
//     那个落库——单调闸在存储侧成立，不在进程侧。
//   - Load：无记录返回 ErrNotFound（**不得**返回零值 + nil——那会被上游
//     误判成「这个运行从没跑过」从而全量重跑，重复副作用正是续跑要避免的）。
//
// # 方言映射（PG → SQLite）
//
//   - 占位符 $1/$2 → ?；TEXT[]（Done）→ TEXT（JSON 数组）；JSONB（Fields）
//     → TEXT（JSON 对象）；BIGINT → INTEGER。
//   - PG 的 ON CONFLICT … DO UPDATE … WHERE 形态在 SQLite 同款可用（占位符与
//     excluded 引用方式一致）⇒ **判据逐字同构**，只有方言不同。
//
// # 事务边界
//
// 写走写句柄的一个事务（独立路径）；读（Load）走**只读句柄**——写句柄只有
// 一条连接，事务占用它期间从写句柄取连接会永久等待（实测：挂到 ctx 超时）。
package sqliteadapter

import (
	"context"
	"database/sql"
	"encoding/json"
	"math"

	"github.com/oekit/pkg/errors"

	"github.com/oagentkit/goagent-kernel/checkpoint"
)

// SnapStore checkpoint.StoreCAS 的 SQLite 参考实现。
type SnapStore struct {
	// exec 执行位置：db 是写句柄，read 是只读句柄（独立读路径走它）。
	exec exec
}

// 编译期断言：本实现满足进度快照的跨进程存取契约。
var _ checkpoint.StoreCAS = (*SnapStore)(nil)

// Snapshots 进度快照面（与其它面共用同一份事务域）。
func (s *Store) Snapshots() *SnapStore { return &SnapStore{exec: s.execBase()} }

// SaveIfNewer 仅当已持久化的序号**严格小于**本次序号时才写入。
//
// 陈旧写（序号 ≤ 已持久化序号）返回 checkpoint.ErrStaleSeq 且**什么都没写**
// （条件 upsert 的影响行数为 0 即判陈旧——「比较与写入」在同一条语句里完成，
// 不存在「先读后写」的窗口）。
func (s *SnapStore) SaveIfNewer(ctx context.Context, snap checkpoint.Snapshot) error {
	// ⚠ 下面三道拒绝都发生在 **exec.run 之外** ⇒ 必须显式登记进失败槽：
	// 否则调用方吞掉这个错误时整批仍会提交（「失败即作废整批」对这条路径
	// 是假的）。
	if snap.Seq == 0 {
		err := errors.Wrap(checkpoint.ErrStaleSeq,
			"序号为 0 的快照不是一次真实提交，不得落库（更不得覆盖既有快照）",
			"run_id", snap.RunID)
		s.exec.fail(err)
		return err
	}
	if snap.Seq > math.MaxInt64 {
		// 序号列是 INTEGER（有符号 64 位）：高位被截断成负数 ⇒ 单调比较整个
		// 反向（负数 < 任何正数 ⇒ 永远「更新」）⇒ 恢复点可被任意覆盖。
		err := errors.New("sqliteadapter: 快照序号超出 INTEGER 范围（run_id=" + snap.RunID + "）")
		s.exec.fail(err)
		return err
	}
	done, fields, err := encodeSnapshot(snap)
	if err != nil {
		s.exec.fail(err)
		return err
	}
	return s.exec.run(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`INSERT INTO gak_snapshot (run_id, seq, status, done, fields)
			 VALUES (?, ?, ?, ?, ?)
			 ON CONFLICT (run_id) DO UPDATE SET
			     seq = excluded.seq, status = excluded.status,
			     done = excluded.done, fields = excluded.fields
			 WHERE gak_snapshot.seq < excluded.seq`,
			snap.RunID, int64(snap.Seq), snap.Status, done, fields)
		if err != nil {
			return errors.Wrap(err, "sqliteadapter: 快照落库失败", "run_id", snap.RunID)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return errors.Wrap(checkpoint.ErrStaleSeq,
				"快照序号未严格递增（不得覆盖）",
				"run_id", snap.RunID, "seq", snap.Seq)
		}
		return nil
	})
}

// Load 读取快照。无记录返回 checkpoint.ErrNotFound，不得返回零值 + nil。
func (s *SnapStore) Load(ctx context.Context, runID string) (checkpoint.Snapshot, error) {
	q, err := s.exec.reader()
	if err != nil {
		return checkpoint.Snapshot{}, err
	}
	var (
		seq    int64
		status string
		done   string
		fields string
	)
	err = q.QueryRowContext(ctx,
		`SELECT seq, status, done, fields FROM gak_snapshot WHERE run_id = ?`, runID).
		Scan(&seq, &status, &done, &fields)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return checkpoint.Snapshot{}, errors.Wrap(checkpoint.ErrNotFound,
				"该运行无进度快照", "run_id", runID)
		}
		return checkpoint.Snapshot{}, errors.Wrap(err, "sqliteadapter: 读快照失败", "run_id", runID)
	}
	snap := checkpoint.Snapshot{
		RunID:  runID,
		Seq:    uint64(seq),
		Status: status,
	}
	// 落库行是新解析出来的切片/映射 ⇒ 不与调用方共享底层数组
	//（checkpoint.Snapshot.Clone 的同款拷贝义务）。
	if done != "" && done != "null" {
		if err := json.Unmarshal([]byte(done), &snap.Done); err != nil {
			return checkpoint.Snapshot{}, errors.Wrap(err,
				"sqliteadapter: 快照已完成步骤解析失败", "run_id", runID)
		}
	}
	if fields != "" {
		if err := json.Unmarshal([]byte(fields), &snap.Fields); err != nil {
			return checkpoint.Snapshot{}, errors.Wrap(err,
				"sqliteadapter: 快照字段解析失败", "run_id", runID)
		}
	}
	return snap, nil
}

// encodeSnapshot 把 Done / Fields 压进列（宿主按自己的序列化习惯来；这里 Done
// 与 Fields 都用 JSON 文本，Done 保持调用方追加的顺序）。
//
// nil 切片/映射落成空值（列是 NOT NULL）：Done 落 "[]"、Fields 落 ""。
func encodeSnapshot(snap checkpoint.Snapshot) (string, string, error) {
	done := "[]"
	if len(snap.Done) > 0 {
		b, err := json.Marshal(snap.Done)
		if err != nil {
			return "", "", errors.Wrap(err, "sqliteadapter: 快照已完成步骤序列化失败", "run_id", snap.RunID)
		}
		done = string(b)
	}
	fields := ""
	if len(snap.Fields) > 0 {
		b, err := json.Marshal(snap.Fields)
		if err != nil {
			return "", "", errors.Wrap(err, "sqliteadapter: 快照字段序列化失败", "run_id", snap.RunID)
		}
		fields = string(b)
	}
	return done, fields, nil
}
