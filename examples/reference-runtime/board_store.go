// board_store.go —— taskboard.Board 的参考实现（内存版）。
//
// 生命周期: Experimental（v0.33.0 定级）——本包仍会改、删、拆，不承诺任何
// 兼容，升级风险自负；定级理由见 api-lifecycle.yaml，分级规则见
// docs/api-compat.md。
//
// # 令牌由存储铸造（v0.33.0 的要害）
//
// 认领请求里**没有令牌**：调用方只说「要多长的租约」，令牌由本存储铸出来
// 并随 Task 返回。调用方自带令牌的话，「释放/完成只认 token」这条契约
// 名存实亡——它能造出任意 token，于是「输家的迟到释放放掉赢家的闸」
// 又回来了，而报告里一切正常。
//
// 到期时刻也由**存储自己的时钟**算：调用方的时钟与存储的时钟可能对不上，
// 跨进程时更不能假设一致。
//
// # 续期与接管代次（v0.33.0 起，issue #87）
//
// 本存储实现 taskboard.RenewingBoard：Renew 是**资格检查**不是心跳——
// 校验令牌仍是当前租约的、且未过期，通过才把到期时刻推后（同一把锁内完成，
// 原子）。接管代次 Lease.Fence 与令牌同一次铸造，同一任务上严格递增。
package main

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/oekit/pkg/errors"

	"github.com/yi-nology/goagent-kernel/taskboard"
)

// boardStore 任务认领面：认领是读-判-写，必须原子。
type boardStore struct {
	mu    sync.Mutex
	tasks map[taskboard.TaskID]taskboard.Task

	// seq 令牌计数器：每次认领铸一枚新令牌（不复用 ⇒ 旧令牌无法重用）。
	seq uint64

	// now 可注入时钟（过期判定必须读它，否则过期场景恒不触发）。
	now func() time.Time
}

func newBoardStore() *boardStore {
	return &boardStore{
		tasks: map[taskboard.TaskID]taskboard.Task{},
		now:   time.Now,
	}
}

// UseClock 注入时钟（boardtest 契约套件用）。
func (s *boardStore) UseClock(now func() time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if now != nil {
		s.now = now
	}
}

// clock 当前时刻。**先取回调、解锁后再调**（issue #317）：注入的时钟可能
// 重入本包，持锁调用即死锁。调用方也不得在持有 s.mu 时调本函数。
func (s *boardStore) clock() time.Time {
	s.mu.Lock()
	now := s.now
	s.mu.Unlock()
	if now == nil {
		return time.Now()
	}
	return now()
}

// Seed 摆初始任务（契约里没有「创建」入口：任务拓扑是宿主的资产）。
func (s *boardStore) Seed(tasks ...taskboard.Task) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, t := range tasks {
		if t.ID == "" {
			continue
		}
		t.Seq++
		// Clone 收口 Board 契约的「快照不共享 Deps 底层数组」（issue #379）。
		s.tasks[t.ID] = t.Clone()
	}
}

// List 返回一致快照（不是边改边读的流）。
func (s *boardStore) List(_ context.Context) ([]taskboard.Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]taskboard.Task, 0, len(s.tasks))
	for _, t := range s.tasks {
		out = append(out, t.Clone())
	}
	slices.SortFunc(out, func(a, b taskboard.Task) int { return cmp.Compare(a.ID, b.ID) })
	return out, nil
}

// Claim 认领一个任务。**读-判-写不可分割**：这里靠同一把互斥锁保证只有一个赢家。
//
// 令牌由本存储铸造（见文件头注释）；到期时刻按存储自己的时钟算。
func (s *boardStore) Claim(_ context.Context, id taskboard.TaskID, agent taskboard.AgentID, req taskboard.ClaimRequest) (taskboard.Task, error) {
	now := s.clock() // 锁外取存储时钟（issue #317）
	s.mu.Lock()
	defer s.mu.Unlock()

	t, ok := s.tasks[id]
	if !ok {
		return taskboard.Task{}, errors.Wrap(taskboard.ErrNotFound, "任务不存在", "task", string(id))
	}
	// 状态可认领判定统一走 taskboard.State.Claimable（issue #360）：
	// 词表外 → ErrUnknownState，终态 → ErrTerminal；只判 Terminal() 会让
	// 词表外状态被当成「可领」。
	if cerr := t.State.Claimable(); cerr != nil {
		return taskboard.Task{}, errors.Wrap(cerr, "任务状态不可认领",
			"task", string(id), "state", string(t.State))
	}
	for _, dep := range t.Deps {
		d, ok := s.tasks[dep]
		if !ok || !d.State.Terminal() {
			return taskboard.Task{}, errors.Wrap(taskboard.ErrBlocked,
				"依赖未完成", "task", string(id), "dep", string(dep))
		}
	}
	if taskboard.LeaseHeld(t, now) {
		return taskboard.Task{}, errors.Wrap(taskboard.ErrTaken, "任务已被他人认领", "task", string(id))
	}

	// 铸一枚新令牌：每次认领都换新 ⇒ 上一轮的令牌无法重用（ABA）。
	// 接管代次与令牌**同一次铸造**（同源）：seq 单调递增 ⇒ 同一任务上严格递增。
	s.seq++
	lease := taskboard.Lease{
		Token: fmt.Sprintf("tok-%d", s.seq),
		Fence: taskboard.Fence(s.seq),
	}
	if req.TTL > 0 {
		lease.Until = now.Add(req.TTL)
	}
	t.State = taskboard.StateClaimed
	t.Owner = agent
	t.Lease = lease
	t.Seq++
	s.tasks[id] = t
	return t.Clone(), nil
}

// Release 归还认领（凭令牌）：非持有者释放一律被拒，不许静默成功。
func (s *boardStore) Release(_ context.Context, id taskboard.TaskID, token string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	t, ok := s.tasks[id]
	if !ok {
		return errors.Wrap(taskboard.ErrNotFound, "任务不存在", "task", string(id))
	}
	if t.State != taskboard.StateClaimed || t.Lease.Token != token {
		return errors.Wrap(taskboard.ErrLeaseLost, "租约令牌不匹配", "task", string(id))
	}
	t.State = taskboard.StateOpen
	t.Owner = ""
	t.Lease = taskboard.Lease{}
	t.Seq++
	s.tasks[id] = t
	return nil
}

// Complete 标记完成（凭令牌）：终态不带 owner，避免「谁做完的」与「谁还占着」混在一起。
func (s *boardStore) Complete(_ context.Context, id taskboard.TaskID, token string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	t, ok := s.tasks[id]
	if !ok {
		return errors.Wrap(taskboard.ErrNotFound, "任务不存在", "task", string(id))
	}
	if t.State != taskboard.StateClaimed || t.Lease.Token != token {
		return errors.Wrap(taskboard.ErrLeaseLost, "租约令牌不匹配", "task", string(id))
	}
	t.State = taskboard.StateDone
	t.Owner = ""
	t.Lease = taskboard.Lease{}
	t.Seq++
	s.tasks[id] = t
	return nil
}

// Renew 续期并确认持有资格（taskboard.RenewingBoard，issue #87）。
//
// 校验（令牌是当前租约的 + 租约未过期）与延长在同一把锁内完成——原子；
// TTL 语义与 Claim 一致；续期不换令牌、不递增代次（续期不是接管）。
func (s *boardStore) Renew(_ context.Context, id taskboard.TaskID, token string, req taskboard.ClaimRequest) (taskboard.Task, error) {
	now := s.clock() // 锁外取存储时钟（issue #317）
	s.mu.Lock()
	defer s.mu.Unlock()

	t, ok := s.tasks[id]
	if !ok {
		return taskboard.Task{}, errors.Wrap(taskboard.ErrNotFound, "任务不存在", "task", string(id))
	}
	if t.State != taskboard.StateClaimed || t.Lease.Token != token {
		return taskboard.Task{}, errors.Wrap(taskboard.ErrLeaseLost, "租约令牌不匹配", "task", string(id))
	}
	if !taskboard.LeaseHeld(t, now) {
		return taskboard.Task{}, errors.Wrap(taskboard.ErrLeaseLost, "租约已过期，资格终止（请重新认领）", "task", string(id))
	}
	if req.TTL > 0 {
		t.Lease.Until = now.Add(req.TTL)
	} else {
		t.Lease.Until = time.Time{}
	}
	t.Seq++
	s.tasks[id] = t
	return t.Clone(), nil
}
