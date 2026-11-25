// snapshot_overflow_test.go —— 快照序号**溢出守卫**的判据（Task 5 评审 M3）。
//
// 守卫（snapshot.go 的 SaveIfNewer）：序号列是 INTEGER（有符号 64 位）⇒ 超过
// MaxInt64 的序号被截断成**负数** ⇒ 单调比较整个反向（负数 < 任何正数 ⇒ 永远
// 「更新」）⇒ 恢复点可被任意覆盖（陈旧慢写照样落库，且不报错）。
//
// 同型的对偶守卫（游标溢出）已有判据（TestTailCursorStoreRejectsOverflow）⇒
// 这一条是**遗漏**而非取舍：删掉守卫没有任何用例会红。
package sqliteadapter

import (
	"testing"

	"github.com/oekit/pkg/errors"

	"github.com/oagentkit/goagent-kernel/checkpoint"
)

// TestSnapshotSaveIfNewerRejectsOverflowSeq 超范围序号必须报错且不落库。
func TestSnapshotSaveIfNewerRejectsOverflowSeq(t *testing.T) {
	ctx := snapCtx(t)
	s := openStore(t)
	// 先落一份正常快照（证明后面的「没有落库」不是因为表里本来就没有）。
	if err := s.Snapshots().SaveIfNewer(ctx, checkpoint.Snapshot{
		RunID: "r1", Seq: 10, Status: "running",
	}); err != nil {
		t.Fatalf("存快照失败: %v", err)
	}
	overflow := uint64(1) << 63 // = MaxInt64 + 1；转成 int64 即变成负数
	err := s.Snapshots().SaveIfNewer(ctx, checkpoint.Snapshot{
		RunID: "r-overflow", Seq: overflow, Status: "overflow",
	})
	if err == nil {
		t.Fatal("超出 INTEGER 范围的序号必须报错（截断成负数会让单调比较反向 ⇒ 恢复点可被任意覆盖）")
	}
	// 不得落库：这一 run 根本没有快照。
	if _, err := s.Snapshots().Load(ctx, "r-overflow"); !errors.Is(err, checkpoint.ErrNotFound) {
		t.Fatalf("超范围序号不得落库（实得 err=%v）", err)
	}
	// 既有的正常快照不得受影响（守卫是「拒写」，不是「写坏」）。
	got, err := s.Snapshots().Load(ctx, "r1")
	if err != nil {
		t.Fatalf("读快照失败: %v", err)
	}
	if got.Seq != 10 {
		t.Fatalf("既有快照被越界写影响: seq=%d", got.Seq)
	}
}
