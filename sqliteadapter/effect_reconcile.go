// effect_reconcile.go —— 对账侧：ResolveUncertain 与 BeginReconcile。
//
// 裁决**不校验栅栏**（契约硬要求）：它的依据是**下游事实**，不是持有权——
// 原持有者可能已经死亡、租约早已过期，而对账恰恰要在这种情况下进行。
// 占用与裁决的关系：占用过期可重入（对账者死亡不永久占住）；裁决成功即
// 清除占用（这次尝试已有结论）。
//
// 判定口径逐项对位 pgstore（v0.2.0 自 goagent-kernel 迁入）/effect_reconcile.go。
package sqliteadapter

import (
	"context"
	"database/sql"
	"time"

	"github.com/oekit/pkg/errors"

	"github.com/oagentkit/goagent-kernel/sideeffect"
)

// ResolveUncertain 裁决「结果未知」的键。**先到者为准**（简化入口，
// 不校验对账凭据——短对账无须认领，见 OwnedResolveStore）。
//
// RetrySafetyUnsafe 的动作：未生效的裁决不解除阻塞（ErrUnsafeNotRetryable），
// 必须 Resolution.ForceRetryable 的显式决策才放行——「不可重试」是判据，
// 不是注释。
func (s *EffectStore) ResolveUncertain(ctx context.Context, act sideeffect.Action, res sideeffect.Resolution) (sideeffect.Outcome, sideeffect.Result, error) {
	act, err := s.prepareAction(ctx, act)
	if err != nil {
		return sideeffect.OutcomeContended, sideeffect.Result{}, err
	}
	var (
		outcome sideeffect.Outcome
		result  sideeffect.Result
	)
	err = s.exec.run(ctx, func(tx *sql.Tx) error {
		outcome, result, err = s.resolveBody(ctx, tx, act, res, "")
		return err
	})
	if err != nil {
		return sideeffect.OutcomeContended, sideeffect.Result{}, err
	}
	return outcome, result, nil
}

// ResolveUncertainOwned 带对账占用凭据的裁决（OwnedResolveStore）：
// holder 不是**当前有效**对账认领的持有者 → ErrReconcileLost，不改状态。
//
// 校验与裁决同一事务：占用状态被并发接管时，写锁让校验读到的就是裁决要落
// 的那份状态——「校验时还有效、裁决前已被接管」的窗口不存在。
func (s *EffectStore) ResolveUncertainOwned(ctx context.Context, act sideeffect.Action, res sideeffect.Resolution, holder string) (sideeffect.Outcome, sideeffect.Result, error) {
	if holder == "" {
		// 失败登记：拒绝发生在 exec.run 之外 ⇒ 不登记的话调用方吞掉它时
		// 整批仍会提交（「失败即作废整批」对这条路径是假的）。
		err := errors.Wrap(sideeffect.ErrReconcileLost,
			"空凭据不是任何一次对账认领", "key", string(act.Key))
		s.exec.fail(err)
		return sideeffect.OutcomeContended, sideeffect.Result{}, err
	}
	act, err := s.prepareAction(ctx, act)
	if err != nil {
		return sideeffect.OutcomeContended, sideeffect.Result{}, err
	}
	var (
		outcome sideeffect.Outcome
		result  sideeffect.Result
	)
	err = s.exec.run(ctx, func(tx *sql.Tx) error {
		outcome, result, err = s.resolveBody(ctx, tx, act, res, holder)
		return err
	})
	if err != nil {
		return sideeffect.OutcomeContended, sideeffect.Result{}, err
	}
	return outcome, result, nil
}

// resolveBody 裁决主体（事务内、行已锁）。requiredHolder 非空时先校验
// 对账占用凭据；已完成的重放无害，先于凭据校验（读既有事实不问资格）。
func (s *EffectStore) resolveBody(
	ctx context.Context,
	tx *sql.Tx,
	act sideeffect.Action,
	res sideeffect.Resolution,
	requiredHolder string,
) (sideeffect.Outcome, sideeffect.Result, error) {
	if err := ensureActionRow(ctx, tx, act); err != nil {
		return sideeffect.OutcomeContended, sideeffect.Result{}, err
	}
	a, err := lockAction(ctx, tx, act)
	if err != nil {
		return sideeffect.OutcomeContended, sideeffect.Result{}, err
	}
	if a.done {
		return sideeffect.OutcomeReplayed, a.resultValue(), nil
	}
	if requiredHolder != "" {
		// 带凭据的裁决：占用必须存在、未过期、身份匹配（行已锁，判定与
		// 裁决同一份状态——「校验时还有效、裁决前被接管」的窗口不存在）。
		now, err := s.execIn(tx).now(ctx, tx)
		if err != nil {
			return sideeffect.OutcomeContended, sideeffect.Result{}, err
		}
		if !a.hasRecon() || !a.reconHolder.Valid ||
			!now.Before(a.reconExpiresAt()) || a.reconHolder.String != requiredHolder {
			return sideeffect.OutcomeContended, sideeffect.Result{}, errors.Wrap(sideeffect.ErrReconcileLost,
				"出示的对账认领不是当前有效的一份（已过期或已被接管）",
				"key", string(act.Key))
		}
	}
	if !a.uncertain {
		return sideeffect.OutcomeContended, sideeffect.Result{}, sideeffect.ErrNotUncertain
	}
	// 裁决即结论：执行意图与对账占用一并撤销（这次尝试已有结论）。
	a.clearIntent()
	a.clearRecon()
	if res.Applied {
		payload := append([]byte(nil), res.Result.Payload...)
		a.done = true
		a.uncertain = false
		a.result = payload
		a.hasResult = true
		if err := writeAction(ctx, tx, act.Tenant, act.Key, a); err != nil {
			return sideeffect.OutcomeContended, sideeffect.Result{}, err
		}
		return sideeffect.OutcomeReplayed, res.Result, nil
	}
	if act.RetrySafety == sideeffect.RetrySafetyUnsafe && !res.ForceRetryable {
		// 「不可重试」的动作，一句来自对账的「未生效」不足以重新开门：
		// 那句结论来自宿主的一次查询，查询可能出错，查错的代价是重复施加。
		// 未知标记**原样保留**（这个 Key 仍被硬阻塞），放行必须显式签字。
		if err := writeAction(ctx, tx, act.Tenant, act.Key, a); err != nil {
			return sideeffect.OutcomeContended, sideeffect.Result{}, err
		}
		return sideeffect.OutcomeUncertain, sideeffect.Result{}, sideeffect.ErrUnsafeNotRetryable
	}
	a.uncertain = false
	if err := writeAction(ctx, tx, act.Tenant, act.Key, a); err != nil {
		return sideeffect.OutcomeContended, sideeffect.Result{}, err
	}
	return sideeffect.OutcomeApplied, sideeffect.Result{}, nil
}

// BeginReconcile 认领对账权：同一 Key 同一时刻至多一个对账者。
//
// 仅当该 Key 处于未知态时接受认领；占用未过期时拒绝（ErrReconcileTaken）；
// 占用过期可重新认领（可重入）——对账者死亡不永久占住这个 Key。
func (s *EffectStore) BeginReconcile(ctx context.Context, act sideeffect.Action, ttl time.Duration) (sideeffect.Reconcile, error) {
	if ttl <= 0 {
		// 失败登记：拒绝发生在 exec.run 之外（见上）。
		err := errors.Wrap(sideeffect.ErrInvalidTTL,
			"对账认领时长必须为正（零/负 TTL 造出出生即过期的认领）",
			"key", string(act.Key))
		s.exec.fail(err)
		return sideeffect.Reconcile{}, err
	}
	act, err := s.prepareAction(ctx, act)
	if err != nil {
		return sideeffect.Reconcile{}, err
	}
	var out sideeffect.Reconcile
	err = s.exec.run(ctx, func(tx *sql.Tx) error {
		if err := ensureActionRow(ctx, tx, act); err != nil {
			return err
		}
		a, err := lockAction(ctx, tx, act)
		if err != nil {
			return err
		}
		// 认领时刻：拿到写锁之后取（同 Acquire）。
		now, err := s.execIn(tx).now(ctx, tx)
		if err != nil {
			return err
		}
		if a.done || !a.uncertain {
			return sideeffect.ErrNotUncertain
		}
		if a.hasRecon() && now.Before(a.reconExpiresAt()) {
			return sideeffect.ErrReconcileTaken
		}
		holder := sideeffect.MintHolder()
		expires := now.Add(ttl)
		a.reconHolder = sql.NullString{String: holder, Valid: true}
		a.reconExpires = nullInt64Value(expires.UnixMilli())
		if err := writeAction(ctx, tx, act.Tenant, act.Key, a); err != nil {
			return err
		}
		out = sideeffect.Reconcile{Holder: holder, ExpiresAt: expires}
		return nil
	})
	if err != nil {
		return sideeffect.Reconcile{}, err
	}
	return out, nil
}
