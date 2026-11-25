// effect_lease.go —— 凭据一族：Complete / Release / Abandon / Renew / MarkUncertain。
//
// 五个方法共用同一套纪律：
//   - 校验与写入在**同一个事务**里 ⇒ 校验失败即整体回滚，存储侧不剩半截状态；
//   - 资源行 → 动作行的固定锁序（见 effect.go）；
//   - 三项联查不过即拒（ErrFenceStale / ErrNotHolder / ErrResourceMismatch），
//     拒绝时**不落任何一行**。
//
// 判定口径逐项对位 pgstore（v0.2.0 自 goagent-kernel 迁入）/effect_lease.go（同一套契约、同一套判据）。
package sqliteadapter

import (
	"context"
	"database/sql"
	"time"

	"github.com/oekit/pkg/errors"

	"github.com/oagentkit/goagent-kernel/sideeffect"
)

// leaseResourceErr 三项联查的第一项：租约的资源必须与动作的资源一致。
// （A 资源的租约被拿去做 B 资源的操作时，两份真相拼出一个错误的「合规」。）
func leaseResourceErr(act sideeffect.Action, lease sideeffect.Lease) error {
	if lease.Resource != act.Resource {
		return errors.Wrap(sideeffect.ErrResourceMismatch,
			"凭据被拒：租约与动作的资源不一致",
			"act_resource", act.Resource, "lease_resource", lease.Resource)
	}
	return nil
}

// holdOwnerErr 三项联查：资源闸是否仍由**这份凭据**持有。
//
// 判定顺序与 PG 同款：令牌不匹配或已释放 → ErrFenceStale；令牌还在这一代
// 但持有者不是出示者 → ErrNotHolder。
func holdOwnerErr(act sideeffect.Action, lease sideeffect.Lease, rr *resourceRow) error {
	if err := leaseResourceErr(act, lease); err != nil {
		return err
	}
	if lease.Fence == 0 || !rr.holder.Valid || rr.fence != int64(lease.Fence) {
		return errors.Wrap(sideeffect.ErrFenceStale,
			"凭据被拒：本次持有已失效（令牌不是最新代或资源已释放）",
			"key", string(act.Key), "resource", act.Resource)
	}
	if rr.holder.String != lease.Holder {
		return errors.Wrap(sideeffect.ErrNotHolder,
			"凭据被拒：持有者身份不匹配（令牌不是本次授予铸造的那份）",
			"key", string(act.Key), "resource", act.Resource)
	}
	return nil
}

// intentOwnerErr 三项联查：本次尝试是否仍归**这份凭据**所有。
func intentOwnerErr(act sideeffect.Action, lease sideeffect.Lease, a *actionRow) error {
	if err := leaseResourceErr(act, lease); err != nil {
		return err
	}
	if !a.hasIntent() {
		return errors.Wrap(sideeffect.ErrFenceStale,
			"凭据被拒：本次尝试已无未裁决的执行意图",
			"key", string(act.Key))
	}
	if a.intentFence.Int64 != int64(lease.Fence) {
		return errors.Wrap(sideeffect.ErrFenceStale,
			"凭据被拒：本次尝试已不属于该令牌",
			"key", string(act.Key), "resource", act.Resource)
	}
	if !a.intentHolder.Valid || a.intentHolder.String != lease.Holder {
		return errors.Wrap(sideeffect.ErrNotHolder,
			"凭据被拒：持有者身份不匹配（意图不是本次授予铸造的那份）",
			"key", string(act.Key))
	}
	return nil
}

// lockPair 一次事务的行读取：资源行在前、动作行在后（锁序恒定）。
func lockPair(ctx context.Context, tx *sql.Tx, act sideeffect.Action) (*resourceRow, *actionRow, error) {
	if err := ensureResourceRow(ctx, tx, act.Tenant, act.Resource); err != nil {
		return nil, nil, err
	}
	if err := ensureActionRow(ctx, tx, act); err != nil {
		return nil, nil, err
	}
	rr, err := lockResource(ctx, tx, act.Tenant, act.Resource)
	if err != nil {
		return nil, nil, err
	}
	a, err := lockAction(ctx, tx, act)
	if err != nil {
		return nil, nil, err
	}
	return rr, a, nil
}

// Complete 标记该键的动作已完成并留存结果。**先到者为准**。
//
// 幂等：同一 Key 重复 Complete 不报错，返回首次记录的结果（后到者不覆盖）。
func (s *EffectStore) Complete(ctx context.Context, act sideeffect.Action, lease sideeffect.Lease, res sideeffect.Result) (sideeffect.Result, error) {
	act, err := s.prepareAction(ctx, act)
	if err != nil {
		return sideeffect.Result{}, err
	}
	var out sideeffect.Result
	err = s.exec.run(ctx, func(tx *sql.Tx) error {
		rr, a, err := lockPair(ctx, tx, act)
		if err != nil {
			return err
		}
		if a.done {
			// 幂等：先到者的结果不被后到者覆盖（此检查在凭据校验之前，
			// 与内存版一致——已完成的事实不因出示者过期而不可见）。
			out = a.resultValue()
			return nil
		}
		if err := holdOwnerErr(act, lease, rr); err != nil {
			return err
		}
		if err := intentOwnerErr(act, lease, a); err != nil {
			return err
		}
		payload := append([]byte(nil), res.Payload...)
		a.done = true
		a.result = payload
		a.hasResult = true
		a.clearIntent()
		if err := writeAction(ctx, tx, act.Tenant, act.Key, a); err != nil {
			return err
		}
		// 动作已完成 ⇒ 资源闸释放（否则后来者会被「资源正忙」挡住）。
		rr.holder = nullStringValue()
		if err := writeResource(ctx, tx, act.Tenant, act.Resource, rr); err != nil {
			return err
		}
		out = res
		return nil
	})
	if err != nil {
		return sideeffect.Result{}, err
	}
	return out, nil
}

// Release 释放**资源持有**，**不动**该键的执行意图。
//
// 意图没被裁决之前，这个键不得再被执行——否则「施加成功但进程死在记账
// 之前」会被下一次 Acquire 变成重复施加。两条出口（Abandon / Release）
// 不可互相替代。
func (s *EffectStore) Release(ctx context.Context, act sideeffect.Action, lease sideeffect.Lease) error {
	act, err := s.prepareAction(ctx, act)
	if err != nil {
		return err
	}
	return s.exec.run(ctx, func(tx *sql.Tx) error {
		rr, _, err := lockPair(ctx, tx, act)
		if err != nil {
			return err
		}
		if err := holdOwnerErr(act, lease, rr); err != nil {
			return err
		}
		rr.holder = nullStringValue()
		return writeResource(ctx, tx, act.Tenant, act.Resource, rr)
	})
}

// Abandon 显式放弃本次尝试：撤销执行意图，并释放（仍是自己的）资源持有。
//
// 资源闸只在仍是自己的时才放——易主了就不碰（那是新持有者的闸）。
func (s *EffectStore) Abandon(ctx context.Context, act sideeffect.Action, lease sideeffect.Lease, _ string) error {
	act, err := s.prepareAction(ctx, act)
	if err != nil {
		return err
	}
	return s.exec.run(ctx, func(tx *sql.Tx) error {
		rr, a, err := lockPair(ctx, tx, act)
		if err != nil {
			return err
		}
		if err := intentOwnerErr(act, lease, a); err != nil {
			return err
		}
		a.clearIntent()
		if err := writeAction(ctx, tx, act.Tenant, act.Key, a); err != nil {
			return err
		}
		if holdOwnerErr(act, lease, rr) == nil {
			rr.holder = nullStringValue()
			if err := writeResource(ctx, tx, act.Tenant, act.Resource, rr); err != nil {
				return err
			}
		}
		return nil
	})
}

// Renew 续期并**同时确认持有资格**：闸已易主、尝试已失效**或租约已过期**
// 则拒绝续期（假续期是重复施加的入口）。
//
// 过期即资格终止：租约已过期而无人接管时，旧持有者不得复活它。
func (s *EffectStore) Renew(ctx context.Context, act sideeffect.Action, lease sideeffect.Lease, ttl time.Duration) (sideeffect.Lease, error) {
	act, err := s.prepareAction(ctx, act)
	if err != nil {
		return sideeffect.Lease{}, err
	}
	var out sideeffect.Lease
	err = s.exec.run(ctx, func(tx *sql.Tx) error {
		rr, a, err := lockPair(ctx, tx, act)
		if err != nil {
			return err
		}
		if err := holdOwnerErr(act, lease, rr); err != nil {
			return err
		}
		if err := intentOwnerErr(act, lease, a); err != nil {
			return err
		}
		// 过期即资格终止：租约已过期而无人接管时，旧持有者不得复活它。
		// 判定读**拿到写锁之后**的当前时刻，拿锁期间的时间流逝不被计入
		// 租约寿命；过期的尝试本该由 Acquire 转入未知态先对账，续期放行
		// 就绕开了那条判据（Store.Renew 契约「过期即资格终止」）。
		now, err := s.execIn(tx).now(ctx, tx)
		if err != nil {
			return err
		}
		if !rr.until.Valid || !time.UnixMilli(rr.until.Int64).UTC().After(now) {
			return errors.Wrap(sideeffect.ErrFenceStale,
				"租约已过期且无人接管——到期即资格终止（要继续做请重新 Acquire）",
				"key", string(act.Key), "resource", act.Resource)
		}
		until := now.Add(ttl)
		rr.until = nullInt64Value(until.UnixMilli())
		if err := writeResource(ctx, tx, act.Tenant, act.Resource, rr); err != nil {
			return err
		}
		a.intentExpires = nullInt64Value(until.UnixMilli())
		if err := writeAction(ctx, tx, act.Tenant, act.Key, a); err != nil {
			return err
		}
		out = sideeffect.Lease{Resource: lease.Resource, Fence: lease.Fence, Holder: lease.Holder, TTL: ttl}
		return nil
	})
	if err != nil {
		return sideeffect.Lease{}, err
	}
	return out, nil
}

// MarkUncertain 把该键标记为「结果未知」（按键，与资源租约无关）。
//
// # 标记写失败必须保住执行意图（契约硬要求第 5 条）
//
// 本实现「校验 → 改状态」在一个事务里完成：任何一步失败都发生在**任何状态
// 被改之前**，整体回滚 ⇒ 意图原样留在存储里，后续 Acquire 被意图挡住。
// 这不是实现自由：落到数据库上时，标记与撤销意图**不得**「先撤意图、再标
// 未知」分两步提交——第二步失败，存储侧就什么都不剩，另一个 worker 直接重放。
//
// 标记成功后意图一并撤销（这次尝试已有结论：未知），但**闸不自动释放**
// ——调用方还要显式调 Release 只放资源。
func (s *EffectStore) MarkUncertain(ctx context.Context, act sideeffect.Action, lease sideeffect.Lease, reason string) error {
	act, err := s.prepareAction(ctx, act)
	if err != nil {
		return err
	}
	return s.exec.run(ctx, func(tx *sql.Tx) error {
		rr, a, err := lockPair(ctx, tx, act)
		if err != nil {
			return err
		}
		if err := holdOwnerErr(act, lease, rr); err != nil {
			return err
		}
		if err := intentOwnerErr(act, lease, a); err != nil {
			return err
		}
		a.uncertain = true
		a.reason = reason
		a.clearIntent()
		return writeAction(ctx, tx, act.Tenant, act.Key, a)
	})
}
