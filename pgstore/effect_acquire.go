// effect_acquire.go —— Acquire：三判合一的一个事务。
//
// 判定顺序是契约里最要紧的一行：**已完成 > 结果未知 > 执行中 > 资源被占 > 授予**。
// 顺序写反的代价是重复施加：把「结果未知」排到「资源被占」之后，调用方会拿到
// contended（以为「晚点重试就行」），于是重试发生、重复施加随之发生。
//
// 为什么整段在一个事务里：三判合一的每一步都读同一份状态，行锁保证第二个
// 进程要么读到判定前的旧状态（然后在锁上等），要么读到判定后的新状态——
// 不存在「两个进程同时读到没做过、资源空闲」的窗口。
package pgstore

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/oekit/pkg/errors"

	"github.com/yi-nology/goagent-kernel/sideeffect"
)

// intentCompleteErr 意图四列齐全性守卫（Acquire 判定路径的解引用前置）。
//
// lockAction 的 checkIntentColumns 已经在所有入口前拒掉半写行；这里再守一次
// 是因为 Acquire 随后要解引用 intentExpires / intentResource / intentFence——
// 守卫与解引用放在同一段里，防止将来新增入口绕过 lockAction 直接判。
func (r *actionRow) intentCompleteErr(key sideeffect.Key) error {
	if r.intentColumnsFilled() == 4 {
		return nil
	}
	return errors.Wrap(ErrIntentIncomplete,
		"pgstore: 动作行的执行意图残缺（四列应同时为 NULL 或同时有值）",
		"key", string(key))
}

// Acquire 一次原子判定（契约语义逐条对照 sideeffect.Store 的文档）。
//
// 授予时在同一事务里做三件事：令牌 = 资源已见最大值 + 1；持有者身份由
// sideeffect.MintHolder() 在**同一事务**里铸造；执行意图（本次令牌 + 到期
// 时刻）随授予一并落库。三者分开写就等于没写：中间死掉 ⇒ 意图悬空或没写，
// 重复施加的入口重新打开。
func (s *EffectStore) Acquire(ctx context.Context, act sideeffect.Action, ttl time.Duration) (sideeffect.Outcome, sideeffect.Lease, sideeffect.Result, error) {
	act, err := s.prepareAction(ctx, act)
	if err != nil {
		return sideeffect.OutcomeContended, sideeffect.Lease{}, sideeffect.Result{}, err
	}
	return withinTx3(ctx, s.pool, func(tx pgx.Tx) (sideeffect.Outcome, sideeffect.Lease, sideeffect.Result, error) {
		if err := ensureResourceRow(ctx, tx, act.Tenant, act.Resource); err != nil {
			return sideeffect.OutcomeContended, sideeffect.Lease{}, sideeffect.Result{}, err
		}
		if err := ensureActionRow(ctx, tx, act); err != nil {
			return sideeffect.OutcomeContended, sideeffect.Lease{}, sideeffect.Result{}, err
		}
		rr, err := lockResource(ctx, tx, act.Tenant, act.Resource)
		if err != nil {
			return sideeffect.OutcomeContended, sideeffect.Lease{}, sideeffect.Result{}, err
		}
		a, err := lockAction(ctx, tx, act)
		if err != nil {
			return sideeffect.OutcomeContended, sideeffect.Lease{}, sideeffect.Result{}, err
		}
		// 判定时刻：**行锁之后**取（注入时钟或数据库 clock_timestamp）——
		// 事务在锁上等的时间不计入租约寿命，跨节点判定读同一个权威时钟。
		now, err := s.nowIn(ctx, tx)
		if err != nil {
			return sideeffect.OutcomeContended, sideeffect.Lease{}, sideeffect.Result{}, err
		}

		// 1. 已完成 → 重放既有结果（不再需要令牌）。
		if a.done {
			var res sideeffect.Result
			if a.result != nil {
				res.Payload = append([]byte(nil), *a.result...)
			}
			return sideeffect.OutcomeReplayed, sideeffect.Lease{}, res, nil
		}

		// 2. 结果未知 → 硬阻塞（先对账，不是等一等）。
		if a.uncertain {
			return sideeffect.OutcomeUncertain, sideeffect.Lease{}, sideeffect.Result{}, nil
		}

		// 2b. 对账占用未过期 ⇒ 对账中：仍属未知态一族。过期的占用在这里清掉
		// （可重入：对账者死亡不永久占住这个 Key）。
		if a.hasRecon() {
			if now.Before(*a.reconExpires) {
				return sideeffect.OutcomeUncertain, sideeffect.Lease{}, sideeffect.Result{}, nil
			}
			a.reconHolder, a.reconExpires = nil, nil
			if err := writeAction(ctx, tx, act.Tenant, act.Key, a); err != nil {
				return sideeffect.OutcomeContended, sideeffect.Lease{}, sideeffect.Result{}, err
			}
		}

		// 3. 执行中：这次尝试还没裁决 ⇒ 不得重新授予执行权。
		// hasIntent 要求四列齐全；半写行已由 lockAction 的 checkIntentColumns
		// 拒绝（ErrIntentIncomplete），不会走到下面解引用 NULL 的路径。
		if a.hasIntent() {
			// 部分填充守卫（fail-closed）：判定路径要解引用 intentExpires /
			// intentResource / intentFence，残缺行在这里显式报错、事务整体回滚，
			// 而不是 panic（行锁持有、事务开着的路径上 panic 直接杀宿主进程），
			// 更不是当「没有在途尝试」放行。
			if err := a.intentCompleteErr(act.Key); err != nil {
				return sideeffect.OutcomeContended, sideeffect.Lease{}, sideeffect.Result{}, err
			}
			if now.Before(*a.intentExpires) {
				return sideeffect.OutcomeContended, sideeffect.Lease{}, sideeffect.Result{}, nil
			}
			// 租约过期 ⇒ 这次尝试的结果未知（可能已施加、账没记上）。
			// 意图与资源持有是同一次尝试的两个投影，同刻到期：作废意图时，
			// 若资源闸仍是这次尝试的那份，一并作废（没人接管时令牌没变，
			// 不清的话「尝试已转入未知态」在资源侧看不出来）。
			irResource, irHolder, irFence := *a.intentResource, *a.intentHolder, *a.intentFence
			a.intentResource, a.intentFence, a.intentHolder, a.intentExpires = nil, nil, nil, nil
			if irResource == act.Resource && rr.holder != nil && *rr.holder == irHolder && rr.fence == irFence {
				rr.holder = nil
				if err := writeResource(ctx, tx, act.Tenant, act.Resource, rr); err != nil {
					return sideeffect.OutcomeContended, sideeffect.Lease{}, sideeffect.Result{}, err
				}
			}
			if act.RetrySafety != sideeffect.RetrySafetySafe {
				a.uncertain = true
				a.reason = "上一次尝试的租约已过期，结果未知"
				if err := writeAction(ctx, tx, act.Tenant, act.Key, a); err != nil {
					return sideeffect.OutcomeContended, sideeffect.Lease{}, sideeffect.Result{}, err
				}
				return sideeffect.OutcomeUncertain, sideeffect.Lease{}, sideeffect.Result{}, nil
			}
		}

		// 4. 资源被他人持有且租约未过期 → 可重试的拒绝。
		if rr.held(now) {
			return sideeffect.OutcomeContended, sideeffect.Lease{}, sideeffect.Result{}, nil
		}

		// 5. 授予：令牌单调递增 + 铸造持有者身份 + 登记执行意图，同一事务。
		rr.fence++
		holder := sideeffect.MintHolder()
		rr.holder = &holder
		until := now.Add(ttl)
		rr.until = &until
		if err := writeResource(ctx, tx, act.Tenant, act.Resource, rr); err != nil {
			return sideeffect.OutcomeContended, sideeffect.Lease{}, sideeffect.Result{}, err
		}
		intentResource := act.Resource
		intentFence := rr.fence
		a.intentResource = &intentResource
		a.intentFence = &intentFence
		a.intentHolder = &holder
		a.intentExpires = &until
		if err := writeAction(ctx, tx, act.Tenant, act.Key, a); err != nil {
			return sideeffect.OutcomeContended, sideeffect.Lease{}, sideeffect.Result{}, err
		}
		return sideeffect.OutcomeApplied,
			sideeffect.Lease{Resource: act.Resource, Fence: sideeffect.Fence(rr.fence), Holder: holder, TTL: ttl},
			sideeffect.Result{}, nil
	})
}

// writeResource 把资源级状态写回（调用方持行锁，无条件写即可）。
func writeResource(ctx context.Context, tx pgx.Tx, tenant, resource string, rr *resourceRow) error {
	_, err := tx.Exec(ctx,
		`UPDATE pgref_effect_resource SET fence = $3, holder = $4, until = $5
		  WHERE tenant = $1 AND resource = $2`,
		tenant, resource, rr.fence, rr.holder, rr.until)
	if err != nil {
		return errors.Wrap(err, "pgstore: 写资源行失败", "resource", resource)
	}
	return nil
}

// writeAction 把 Key 级状态写回（调用方持行锁，无条件写即可）。
func writeAction(ctx context.Context, tx pgx.Tx, tenant string, key sideeffect.Key, a *actionRow) error {
	_, err := tx.Exec(ctx,
		`UPDATE pgref_effect_action
		    SET done = $3, result = $4, uncertain = $5, reason = $6,
		        intent_resource = $7, intent_fence = $8, intent_holder = $9, intent_expires = $10,
		        recon_holder = $11, recon_expires = $12
		  WHERE tenant = $1 AND key = $2`,
		tenant, string(key), a.done, a.result, a.uncertain, a.reason,
		a.intentResource, a.intentFence, a.intentHolder, a.intentExpires,
		a.reconHolder, a.reconExpires)
	if err != nil {
		return errors.Wrap(err, "pgstore: 写动作行失败", "key", string(key))
	}
	return nil
}
