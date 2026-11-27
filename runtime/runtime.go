// Package runtime —— 面向业务开发者的**运行时装配层**。
//
// # 定位
//
// goagent-kernel 给确定性机制（判据单源、包级散件），本仓把它们装配成
// 「一个普通 Go 团队不读几十个包也能用」的运行时。装配语义：
//
//	宿主产品          只写领域：系统提示、工具实现、模型供应商适配
//	goagent-runtime   本层：Loop/Pipeline/存储/事件 的接线与默认口径
//	goagent-kernel    判据：状态/幂等/栅栏/预算/恢复
//
// 向下只依赖 kernel 契约，不向上依赖任何宿主——本层不含领域概念，
// 工具与提示词全是宿主注入的。
//
// # 模板
//
//   - NewEmbedded：内存存储，单进程，零外部依赖（测试/内嵌场景）；
//   - NewStandalone：SQLite 单文件持久化（本机服务场景）；
//   - cluster（PG + worker 池，另见本仓 cluster 包）：多副本形态——多个副本
//     共享同一个 PG；同一会话集群内有且只有一个副本执行（认领 + 租约 +
//     接管代次，走 kernel taskboard 判据）；副本崩了别的副本接管重跑，已
//     施加的副作用不重复施加（前提是宿主声明业务幂等键，见
//     pipeline.Tool.IdempotencyKey）。
package runtime

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/oekit/pkg/errors"

	"github.com/oagentkit/agent/roundguard"
	"github.com/yi-nology/goagent-kernel/checkpoint"
	"github.com/yi-nology/goagent-kernel/eventbus"

	"github.com/yi-nology/goagent-runtime/agentloop"
	"github.com/yi-nology/goagent-runtime/chat"
	"github.com/yi-nology/goagent-runtime/observe"
	"github.com/yi-nology/goagent-runtime/pipeline"
	"github.com/yi-nology/goagent-runtime/storage"
	"github.com/yi-nology/goagent-runtime/storage/sqlitestore"
)

// DefaultSideEffectTTL 工具副作用租约默认有效期。
const DefaultSideEffectTTL = 30 * time.Second

// Config 宿主注入面：模型、工具、提示词、额度、观测——全是领域，全由宿主给。
type Config struct {
	// Model 供应商中立的模型调用面（必填）。
	Model chat.Model
	// Tools 工具列表（名称/描述/参数模式/执行体）。
	Tools []pipeline.Tool
	// System 可选系统提示。
	System string
	// Budget 回合/工具额度；零值 = 不限。
	Budget roundguard.Budget
	// Observer 事件观测回调；nil = 不出事件给宿主。
	Observer observe.Sink
	// ApprovalGate 工具执行前的人工审批闸（宿主注入面）。
	//
	// ⚠️ 只要注册的工具里有声明 pipeline.Tool.NeedsApproval 的，这里**必须**
	// 装配闸——否则装配直接失败（fail-closed）。装配层不提供「默认放行」
	// 这条路：审批被静默绕过是安全缺陷。
	ApprovalGate agentloop.ApprovalGateFunc
}

// Runtime 装配完成的运行时。
type Runtime struct {
	// cfg / provider / ttl / emitter / joint 装配事实面：会话级覆盖
	// （SessionOptions）在此基础上合成新的 Loop（buildLoop）。
	cfg      Config
	provider storage.Provider
	ttl      time.Duration
	emitter  agentloop.Emitter
	joint    agentloop.JointCommitter

	loop      *agentloop.Loop
	stop      func()
	closeFn   func() error
	loopBus   *eventbus.Bus
	publisher *eventbus.DurablePublisher
	// store 快照读面（Session.Status 的投影源）。装配期必非 nil。
	store checkpoint.StoreCAS
	// closed Close 已收尾标志（幂等 Close + 事后 fail-fast 的判据）。
	closed atomic.Bool
	// mu 保护 sessionStops（会话级观测回调的 detach 登记）。
	mu sync.Mutex
	// sessionStops 带 Observer 的会话在 NewSession 时登记的 detach——
	// Runtime.Close 统一收尾（会话对象自身没有 Close，资源归 Runtime）。
	sessionStops []func()
}

// Run 执行（或续跑）一个会话。sessionID 是恢复锚点：同一 ID 的多次 Run
// 共享历史；进程重启后用同一 ID 再 Run 即从上次快照恢复。
//
// 兼容入口（薄委托到 Session）：新代码建议直接 NewSession 拿会话对象，
// 会话身份与 Run 入口在一处（issue #38 对象模型）。
func (r *Runtime) Run(ctx context.Context, sessionID, user string) (*agentloop.Result, error) {
	s, err := r.NewSession(ctx, SessionOptions{ID: sessionID})
	if err != nil {
		return nil, err
	}
	return s.Run(ctx, user)
}

// ensureOpen 关闭后 fail-fast：存储已收尾，继续执行只会炸出含糊的驱动层
// 错误（sql: database is closed），不如在入口说清「runtime 已关闭」。
func (r *Runtime) ensureOpen() error {
	if r.closed.Load() {
		return errors.New("runtime 已关闭（Close 之后不接受新执行与读查询）")
	}
	return nil
}

// Close 收尾：停观测转发（Runtime 级 + 各会话级）、关存储。幂等——重复
// Close（含与已收尾状态竞态）不重复释放，返回 nil。
func (r *Runtime) Close() error {
	if !r.closed.CompareAndSwap(false, true) {
		return nil
	}
	// 会话级观测 detach：与 trackSessionStop 同一把锁——先置 closed 再
	// 抽干登记表，之后登记的会话 detach 会当场执行（见 trackSessionStop），
	// 不漏不重。
	r.mu.Lock()
	stops := r.sessionStops
	r.sessionStops = nil
	r.mu.Unlock()
	for _, stop := range stops {
		stop()
	}
	r.stop()
	if r.publisher != nil {
		// DurablePublisher 无 Close 语义：订阅随 ctx 收尾，这里只断引用。
		r.publisher = nil
	}
	if r.closeFn != nil {
		return r.closeFn()
	}
	return nil
}

// trackSessionStop 登记会话级观测 detach。已关闭的 Runtime 当场执行
// （不入表——入了也没人收）。
func (r *Runtime) trackSessionStop(stop func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed.Load() {
		stop()
		return
	}
	r.sessionStops = append(r.sessionStops, stop)
}

// Option 装配选项。
type Option func(*assembly)

type assembly struct {
	storage storage.Provider
	ttl     time.Duration
}

// WithStorage 注入自定义存储（宿主自落库、或多个 Runtime 共享同一存储时
// 用——演示中断恢复就是共享同一 Provider 的两个 Runtime）。
func WithStorage(p storage.Provider) Option {
	return func(a *assembly) { a.storage = p }
}

// WithSideEffectTTL 覆盖工具副作用租约有效期。
func WithSideEffectTTL(d time.Duration) Option {
	return func(a *assembly) { a.ttl = d }
}

// NewEmbedded 装配 embedded 模板：内存存储、零外部依赖。
// 单进程形态——进程退出即失历史；要持久化用 NewStandalone。
func NewEmbedded(cfg Config, opts ...Option) (*Runtime, error) {
	return assemble(cfg, opts, func(a *assembly) error {
		if a.storage == nil {
			a.storage = storage.NewMem()
		}
		return nil
	}, nil)
}

// NewStandalone 装配 standalone 模板：SQLite 单文件持久化。
// sqlitePath 是库文件路径；进程重启后同一 sessionID 从快照恢复。
func NewStandalone(cfg Config, sqlitePath string, opts ...Option) (*Runtime, error) {
	if sqlitePath == "" {
		return nil, errors.New("standalone 模板必须给 SQLite 路径（空路径 = 匿名库，历史无处可寻）")
	}
	var sq *sqlitestore.SQLite
	rt, err := assemble(cfg, opts, func(a *assembly) error {
		if a.storage != nil {
			return nil // 宿主已注入存储（含测试），不再开库
		}
		st, err := sqlitestore.Open(context.Background(), sqlitePath)
		if err != nil {
			return err
		}
		sq = st
		a.storage = st
		return nil
	}, func() error {
		if sq != nil {
			return sq.Close()
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return rt, nil
}

// assemble 模板公共装配顺序：选项 → 存储 → 事件面 → 合成共享 Loop。
func assemble(cfg Config, opts []Option, prep func(*assembly) error, closeFn func() error) (*Runtime, error) {
	a := &assembly{ttl: DefaultSideEffectTTL}
	for _, o := range opts {
		o(a)
	}
	if cfg.Model == nil {
		return nil, errors.New("runtime 缺模型面")
	}
	if err := prep(a); err != nil {
		return nil, err
	}
	if a.storage == nil || a.storage.Checkpoints() == nil || a.storage.Effects() == nil {
		return nil, errors.New("runtime 存储面不完整（Checkpoints/Effects 缺一不可）")
	}

	bus := eventbus.New()
	rt := &Runtime{
		cfg:      cfg,
		provider: a.storage,
		ttl:      a.ttl,
		stop:     func() {},
		closeFn:  closeFn,
		loopBus:  bus,
		store:    a.storage.Checkpoints(),
	}

	// 事件发射三分派：Provider 支持联合提交（两脸同库，如 SQLite）⇒ Joint
	// 模式——事件随快照单事务落库（issue #2 主体：孤儿事件/终态事件丢失的
	// 崩溃窗口结构闭合），广播走裸总线；否则有持久化面走 DurablePublisher
	// （两步旧路径）；都没有=裸广播（embedded）。
	if j, ok := a.storage.(storage.JointCommitter); ok && a.storage.Events() != nil {
		rt.joint = j
	} else if a.storage.Events() != nil {
		rt.publisher = eventbus.NewDurablePublisher(bus, a.storage.Events())
		rt.emitter = durableEmitter{p: rt.publisher}
	}
	if cfg.Observer != nil {
		rt.stop = observe.Attach(context.Background(), bus, cfg.Observer)
	}

	loop, err := rt.buildLoop(SessionOptions{})
	if err != nil {
		rt.stop()
		return nil, err
	}
	rt.loop = loop
	return rt, nil
}

// buildLoop 按（可能带会话覆盖的）口径装配一个 Loop。
//
// 共享面（模型 / 存储 / 事件面 / 总线 / 审批闸）恒取 Runtime 级装配事实；
// 覆盖只动 System / Budget / Tools 三件（nil = 继承，非 nil = 本会话生效）。
// 无覆盖时装配出的 Loop 与共享 Loop 语义等价——只有带覆盖的会话才真建。
func (r *Runtime) buildLoop(opts SessionOptions) (*agentloop.Loop, error) {
	tools := r.cfg.Tools
	if opts.Tools != nil {
		tools = opts.Tools
	}
	pipe, err := pipeline.New(r.provider.Effects(), r.ttl, tools)
	if err != nil {
		return nil, err
	}
	system := r.cfg.System
	if opts.System != "" {
		system = opts.System
	}
	budget := r.cfg.Budget
	if opts.Budget != nil {
		budget = *opts.Budget
	}
	return agentloop.New(agentloop.Config{
		Model:        r.cfg.Model,
		Tools:        pipe,
		Store:        r.store,
		Emitter:      r.emitter,
		Joint:        r.joint,
		Bus:          r.loopBus,
		Budget:       budget,
		System:       system,
		ApprovalGate: r.cfg.ApprovalGate,
	})
}

// durableEmitter 把 eventbus.DurablePublisher 包成 agentloop.Emitter。
type durableEmitter struct{ p *eventbus.DurablePublisher }

func (e durableEmitter) Publish(ctx context.Context, sessionID, caseID string, stepID *uint64, typ, level string, payload map[string]any) error {
	_, err := e.p.Publish(ctx, eventbus.Event{
		SessionID: sessionID, CaseID: caseID, StepID: stepID,
		Type: typ, Level: level, Payload: payload, OccurredAt: time.Now(),
	})
	return err
}
