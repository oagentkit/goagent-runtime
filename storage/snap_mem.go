package storage

import (
	"context"
	"sync"

	"github.com/oekit/pkg/errors"

	"github.com/yi-nology/goagent-kernel/checkpoint"
)

// memSnapStore 进度快照的内存存取（序号单调）。
//
// 判据移植自 goagent-kernel examples/reference-runtime（MIT、同作者）：
// 序号 0 一律拒收；已持久化序号 >= 本次序号 ⇒ ErrStaleSeq 且不写入；
// Load 无记录返回 ErrNotFound。原子性来自进程内互斥锁——**只在单进程内
// 成立**；跨进程形态请用 sqlitestore.Open 或宿主自落库。
type memSnapStore struct {
	mu    sync.Mutex
	snaps map[string]checkpoint.Snapshot
}

func newMemSnapStore() *memSnapStore {
	return &memSnapStore{snaps: map[string]checkpoint.Snapshot{}}
}

// SaveIfNewer 仅当已持久化的序号**严格小于**本次序号时才写入。
func (s *memSnapStore) SaveIfNewer(_ context.Context, snap checkpoint.Snapshot) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if snap.Seq == 0 {
		return errors.Wrap(checkpoint.ErrStaleSeq,
			"序号为 0 的快照不是一次真实提交，不得落库（更不得覆盖既有快照）",
			"run_id", snap.RunID)
	}
	cur, ok := s.snaps[snap.RunID]
	if ok && cur.Seq >= snap.Seq {
		return errors.Wrap(checkpoint.ErrStaleSeq,
			"快照序号未严格递增", "run_id", snap.RunID, "seq", snap.Seq, "persisted", cur.Seq)
	}
	s.snaps[snap.RunID] = snap.Clone()
	return nil
}

// Save 满足 checkpoint.Store：同一 (RunID, Seq) 幂等。
func (s *memSnapStore) Save(_ context.Context, snap checkpoint.Snapshot) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	cur, ok := s.snaps[snap.RunID]
	if ok && cur.Seq == snap.Seq {
		return nil
	}
	if ok && cur.Seq > snap.Seq {
		return errors.Wrap(checkpoint.ErrStaleSeq, "快照序号回退", "run_id", snap.RunID)
	}
	s.snaps[snap.RunID] = snap.Clone()
	return nil
}

// Load 读取快照。无记录时返回 checkpoint.ErrNotFound（不得返回零值 + nil）。
func (s *memSnapStore) Load(_ context.Context, runID string) (checkpoint.Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	snap, ok := s.snaps[runID]
	if !ok {
		return checkpoint.Snapshot{}, errors.Wrap(checkpoint.ErrNotFound, "该运行无进度快照", "run_id", runID)
	}
	return snap.Clone(), nil
}
