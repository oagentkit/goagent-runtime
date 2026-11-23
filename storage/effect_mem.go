package storage

import (
	"context"
	"sync"
	"time"

	"github.com/oekit/pkg/errors"

	"github.com/yi-nology/goagent-kernel/sideeffect"
)

// memEffectStore 副作用闸的内存实现。
//
// 判据移植自 goagent-kernel examples/reference-runtime（MIT、同作者），
// 钉住的三条：三项联查（令牌 + 持有者身份 + 资源一致）、执行意图
// （Acquire 原子登记，过期未裁决转未知态）、对账占用（单对账者，过期可重入）。
// Acquire 判定顺序是契约里最要紧的一行：
//
//	已完成 > 结果未知 > 对账中 > 执行中 > 资源被占 > 授予
//
// 顺序写反的代价是重复施加。原子性来自进程内互斥锁——只在单进程内成立。
type memEffectStore struct {
	mu sync.Mutex

	// now 是可注入时钟：过期判定必须读它，否则过期场景恒不触发
	//（kernel sidetest 契约套件的硬要求）。
	now func() time.Time

	acts map[sideeffect.Key]*memActRecord
	ress map[string]*memResRecord
}

type memActRecord struct {
	done      bool
	result    sideeffect.Result
	uncertain bool
	reason    string

	idSet         bool
	idResource    string
	idSafety      sideeffect.RetrySafety
	idPayloadHash string

	intent *memIntentRecord
	recon  *memReconRecord
}

type memIntentRecord struct {
	resource string
	fence    sideeffect.Fence
	holder   string
	expires  time.Time
}

type memResRecord struct {
	fence  sideeffect.Fence
	holder string
	until  time.Time
}

type memReconRecord struct {
	holder  string
	expires time.Time
}

func newMemEffectStore() *memEffectStore {
	return &memEffectStore{
		now:  time.Now,
		acts: map[sideeffect.Key]*memActRecord{},
		ress: map[string]*memResRecord{},
	}
}

// UseClock 注入时钟（契约套件与过期场景测试用）。
func (s *memEffectStore) UseClock(now func() time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if now != nil {
		s.now = now
	}
}

// clock 当前时刻。先取回调、解锁后再调：注入的时钟可能重入本包，
// 持锁调用即死锁。
func (s *memEffectStore) clock() time.Time {
	s.mu.Lock()
	now := s.now
	s.mu.Unlock()
	if now == nil {
		return time.Now()
	}
	return now()
}

// actLocked 取（或创建）Key 的状态记录，取到后立刻登记/比对动作身份——
// 身份判定天然先于任何状态判定（不授予、不重放、不转未知态，优先于已完成重放）。
func (s *memEffectStore) actLocked(act sideeffect.Action) (*memActRecord, error) {
	rec, ok := s.acts[act.Key]
	if !ok {
		rec = &memActRecord{}
		s.acts[act.Key] = rec
	}
	if !rec.idSet {
		rec.idSet = true
		rec.idResource = act.Resource
		rec.idSafety = act.RetrySafety
		rec.idPayloadHash = sideeffect.HashPayload(act.Payload)
		return rec, nil
	}
	if rec.idResource != act.Resource || rec.idSafety != act.RetrySafety ||
		rec.idPayloadHash != sideeffect.HashPayload(act.Payload) {
		return nil, errors.Wrap(sideeffect.ErrActionConflict,
			"同 Key 的动作身份与首次登记不一致（这不是重试，是另一个动作）",
			"key", string(act.Key))
	}
	return rec, nil
}

func (s *memEffectStore) resLocked(resource string) *memResRecord {
	rec, ok := s.ress[resource]
	if !ok {
		rec = &memResRecord{}
		s.ress[resource] = rec
	}
	return rec
}

// Acquire 一次原子判定：已完成 > 结果未知 > 对账中 > 执行中 > 资源被占 > 授予。
func (s *memEffectStore) Acquire(_ context.Context, act sideeffect.Action, ttl time.Duration) (sideeffect.Outcome, sideeffect.Lease, sideeffect.Result, error) {
	now := s.clock()
	s.mu.Lock()
	defer s.mu.Unlock()

	rec, err := s.actLocked(act)
	if err != nil {
		return sideeffect.OutcomeContended, sideeffect.Lease{}, sideeffect.Result{}, err
	}
	if rec.done {
		return sideeffect.OutcomeReplayed, sideeffect.Lease{}, rec.result, nil
	}
	if rec.uncertain {
		return sideeffect.OutcomeUncertain, sideeffect.Lease{}, sideeffect.Result{}, nil
	}
	if rec.recon != nil {
		if now.Before(rec.recon.expires) {
			return sideeffect.OutcomeUncertain, sideeffect.Lease{}, sideeffect.Result{}, nil
		}
		rec.recon = nil
	}
	if rec.intent != nil {
		if now.Before(rec.intent.expires) {
			return sideeffect.OutcomeContended, sideeffect.Lease{}, sideeffect.Result{}, nil
		}
		s.dropIntentLocked(rec)
		if act.RetrySafety != sideeffect.RetrySafetySafe {
			rec.uncertain = true
			rec.reason = "上一次尝试的租约已过期，结果未知"
			return sideeffect.OutcomeUncertain, sideeffect.Lease{}, sideeffect.Result{}, nil
		}
	}

	rr := s.resLocked(act.Resource)
	if rr.holder != "" && rr.until.After(now) {
		return sideeffect.OutcomeContended, sideeffect.Lease{}, sideeffect.Result{}, nil
	}
	rr.fence++
	holder := sideeffect.MintHolder()
	rr.holder = holder
	rr.until = now.Add(ttl)
	rec.intent = &memIntentRecord{resource: act.Resource, fence: rr.fence, holder: holder, expires: now.Add(ttl)}
	return sideeffect.OutcomeApplied,
		sideeffect.Lease{Resource: act.Resource, Fence: rr.fence, Holder: holder, TTL: ttl},
		sideeffect.Result{}, nil
}

// dropIntentLocked 撤销执行意图并作废它自己的资源闸（同一次尝试的两个投影同刻到期）。
func (s *memEffectStore) dropIntentLocked(rec *memActRecord) {
	it := rec.intent
	rec.intent = nil
	if it == nil || it.resource == "" {
		return
	}
	if rr, ok := s.ress[it.resource]; ok && rr.holder == it.holder && rr.fence == it.fence {
		rr.holder = ""
	}
}

// leaseResourceErr 三项联查第一项：租约资源必须与动作资源一致。
func leaseResourceErr(act sideeffect.Action, lease sideeffect.Lease) error {
	if lease.Resource != act.Resource {
		return errors.Wrap(sideeffect.ErrResourceMismatch,
			"凭据被拒：租约与动作的资源不一致",
			"act_resource", act.Resource, "lease_resource", lease.Resource)
	}
	return nil
}

// holdOwnerErr 三项联查：资源闸是否仍由这份凭据持有。
func (s *memEffectStore) holdOwnerErr(act sideeffect.Action, lease sideeffect.Lease) error {
	if err := leaseResourceErr(act, lease); err != nil {
		return err
	}
	rr := s.resLocked(act.Resource)
	if lease.Fence == 0 || rr.holder == "" || rr.fence != lease.Fence {
		return errors.Wrap(sideeffect.ErrFenceStale,
			"凭据被拒：本次持有已失效（令牌不是最新代或资源已释放）",
			"key", string(act.Key), "resource", act.Resource)
	}
	if rr.holder != lease.Holder {
		return errors.Wrap(sideeffect.ErrNotHolder,
			"凭据被拒：持有者身份不匹配（令牌不是本次授予铸造的那份）",
			"key", string(act.Key), "resource", act.Resource)
	}
	return nil
}

// intentOwnerErr 三项联查：本次尝试是否仍归这份凭据所有。
func (s *memEffectStore) intentOwnerErr(act sideeffect.Action, lease sideeffect.Lease) error {
	if err := leaseResourceErr(act, lease); err != nil {
		return err
	}
	rec, err := s.actLocked(act)
	if err != nil {
		return err
	}
	if rec.intent == nil {
		return errors.Wrap(sideeffect.ErrFenceStale,
			"凭据被拒：本次尝试已无未裁决的执行意图", "key", string(act.Key))
	}
	if rec.intent.fence != lease.Fence {
		return errors.Wrap(sideeffect.ErrFenceStale,
			"凭据被拒：本次尝试已不属于该令牌", "key", string(act.Key), "resource", act.Resource)
	}
	if rec.intent.holder != lease.Holder {
		return errors.Wrap(sideeffect.ErrNotHolder,
			"凭据被拒：持有者身份不匹配（意图不是本次授予铸造的那份）",
			"key", string(act.Key), "resource", act.Resource)
	}
	return nil
}

// Complete 标记该键已完成并留存结果。先到者为准。
func (s *memEffectStore) Complete(_ context.Context, act sideeffect.Action, lease sideeffect.Lease, res sideeffect.Result) (sideeffect.Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	rec, err := s.actLocked(act)
	if err != nil {
		return sideeffect.Result{}, err
	}
	if rec.done {
		return rec.result, nil
	}
	if err := s.holdOwnerErr(act, lease); err != nil {
		return sideeffect.Result{}, err
	}
	if err := s.intentOwnerErr(act, lease); err != nil {
		return sideeffect.Result{}, err
	}
	rec.done = true
	rec.result = res
	rec.intent = nil
	rr := s.resLocked(act.Resource)
	rr.holder = ""
	return rec.result, nil
}

// Release 释放资源持有，不动执行意图（意图未裁决前这个键不得再被执行）。
func (s *memEffectStore) Release(_ context.Context, act sideeffect.Action, lease sideeffect.Lease) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.holdOwnerErr(act, lease); err != nil {
		return err
	}
	rr := s.resLocked(act.Resource)
	rr.holder = ""
	return nil
}

// Abandon 显式放弃本次尝试：撤销执行意图并释放资源持有。
func (s *memEffectStore) Abandon(_ context.Context, act sideeffect.Action, lease sideeffect.Lease, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.intentOwnerErr(act, lease); err != nil {
		return err
	}
	rec, err := s.actLocked(act)
	if err != nil {
		return err
	}
	rec.intent = nil
	if err := s.holdOwnerErr(act, lease); err == nil {
		rr := s.resLocked(act.Resource)
		rr.holder = ""
	}
	return nil
}

// Renew 续期并同时确认持有资格：过期即资格终止。
func (s *memEffectStore) Renew(_ context.Context, act sideeffect.Action, lease sideeffect.Lease, ttl time.Duration) (sideeffect.Lease, error) {
	now := s.clock()
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.holdOwnerErr(act, lease); err != nil {
		return sideeffect.Lease{}, err
	}
	if err := s.intentOwnerErr(act, lease); err != nil {
		return sideeffect.Lease{}, err
	}
	rr := s.resLocked(act.Resource)
	if !rr.until.After(now) {
		return sideeffect.Lease{}, errors.Wrap(sideeffect.ErrFenceStale,
			"租约已过期且无人接管——到期即资格终止（要继续做请重新 Acquire）",
			"key", string(act.Key), "resource", act.Resource)
	}
	rr.until = now.Add(ttl)
	rec, err := s.actLocked(act)
	if err != nil {
		return sideeffect.Lease{}, err
	}
	rec.intent.expires = now.Add(ttl)
	return sideeffect.Lease{Resource: act.Resource, Fence: lease.Fence, Holder: lease.Holder, TTL: ttl}, nil
}

// MarkUncertain 把该键标记为「结果未知」。校验 → 改状态在一个临界区内完成：
// 任何一步失败都发生在任何状态被改之前——意图原样留在存储里。
func (s *memEffectStore) MarkUncertain(_ context.Context, act sideeffect.Action, lease sideeffect.Lease, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.holdOwnerErr(act, lease); err != nil {
		return err
	}
	if err := s.intentOwnerErr(act, lease); err != nil {
		return err
	}
	rec, err := s.actLocked(act)
	if err != nil {
		return err
	}
	rec.uncertain = true
	rec.reason = reason
	rec.intent = nil
	return nil
}

// ResolveUncertain 裁决「结果未知」的键。先到者为准。
func (s *memEffectStore) ResolveUncertain(_ context.Context, act sideeffect.Action, res sideeffect.Resolution) (sideeffect.Outcome, sideeffect.Result, error) {
	now := s.clock() // 锁外取时钟：clock 与 store 的锁不可重入
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.resolveLocked(act, res, "", now)
}

// ResolveUncertainOwned 带对账占用凭据的裁决（OwnedResolveStore 契约）：
// holder 不是当前有效对账认领的持有者 ⇒ ErrReconcileLost，不改状态——
// 跨进程长对账的迟到提交必须能被拒。
func (s *memEffectStore) ResolveUncertainOwned(_ context.Context, act sideeffect.Action, res sideeffect.Resolution, holder string) (sideeffect.Outcome, sideeffect.Result, error) {
	if holder == "" {
		return sideeffect.OutcomeContended, sideeffect.Result{}, errors.Wrap(sideeffect.ErrReconcileLost,
			"空凭据不是任何一次对账认领", "key", string(act.Key))
	}
	now := s.clock() // 锁外取时钟
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.resolveLocked(act, res, holder, now)
}

// resolveLocked 裁决主体。requiredHolder 非空时先校验对账占用凭据
// （已完成的重放无害，先于凭据校验——读既有事实不问资格）。
// now 必须由调用方在**加锁前**取好传入：本方法在持锁状态运行，
// 不得再调 s.clock()（Mutex 不可重入，那是自死锁）。
func (s *memEffectStore) resolveLocked(act sideeffect.Action, res sideeffect.Resolution, requiredHolder string, now time.Time) (sideeffect.Outcome, sideeffect.Result, error) {
	rec, err := s.actLocked(act)
	if err != nil {
		return sideeffect.OutcomeContended, sideeffect.Result{}, err
	}
	if rec.done {
		return sideeffect.OutcomeReplayed, rec.result, nil
	}
	if requiredHolder != "" {
		if rec.recon == nil || !now.Before(rec.recon.expires) || rec.recon.holder != requiredHolder {
			return sideeffect.OutcomeContended, sideeffect.Result{}, errors.Wrap(sideeffect.ErrReconcileLost,
				"出示的对账认领不是当前有效的一份（已过期或已被接管）",
				"key", string(act.Key))
		}
	}
	if !rec.uncertain {
		return sideeffect.OutcomeContended, sideeffect.Result{}, sideeffect.ErrNotUncertain
	}
	// 裁决即结论：执行意图与对账占用一并撤销。
	rec.intent = nil
	rec.recon = nil
	if res.Applied {
		rec.done = true
		rec.uncertain = false
		rec.result = res.Result
		return sideeffect.OutcomeReplayed, rec.result, nil
	}
	if act.RetrySafety == sideeffect.RetrySafetyUnsafe && !res.ForceRetryable {
		// 「不可重试」的动作，一句来自对账的「未生效」不足以重新开门：
		// 未知标记原样保留，放行必须显式签字。
		return sideeffect.OutcomeUncertain, sideeffect.Result{}, sideeffect.ErrUnsafeNotRetryable
	}
	rec.uncertain = false
	return sideeffect.OutcomeApplied, sideeffect.Result{}, nil
}

// BeginReconcile 认领对账权：同一 Key 同一时刻至多一个对账者，过期可重入。
func (s *memEffectStore) BeginReconcile(_ context.Context, act sideeffect.Action, ttl time.Duration) (sideeffect.Reconcile, error) {
	if ttl <= 0 {
		return sideeffect.Reconcile{}, errors.Wrap(sideeffect.ErrInvalidTTL,
			"对账认领时长必须为正", "key", string(act.Key))
	}
	now := s.clock()
	s.mu.Lock()
	defer s.mu.Unlock()

	rec, err := s.actLocked(act)
	if err != nil {
		return sideeffect.Reconcile{}, err
	}
	if rec.done || !rec.uncertain {
		return sideeffect.Reconcile{}, sideeffect.ErrNotUncertain
	}
	if rec.recon != nil && now.Before(rec.recon.expires) {
		return sideeffect.Reconcile{}, sideeffect.ErrReconcileTaken
	}
	holder := sideeffect.MintHolder()
	rec.recon = &memReconRecord{holder: holder, expires: now.Add(ttl)}
	return sideeffect.Reconcile{Holder: holder, ExpiresAt: now.Add(ttl)}, nil
}
