// pg_snapshot_overflow_test.go —— 快照序号**溢出守卫**的判据（PG 侧）。
//
// 守卫（snapshot.go 的 SaveIfNewer）：序号列是有符号 BIGINT（64 位）⇒ 超过
// MaxInt64 的序号被截断成**负数** ⇒ 单调比较整个反向（负数 < 任何正数 ⇒ 永远
// 「更新」）⇒ 恢复点可被任意覆盖（陈旧慢写照样落库，且不报错）。
//
// 与 SQLite 侧（sqliteadapter（v0.2.0 自 goagent-kernel 迁入）/snapshot_overflow_test.go）**同款守卫语义**
// （超范围序号一律先拒、不得落库、既有快照不受影响）⇒ 但**判据不逐条对位**：
// SQLite 侧的溢出判据只有 1 条用例，**没有**「溢出拒绝须登记进批次失败槽」
// 这一维（那边快照面的批次失败槽覆盖在 apply_test.go 里走的是 Seq == 0 那条
// 拒绝，不是溢出这条）⇒ 本文件第二条用例是 PG 侧独有。
//
// # 为什么这条守卫必须登记进失败槽
//
// 溢出拒绝发生在**进事务之前**（与 Seq == 0 那条拒绝同款）⇒ 若不显式登记，
// 调用方吞掉这个错误继续走批内其它面时，本批仍会提交 ⇒「失败即作废整批」
// 对快照面是假的。故第二个用例专门钉这条登记。
//
// # 门控
//
// 本文件需要真实 PostgreSQL（GAK_PGREF_DSN，见 pg_test.go 的 requireDSN）：
// 没有可连的库时**显式跳过**并写明原因与起法，不在无库时伪造结果。
package pgstore

import (
	"context"
	"testing"

	"github.com/oekit/pkg/errors"

	"github.com/oagentkit/goagent-kernel/checkpoint"
)

// overflowSeq 超出有符号 64 位上限的序号（= MaxInt64 + 1，转成 int64 即负数）。
const overflowSeq = uint64(1) << 63

// TestPGSnapshotSaveIfNewerRejectsOverflowSeq 超范围序号必须报错且不落库。
//
// 去掉守卫 ⇒ 本用例必须红（err == nil ⇒ 落库成功 ⇒ 后续 Load 也读得到）。
func TestPGSnapshotSaveIfNewerRejectsOverflowSeq(t *testing.T) {
	dsn := requireDSN(t)
	ctx := applyCtx(t)
	s := freshStore(t, dsn)

	// 先落一份正常快照（证明后面的「没有落库」不是因为表里本来就没有）。
	if err := s.Snapshots().SaveIfNewer(ctx, checkpoint.Snapshot{
		RunID: "run-ok", Seq: 10, Status: "running",
	}); err != nil {
		t.Fatalf("存正常快照失败: %v", err)
	}

	err := s.Snapshots().SaveIfNewer(ctx, checkpoint.Snapshot{
		RunID: "run-overflow", Seq: overflowSeq, Status: "overflow",
	})
	if err == nil {
		t.Fatal("超出 BIGINT 范围的序号必须报错（截断成负数会让单调比较反向 ⇒ 恢复点可被任意覆盖）")
	}
	// 不得落库：这一 run 根本没有快照。
	if _, err := s.Snapshots().Load(ctx, "run-overflow"); !errors.Is(err, checkpoint.ErrNotFound) {
		t.Fatalf("超范围序号不得落库（实得 err=%v）", err)
	}
	// 既有的正常快照不得受影响（守卫是「拒写」，不是「写坏」）。
	got, err := s.Snapshots().Load(ctx, "run-ok")
	if err != nil {
		t.Fatalf("读快照失败: %v", err)
	}
	if got.Seq != 10 {
		t.Fatalf("既有快照被越界写影响: seq=%d", got.Seq)
	}
}

// TestPGSnapshotOverflowRegistersBatchFailure 溢出拒绝必须登记进批内失败槽。
//
// 判据形态照抄 pg_apply_faceslot_test.go 的失败槽覆盖面用例：调用方**吞掉**
// 溢出错误继续走批内其它面时，Apply 仍必须整批作废 ⇒ 证明这条拒绝确实经由
// 失败槽登记（否则「失败即作废整批」对快照面是假的）。
//
// 去掉守卫 ⇒ 本用例必须红（溢出写不再失败 ⇒ 失败槽无失败 ⇒ 整批照常提交 ⇒
// 批内那次成功写留下了）。
func TestPGSnapshotOverflowRegistersBatchFailure(t *testing.T) {
	dsn := requireDSN(t)
	ctx := applyCtx(t)
	s := freshStore(t, dsn)

	err := s.Apply(ctx, func(ctx context.Context) error {
		// 批内的一次成功写（先写：PG 里 SQL 错误会中止事务；这里走的是
		// 落库前的校验拒绝 ⇒ 事务仍然可用）。
		if werr := s.Snapshots().SaveIfNewer(ctx, checkpoint.Snapshot{
			RunID: "run-ok", Seq: 1, Status: "running",
		}); werr != nil {
			return werr
		}
		// 内层失败：错误被吞掉。
		_ = s.Snapshots().SaveIfNewer(ctx, checkpoint.Snapshot{
			RunID: "run-overflow", Seq: overflowSeq, Status: "overflow",
		})
		return nil
	})
	if err == nil {
		t.Fatal("溢出拒绝被吞掉时 Apply 不得报成功（本批已作废）")
	}
	// 整批作废 ⇒ 批内那次成功写不得留下。
	if _, err := s.Snapshots().Load(ctx, "run-ok"); !errors.Is(err, checkpoint.ErrNotFound) {
		t.Fatalf("快照面未登记失败 ⇒ 整批仍提交了（半截事务），实得 err=%v", err)
	}
}
