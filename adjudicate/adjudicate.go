// Package adjudicate —— 副作用未知态的周期裁决框架（yi-nology/goagent-runtime#1）。
//
// kernel 判据把「结果未知」的 Key 硬阻塞（再动即重复施加），pipeline 把
// 执行结果分类出 KindUncertain——但 **uncertain 之后没有下文**：已生效
// 还是未生效？盲目重试可能二次施加，不重试可能永久漏。本包把「证据回查
// → 三态判定 → 落裁决」变成机制：
//
//	Source（枚举未知态 Key，宿主按自己的存储实现——kernel Store 契约
//	只有 Mark/Resolve 没有枚举，枚举是宿主存储面的事）
//	  → grace 静置窗（给施加方「失败→标未知→释放」窗口，避免与在途动作抢判）
//	  → Resolver 链（Router 按声明顺序分发：第一个「认识这个 Key」的裁决
//	    生效；error 透传 = 判据不足本轮不裁决；全不认识 = 跳过留痕，绝不猜）
//	  → sideeffect.Runner.Reconcile 落裁决（已生效记完成重放 / 未生效
//	    RetrySafetySafe 释放重做 / RetrySafetyUnsafe 须 ForceRetryable
//	    签字——签字语义归 kernel）
//	  → 单行失败不阻断其余行，错误汇总返回。
//
// 领域裁决器（认识什么 Key、查什么下游证据）全注入：本包零业务词。
// 周期调度不在此处起 goroutine——由宿主 bootstrap 的 ticker/worker 管理，
// Loop.ReconcileOnce 是无副作用单轮（可手动触发可周期触发）。
// 生命周期: Experimental（v0.9.0 定级基线；ADR-004 波次接入 api-lifecycle 清单）
package adjudicate

import (
	"context"
	"time"

	"github.com/oekit/pkg/errors"

	"github.com/yi-nology/goagent-kernel/sideeffect"
)

// 缺省对账参数（提出方 argus 生产调参实证）。
const (
	// DefaultGrace 未知态 Key 的最短静置时间：给施加方留出标记/释放窗口。
	DefaultGrace = 5 * time.Minute
	// DefaultBatch 单轮最多处理的 Key 数。
	DefaultBatch = 50
)

// Item 一条未知态副作用（枚举面元素）。Action 是 kernel 动作身份
// （Key/Resource/Payload/RetrySafety——裁决落库时须与登记身份一致，
// 不一致由 kernel ErrActionConflict 拦）。
type Item struct {
	Action   sideeffect.Action
	Reason   string    // 标未知时的原因（留痕/裁决日志用）
	MarkedAt time.Time // 标未知时刻（grace 静置窗的判据）
}

// Source 枚举未知态 Key（宿主实现，过自己的存储）。olderThan 之前标记的、
// 最多 limit 条；无候选返回空切片（nil error）。
type Source interface {
	ListUncertain(ctx context.Context, olderThan time.Time, limit int) ([]Item, error)
}

// Resolver 把一条未知态 Key 判定为裁决（或声明不认识这个 Key）。
// 语义（三态，对应提出方生产裁决器的判定边界）：
//
//   - ok=true → 返回的 Resolution 生效（Applied=true 记完成可重放 /
//     Applied=false 按 RetrySafety 走 kernel 释放语义）；
//   - ok=false → 不认识（该 Key 类型不受支持），跳过不改状态——未知 Key
//     留痕等扩展，绝不猜；
//   - err != nil → 认识但**判据不足**（下游查询失败等）——本轮不裁决，
//     留给下一轮。判据不足 ≠ 未生效：把「查不到」当「没生效」放行重试
//     就是二次施加的入口。
type Resolver interface {
	Resolve(ctx context.Context, item Item) (sideeffect.Resolution, bool, error)
}

// Router 按声明顺序试一组 resolver，第一个「认识这个 Key」的裁决生效。
// 某 resolver 返回 error（认识但裁决不了）→ 立即透传（判据不足不裁决）；
// 全部 ok=false（无人认识）→ (空, false, nil)。
type Router struct {
	resolvers []Resolver
}

// NewRouter 组装路由裁决器（nil 项忽略）。
func NewRouter(rs ...Resolver) *Router {
	out := make([]Resolver, 0, len(rs))
	for _, r := range rs {
		if r != nil {
			out = append(out, r)
		}
	}
	return &Router{resolvers: out}
}

// Resolve 按声明顺序分发（见类型注释）。
func (r *Router) Resolve(ctx context.Context, item Item) (sideeffect.Resolution, bool, error) {
	if r == nil {
		return sideeffect.Resolution{}, false, nil
	}
	for _, res := range r.resolvers {
		out, ok, err := res.Resolve(ctx, item)
		if err != nil || ok {
			return out, ok, err
		}
	}
	return sideeffect.Resolution{}, false, nil
}

// Stats 单轮对账结果面（宿主观测/告警用：uncertain 积压、裁决分布）。
type Stats struct {
	Scanned  int // 枚举到的候选数
	Settled  int // 成功落裁决数（已生效记完成 + 未生效释放）
	Skipped  int // 无人认识的 Key 数（类型不受支持）+ 并发已裁决数
	Deferred int // 判据不足本轮未裁决数
	NotDue   int // 未到静置窗被 Source 滤掉的计数（Source 实现可选返回）
	Failures int // 落裁决失败数（错误已汇总）
}

// 逐项观测事件词表（Observer 消费）。
const (
	// EventSettled 落裁决成功（Resolution/Outcome 有效）。
	EventSettled = "settled"
	// EventSkipped 无人认识（Err=nil）或并发已裁决（Err=ErrNotUncertain）。
	EventSkipped = "skipped"
	// EventDeferred resolver 认识但判据不足（Err=其错误），本轮不裁决。
	EventDeferred = "deferred"
	// EventFailed 落库失败（Err=其错误）。
	EventFailed = "failed"
)

// Observation 一次逐项结算的观测面（Observer 输入）。
type Observation struct {
	Item       Item
	Event      string
	Resolution sideeffect.Resolution // EventSettled 时有效
	Outcome    sideeffect.Outcome    // EventSettled 时有效（落库返回）
	Err        error                 // 非 Settled：Skipped 仅并发已裁决带 sentinel；Deferred/Failed 为对应错误
}

// Observer 逐项观测钩子：宿主接自己的 obs/日志面（观测分布、告警、结构化
// 日志），Loop 自身保持零日志零指标（纯库纪律）。每项结算后同步调用；
// 钩子应是纯观测（不改判据、不 panic——panic 会中断本轮对账）。
type Observer func(ctx context.Context, o Observation)

// Loop 对账循环（无 goroutine：ReconcileOnce 是单轮，周期由宿主驱动）。
type Loop struct {
	source   Source
	resolver Resolver
	runner   *sideeffect.Runner
	observer Observer
	grace    time.Duration
	batch    int
}

// LoopOption 装配选项。
type LoopOption func(*Loop)

// WithGrace 未知态静置窗（默认 DefaultGrace；显式 0 = 不等待，测试/即时
// 对账形态）。须覆盖施加方「失败→标未知→释放」的最长窗口，否则对账会与
// 在途动作抢判；负值忽略（保持缺省）。
func WithGrace(d time.Duration) LoopOption {
	return func(l *Loop) {
		if d >= 0 {
			l.grace = d
		}
	}
}

// WithBatch 单轮上限（默认 DefaultBatch）。
func WithBatch(n int) LoopOption {
	return func(l *Loop) {
		if n > 0 {
			l.batch = n
		}
	}
}

// WithObserver 逐项观测钩子（nil = 不观测）。
func WithObserver(o Observer) LoopOption {
	return func(l *Loop) {
		if o != nil {
			l.observer = o
		}
	}
}

// NewLoop 装配对账循环（装配期 fail-fast：缺件即报错）。
func NewLoop(source Source, resolver Resolver, runner *sideeffect.Runner, opts ...LoopOption) (*Loop, error) {
	if source == nil {
		return nil, errors.New("adjudicate: 缺 Source（枚举未知态 Key 的宿主存储面）")
	}
	if resolver == nil {
		return nil, errors.New("adjudicate: 缺 Resolver（裁决分发面）")
	}
	if runner == nil {
		return nil, errors.New("adjudicate: 缺 sideeffect.Runner（裁决落库面）")
	}
	l := &Loop{source: source, resolver: resolver, runner: runner, grace: DefaultGrace, batch: DefaultBatch}
	for _, opt := range opts {
		if opt != nil {
			opt(l)
		}
	}
	return l, nil
}

// ReconcileOnce 执行一轮对账：枚举静置窗外的未知态 Key → 路由裁决 →
// 落库结算。单行失败不阻断其余行；错误 errors.Join 汇总返回（Stats
// 照样如实计数）。
func (l *Loop) ReconcileOnce(ctx context.Context) (Stats, error) {
	var st Stats
	items, err := l.source.ListUncertain(ctx, time.Now().Add(-l.grace), l.batch)
	if err != nil {
		return st, errors.Wrap(err, "adjudicate: 枚举未知态 Key 失败")
	}
	st.Scanned = len(items)
	var errs []error
	for _, item := range items {
		if cerr := ctx.Err(); cerr != nil {
			return st, errors.Wrap(cerr, "adjudicate: 上下文取消，本轮中止", "settled", st.Settled, "scanned", st.Scanned)
		}
		res, ok, rerr := l.resolver.Resolve(ctx, item)
		if rerr != nil {
			st.Deferred++
			l.observe(ctx, Observation{Item: item, Event: EventDeferred, Err: rerr})
			errs = append(errs, errors.Wrap(rerr, "adjudicate: 裁决判据不足", "key", string(item.Action.Key)))
			continue
		}
		if !ok {
			st.Skipped++ // 无人认识：留痕等扩展，绝不猜
			l.observe(ctx, Observation{Item: item, Event: EventSkipped})
			continue
		}
		outcome, _, serr := l.runner.Reconcile(ctx, item.Action, res)
		if serr != nil {
			if errors.Is(serr, sideeffect.ErrNotUncertain) {
				// 并发：另一对账者/人工先落结论——不是故障，按 skipped 计
				//（否则每个可重试 Key 每周期都刷失败告警）。
				st.Skipped++
				l.observe(ctx, Observation{Item: item, Event: EventSkipped, Err: serr})
				continue
			}
			st.Failures++
			l.observe(ctx, Observation{Item: item, Event: EventFailed, Err: serr})
			errs = append(errs, errors.Wrap(serr, "adjudicate: 裁决落库失败", "key", string(item.Action.Key)))
			continue
		}
		st.Settled++
		l.observe(ctx, Observation{Item: item, Event: EventSettled, Resolution: res, Outcome: outcome})
	}
	// 错误面：Stats 已逐类计数（Deferred/Failures），返回值带首个错误 +
	// 总数——pkg/errors 无 Join 且本仓不混 stdlib errors（AGENTS 红线）。
	if len(errs) > 0 {
		return st, errors.Wrap(errs[0], "adjudicate: 本轮对账有未决错误", "total", len(errs))
	}
	return st, nil
}

// observe 调用观测钩子（nil 钩子静默跳过）。
func (l *Loop) observe(ctx context.Context, o Observation) {
	if l.observer != nil {
		l.observer(ctx, o)
	}
}
