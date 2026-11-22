// snapshot.go —— checkpoint.StoreCAS 的 PostgreSQL 参考实现。
//
// 契约只有两条，落库形态也是两条：
//
//   - SaveIfNewer：一条带条件的 upsert（WHERE 已持久化序号 < 本次序号），
//     影响行数为 0 ⇒ 判陈旧（ErrStaleSeq），**不写入**；Seq == 0 一律先拒
//     （它不是一次真实提交，放它落库等于给了它唯一能覆盖既有快照的写）；
//     序号超过 MaxInt64 一律先拒（序号列是有符号 64 位 ⇒ 高位被截成负数
//     ⇒ 单调比较反向 ⇒ 恢复点可被任意覆盖）。
//     并发提交同一 run 时，两个进程的条件更新在行锁上排队，只有序号更大的
//     那个落库——单调闸在存储侧成立，不在进程侧。
//   - Load：无记录返回 ErrNotFound（**不得**返回零值 + nil——那会被上游
//     误判成「这个运行从没跑过」从而全量重跑）。
package pgstore

import (
	"context"
	"encoding/json"
	"math"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/oekit/pkg/errors"

	"github.com/yi-nology/goagent-kernel/checkpoint"
)

// SnapStore checkpoint.StoreCAS 的 PostgreSQL 参考实现。
type SnapStore struct {
	pool *pgxpool.Pool
}

// NewSnapStore 构造进度快照存储。
func NewSnapStore(pool *pgxpool.Pool) *SnapStore {
	return &SnapStore{pool: pool}
}

// SaveIfNewer 仅当已持久化的序号**严格小于**本次序号时才写入。
//
// 陈旧写（序号 ≤ 已持久化序号）返回 checkpoint.ErrStaleSeq 且**什么都没写**
// （条件 upsert 的影响行数为 0 即判陈旧——「比较与写入」在同一条语句里完成，
// 不存在「先读后写」的窗口）。
//
// # 事务边界
//
// 写走 runTx ⇒ **批内复用本批的 tx**（与各面同一次提交 ⇒ 快照与其它面的
// 变更同生共死）；批外自开事务（单条 upsert 本身即原子 ⇒ 包一层事务不改变
// 语义，只是让批内能同生共死）。
func (s *SnapStore) SaveIfNewer(ctx context.Context, snap checkpoint.Snapshot) error {
	if snap.Seq == 0 {
		err := errors.Wrap(checkpoint.ErrStaleSeq,
			"序号为 0 的快照不是一次真实提交，不得落库（更不得覆盖既有快照）",
			"run_id", snap.RunID)
		// 批内 ⇒ 登记失败：调用方**可能吞掉**它 ⇒ 本批必须作废。
		// （这条拒绝发生在**进事务之前** ⇒ 不登记的话，「失败即作废整批」对
		// 快照面是假的。）
		failBatch(ctx, err)
		return err
	}
	if snap.Seq > math.MaxInt64 {
		// 序号列是有符号 BIGINT（64 位）：超过 MaxInt64 的序号被截断成**负数**
		// ⇒ 单调比较整个反向（负数 < 任何正数 ⇒ 永远「更新」）⇒ 恢复点可被
		// 任意覆盖（陈旧慢写照样落库，且不报错）。
		err := errors.New("pgstore: 快照序号超出 BIGINT 范围（run_id=" + snap.RunID + "）")
		// 批内 ⇒ 登记失败：调用方**可能吞掉**它 ⇒ 本批必须作废。
		// （与 Seq == 0 那条拒绝同款：都发生在**进事务之前** ⇒ 不登记的话，
		// 「失败即作废整批」对快照面是假的。）
		failBatch(ctx, err)
		return err
	}
	fields, done, err := encodeSnapshot(snap)
	if err != nil {
		// ⚠ 本分支当前**不可达**（checkpoint.Snapshot.Fields 是 map[string]string
		// ⇒ json.Marshal 不会失败）⇒ 这一处登记点**无法补判据**（没有路径可
		// 走）。保留登记：若将来 Fields 变成可序列化失败的类型，它就是承重的。
		failBatch(ctx, err)
		return err
	}
	_, err = withinTx(ctx, s.pool, func(tx pgx.Tx) (int64, error) {
		tag, err := tx.Exec(ctx,
			`INSERT INTO pgref_snapshot (run_id, seq, status, done, fields)
			 VALUES ($1, $2, $3, $4, $5)
			 ON CONFLICT (run_id) DO UPDATE SET
			     seq = EXCLUDED.seq, status = EXCLUDED.status,
			     done = EXCLUDED.done, fields = EXCLUDED.fields
			 WHERE pgref_snapshot.seq < EXCLUDED.seq`,
			snap.RunID, int64(snap.Seq), snap.Status, done, fields)
		if err != nil {
			return 0, errors.Wrap(err, "pgstore: 快照落库失败", "run_id", snap.RunID)
		}
		if tag.RowsAffected() == 0 {
			// 陈旧写：在**事务内**判 ⇒ 失败经 runTx 登记进失败槽（批内必须
			// 作废整批），且此刻什么都没写。
			return 0, errors.Wrap(checkpoint.ErrStaleSeq,
				"快照序号未严格递增（不得覆盖）",
				"run_id", snap.RunID, "seq", snap.Seq)
		}
		return 1, nil
	})
	return err
}

// Load 读取快照。无记录返回 checkpoint.ErrNotFound，不得返回零值 + nil。
func (s *SnapStore) Load(ctx context.Context, runID string) (checkpoint.Snapshot, error) {
	var seq int64
	var status string
	var done []string
	var fields string
	err := s.pool.QueryRow(ctx,
		`SELECT seq, status, done, fields FROM pgref_snapshot WHERE run_id = $1`, runID).
		Scan(&seq, &status, &done, &fields)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return checkpoint.Snapshot{}, errors.Wrap(checkpoint.ErrNotFound, "该运行无进度快照", "run_id", runID)
		}
		return checkpoint.Snapshot{}, errors.Wrap(err, "pgstore: 读快照失败", "run_id", runID)
	}
	snap := checkpoint.Snapshot{
		RunID:  runID,
		Seq:    uint64(seq),
		Status: status,
		Done:   append([]string(nil), done...),
	}
	if fields != "" {
		if err := json.Unmarshal([]byte(fields), &snap.Fields); err != nil {
			return checkpoint.Snapshot{}, errors.Wrap(err, "pgstore: 快照字段解析失败", "run_id", runID)
		}
	}
	return snap, nil
}

// encodeSnapshot 把 Done / Fields 压进列（宿主按自己的序列化习惯来，
// 这里 Fields 用 JSON、Done 用 TEXT[]，仅为参考；Done 保持宿主追加的顺序）。
// nil 切片必须转成**空数组**：列是 NOT NULL，pgx 把 nil 编码成 NULL。
func encodeSnapshot(snap checkpoint.Snapshot) (string, []string, error) {
	done := make([]string, 0, len(snap.Done))
	done = append(done, snap.Done...)
	fields := ""
	if len(snap.Fields) > 0 {
		b, err := json.Marshal(snap.Fields)
		if err != nil {
			return "", nil, errors.Wrap(err, "pgstore: 快照字段序列化失败", "run_id", snap.RunID)
		}
		fields = string(b)
	}
	return fields, done, nil
}
