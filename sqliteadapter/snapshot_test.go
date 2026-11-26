// snapshot_test.go —— 进度快照面（checkpoint.StoreCAS）的判据。
//
// 对位 pgstore（v0.2.0 自 goagent-kernel 迁入）/snapshot.go：契约两条（SaveIfNewer 的单调闸 / Load 的
// ErrNotFound），落库形态一条（带条件的 upsert，陈旧写影响行数 0 ⇒ 判陈旧）。
// 内核没有独立的 checkpoint 契约套件包 ⇒ 判据按 PG 侧同款用例逐条对位落在
// 这里（形态同构、判据各写一份——共用会把两个引擎耦在一起）。
//
// 本文件钉住的四条不变量：
//
//  1. **单调闸**：已持久化序号 ≥ 本次序号 ⇒ 拒写且**不覆盖**（陈旧写影响
//     行数 0）。这是跨进程单调性的唯一依据（内核的进程内 SeqGate 只管本进程）。
//  2. **序号 0 不是一次真实提交** ⇒ 拒写。否则它是**唯一能覆盖既有快照**的
//     写（恢复点静默回退，且不报错）。
//  3. **无记录 ⇒ ErrNotFound**：不得返回零值 + nil——上游会误判成「这个运行
//     从没跑过」从而全量重跑，重复副作用正是续跑要避免的那件事。
//  4. **并发提交下序号只进不退**（含两个句柄并发 = 跨进程形态）。
package sqliteadapter

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/oekit/pkg/errors"

	"github.com/yi-nology/goagent-kernel/checkpoint"
)

// snapCtx 用例的 ctx（带超时：回归要**快速红**，而不是挂在 60s 超时上）。
func snapCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// TestSnapshotSaveIfNewerMonotonic 序号更小的快照不得覆盖更大的（单调）。
func TestSnapshotSaveIfNewerMonotonic(t *testing.T) {
	ctx := snapCtx(t)
	s := openStore(t)
	if err := s.Snapshots().SaveIfNewer(ctx, checkpoint.Snapshot{
		RunID: "r1", Seq: 5, Status: "running",
	}); err != nil {
		t.Fatalf("存快照失败: %v", err)
	}
	err := s.Snapshots().SaveIfNewer(ctx, checkpoint.Snapshot{
		RunID: "r1", Seq: 3, Status: "stale",
	})
	if !errors.Is(err, checkpoint.ErrStaleSeq) {
		t.Fatalf("更旧的快照必须被单调闸拒（ErrStaleSeq），实得 %v", err)
	}
	got, err := s.Snapshots().Load(ctx, "r1")
	if err != nil {
		t.Fatalf("读快照失败: %v", err)
	}
	if got.Seq != 5 {
		t.Fatalf("旧快照覆盖了新快照: seq=%d", got.Seq)
	}
	if got.Status != "running" {
		t.Fatalf("陈旧写不得改动任何字段: status=%q", got.Status)
	}
}

// TestSnapshotSaveIfNewerRejectsZeroSeq 序号 0 不是一次真实提交 ⇒ 拒写。
//
// 这一条钉的是「seq=0 是**唯一**能覆盖既有快照的写」那个洞（契约 v0.33.0 起
// 明写）：若实现把 0 当成「不参与单调判定」照写，已有 Seq=10 时一份 Seq=0 的
// 快照照样落库 ⇒ 恢复点静默回退且不报错。
func TestSnapshotSaveIfNewerRejectsZeroSeq(t *testing.T) {
	ctx := snapCtx(t)
	s := openStore(t)
	if err := s.Snapshots().SaveIfNewer(ctx, checkpoint.Snapshot{
		RunID: "r1", Seq: 10, Status: "running",
	}); err != nil {
		t.Fatalf("存快照失败: %v", err)
	}
	err := s.Snapshots().SaveIfNewer(ctx, checkpoint.Snapshot{
		RunID: "r1", Seq: 0, Status: "zero",
	})
	if !errors.Is(err, checkpoint.ErrStaleSeq) {
		t.Fatalf("序号 0 的快照必须被拒（ErrStaleSeq），实得 %v", err)
	}
	got, err := s.Snapshots().Load(ctx, "r1")
	if err != nil {
		t.Fatalf("读快照失败: %v", err)
	}
	if got.Seq != 10 || got.Status != "running" {
		t.Fatalf("序号 0 的快照不得落库: seq=%d status=%q", got.Seq, got.Status)
	}
}

// TestSnapshotLoadNotFound 无记录必须返回 ErrNotFound（不得零值 + nil）。
func TestSnapshotLoadNotFound(t *testing.T) {
	ctx := snapCtx(t)
	s := openStore(t)
	got, err := s.Snapshots().Load(ctx, "r-missing")
	if !errors.Is(err, checkpoint.ErrNotFound) {
		t.Fatalf("无记录必须返回 ErrNotFound，实得 %v", err)
	}
	if got.RunID != "" || got.Seq != 0 {
		t.Fatalf("ErrNotFound 时不得返回半截快照: %+v", got)
	}
}

// TestSnapshotRoundTripsDoneAndFields Done 列表与 Fields 开放键往返一致。
//
// 契约要求宿主实现**拷贝**而非持引用：快照若被调用方在落库后继续改动，恢复
// 判定的依据就不再可信（checkpoint.Snapshot.Clone 的同款义务）。
func TestSnapshotRoundTripsDoneAndFields(t *testing.T) {
	ctx := snapCtx(t)
	s := openStore(t)
	in := checkpoint.Snapshot{
		RunID:  "r1",
		Seq:    3,
		Status: "running",
		Done:   []string{"step-1", "step-2"},
		Fields: map[string]string{"mode": "fast", "lane": "a"},
	}
	if err := s.Snapshots().SaveIfNewer(ctx, in); err != nil {
		t.Fatalf("存快照失败: %v", err)
	}
	got, err := s.Snapshots().Load(ctx, "r1")
	if err != nil {
		t.Fatalf("读快照失败: %v", err)
	}
	if got.Seq != in.Seq || got.Status != in.Status {
		t.Fatalf("标量字段往返不一致: %+v", got)
	}
	if len(got.Done) != len(in.Done) {
		t.Fatalf("Done 往返长度不一致: %d vs %d", len(got.Done), len(in.Done))
	}
	for i := range in.Done {
		if got.Done[i] != in.Done[i] {
			t.Fatalf("Done 往返顺序/内容不一致: %v", got.Done)
		}
	}
	if got.Fields["mode"] != "fast" || got.Fields["lane"] != "a" {
		t.Fatalf("Fields 往返不一致: %v", got.Fields)
	}
	// 落库后调用方改动原快照 ⇒ 库里的内容不得跟着变（拷贝语义）。
	in.Done[0] = "mutated"
	in.Fields["mode"] = "mutated"
	again, err := s.Snapshots().Load(ctx, "r1")
	if err != nil {
		t.Fatalf("读快照失败: %v", err)
	}
	if again.Done[0] != "step-1" || again.Fields["mode"] != "fast" {
		t.Fatalf("落库后不得持调用方引用（拷贝语义）: %+v", again)
	}
}

// TestSnapshotConcurrentSavesKeepHighestSeq 并发提交下序号只进不退。
//
// 单调闸是**存储侧**的判定（checkpoint.StoreCAS 的立论）：进程内再多把锁
// 也判不出跨进程的事，但同一判据在并发下同样必须成立——否则「谁最后写谁赢」
// 就是恢复点静默回退的入口。
func TestSnapshotConcurrentSavesKeepHighestSeq(t *testing.T) {
	ctx := snapCtx(t)
	s := openStore(t)
	const n = 8
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 1; i <= n; i++ {
		wg.Add(1)
		go func(seq uint64) {
			defer wg.Done()
			if err := s.Snapshots().SaveIfNewer(ctx, checkpoint.Snapshot{
				RunID: "r1", Seq: seq, Status: "running",
			}); err != nil && !errors.Is(err, checkpoint.ErrStaleSeq) {
				errs <- err
			}
		}(uint64(i))
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("并发提交只允许 ErrStaleSeq，实得 %v", err)
	}
	got, err := s.Snapshots().Load(ctx, "r1")
	if err != nil {
		t.Fatalf("读快照失败: %v", err)
	}
	if got.Seq != n {
		t.Fatalf("并发提交后落库的必须是最大序号 %d，实得 %d", n, got.Seq)
	}
}

// TestSnapshotCASAcrossTwoHandles 两个独立句柄并发提交同一 runID（跨进程形态）。
//
// 单进程内的并发证明不了「跨进程」的判定，但 SQLite 的锁是**文件级**的 ⇒
// 两个独立 Store 句柄指向同一个库文件，就是跨进程竞争的等效形态（PG 侧靠子
// 进程证，本侧靠第二个句柄证——同样的存储侧判定，不同的引擎机制）。
func TestSnapshotCASAcrossTwoHandles(t *testing.T) {
	ctx := snapCtx(t)
	path := filepath.Join(t.TempDir(), "state.db")
	first, err := Open(path)
	if err != nil {
		t.Fatalf("开第一个句柄失败: %v", err)
	}
	defer func() { _ = first.Close() }()
	if err := first.EnsureSchema(ctx); err != nil {
		t.Fatalf("建表失败: %v", err)
	}
	second, err := Open(path)
	if err != nil {
		t.Fatalf("开第二个句柄失败: %v", err)
	}
	defer func() { _ = second.Close() }()

	if err := first.Snapshots().SaveIfNewer(ctx, checkpoint.Snapshot{
		RunID: "r1", Seq: 5, Status: "running",
	}); err != nil {
		t.Fatalf("存快照失败: %v", err)
	}
	// 另一个句柄（= 另一个进程）拿更旧的序号来写 ⇒ 必须被拒。
	err = second.Snapshots().SaveIfNewer(ctx, checkpoint.Snapshot{
		RunID: "r1", Seq: 3, Status: "stale",
	})
	if !errors.Is(err, checkpoint.ErrStaleSeq) {
		t.Fatalf("跨句柄的陈旧写必须被拒（ErrStaleSeq），实得 %v", err)
	}
	got, err := second.Snapshots().Load(ctx, "r1")
	if err != nil {
		t.Fatalf("读快照失败: %v", err)
	}
	if got.Seq != 5 {
		t.Fatalf("陈旧写覆盖了新快照: seq=%d", got.Seq)
	}
}

// TestSnapshotCommitterComposes 与内核提交器（checkpoint.NewCommitter）合流。
//
// 这一条证明本实现满足的是**内核契约**而不只是「像」：提交器的次序
// （进程内闸 Accept → 存储侧 CAS → 失败才 Release）在真存储上跑通，且
// 同序号重试在竞争之后仍被拒（闸不是一次性的）。
func TestSnapshotCommitterComposes(t *testing.T) {
	ctx := snapCtx(t)
	s := openStore(t)
	gate := checkpoint.NewSeqGate()
	committer := checkpoint.NewCommitter(gate, s.Snapshots())
	snap := checkpoint.Snapshot{RunID: "r1", Seq: 1, Status: "running"}

	if err := committer.Commit(ctx, snap); err != nil {
		t.Fatalf("首次提交失败: %v", err)
	}
	// 同序号重试：进程内闸先拒（存储侧也会拒）。
	if err := committer.Commit(ctx, snap); !errors.Is(err, checkpoint.ErrStaleSeq) {
		t.Fatalf("同序号重试必须被拒（ErrStaleSeq），实得 %v", err)
	}
	// 更大的序号可以提交（闸仍立着，只是不挡前进）。
	if err := committer.Commit(ctx, checkpoint.Snapshot{RunID: "r1", Seq: 2, Status: "running"}); err != nil {
		t.Fatalf("更大序号必须能提交: %v", err)
	}
	got, err := s.Snapshots().Load(ctx, "r1")
	if err != nil {
		t.Fatalf("读快照失败: %v", err)
	}
	if got.Seq != 2 {
		t.Fatalf("落库序号应为 2，实得 %d", got.Seq)
	}
}
