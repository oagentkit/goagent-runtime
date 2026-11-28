package runtime

import (
	"context"

	"github.com/oekit/pkg/errors"

	"github.com/oagentkit/agent/roundguard"
	"github.com/yi-nology/goagent-kernel/checkpoint"
	"github.com/yi-nology/goagent-kernel/eventbus"
	"github.com/yi-nology/goagent-kernel/invocation"

	"github.com/yi-nology/goagent-runtime/agentloop"
	"github.com/yi-nology/goagent-runtime/observe"
	"github.com/yi-nology/goagent-runtime/pipeline"
)

// SessionOptions NewSession 的选项。
//
// # 配置分层（issue #41）
//
// Runtime 级 Config 是默认；下列覆盖字段**只作用于本会话**，不泄漏到
// 其他会话、不改 Runtime 默认值：
//
//   - nil / 空（零值）= 继承 Runtime 级；
//   - 显式给出 = 本会话生效（Tools 的 nil 与空切片有别：nil = 继承，
//     `[]pipeline.Tool{}` = 本会话无工具）。
//
// System 覆盖只影响**首跑**：历史整体落快照，系统提示已固化在首条消息里，
// 续跑不重写历史（改系统提示 = 换一个会话 ID）。
type SessionOptions struct {
	// ID 会话 ID（恢复锚点）：同一 ID 的多次 Run 共享历史；进程重启后用同一
	// ID 再 Run 即从上次快照恢复。必填——空 ID 是编程错误：匿名会话无处可寻，
	// 且所有空 ID 会话的历史会在同一锚点上互相碰撞。
	ID string
	// System 覆盖本会话的系统提示；空 = 继承 Runtime 级（只影响首跑，
	// 见类型注释）。
	System string
	// Budget 覆盖本会话预算；nil = 继承 Runtime 级（每次 Run 独立计数）。
	Budget *roundguard.Budget
	// Tools 覆盖本会话工具面；nil = 继承 Runtime 级全量；非 nil（可为空
	// 切片）= 仅用此列表（换工具面不换模型面——本会话的工具面在打开时
	// 定死，装配错误在 NewSession 即报）。
	Tools []pipeline.Tool
	// Observer 本会话的事件观察回调：**附加**在 Runtime 级 Observer 之上，
	// 按 sessionID 过滤（只收本会话事件）。detach 归 Runtime.Close——
	// 会话对象没有 Close。
	Observer observe.Sink
	// Reconcile 本会话的账目裁决钩子（kernel invocation 契约的宿主裁决面；
	// 可选，nil = 不裁决——恢复遇到 uncertain/executing 账目时只报告不裁决）。
	//
	// 判据归 kernel（Recovery 判「要做什么」），怎么裁决是宿主的事：远端到底
	// 生效没有，只有宿主知道。做成**会话级覆盖**的原因：裁决语义依赖工具集
	// （纯只读工具集恢复重做不可能重复副作用，可自动放行；含变更类工具集
	// 「变更类不猜」），宿主按步骤工具集决定装不装。
	Reconcile func(ctx context.Context, sessionID string, e invocation.Entry) (invocation.State, []byte, error)
}

// SessionStatus 会话状态读数（投影自已落库的 checkpoint 快照）。
//
// 状态的真相在 kernel 判据面（快照 Status 词表 + 终态分类），本结构只搬运
// 不解释、只读不写——不另造第二套状态机。
type SessionStatus struct {
	// Exists 是否已有快照（false = 本会话从未执行过，不是错误）。
	Exists bool
	// Status contract 状态词表（running / waiting_approval / succeeded /
	// failed / timed_out …）；Exists=false 时为空串。
	Status string
	// Seq 最后一次提交的快照序号（执行推进的证据；Exists=false 时为 0）。
	Seq uint64
}

// Session 会话一等对象：承载会话身份与 Run 入口（W1 对象模型，issue #38）。
//
// # 生命周期与资源
//
//   - NewSession 打开会话——**不制造状态**：历史由 checkpoint 存储承载，
//     同一 ID 可重复打开，接力 / 恢复都是「再打开」；
//   - Run 执行或续跑一个回合链（与 Runtime.Run 同一锚点语义）；
//   - Status 读当前状态（投影，不解释语义）；
//   - 资源归 Runtime：Session 自身不持有需释放的资源，Runtime.Close 统一
//     收尾；Close 之后 Session 各方法 fail-fast 报错（存储已收尾，别等到
//     驱动层才炸出一个含糊的 sql: database is closed）。
//
// Session 值可安全并发使用：执行的可变状态全在 agentloop 的 runState
// （每次 Run 局部）与 kernel 快照序号闸里；loop 在 NewSession 一次装配
// 后只读复用。
type Session struct {
	rt *Runtime
	id string
	// loop 会话覆盖装配出的 Loop；nil = 用 Runtime 共享 Loop（无覆盖）。
	loop *agentloop.Loop
}

// ID 会话 ID（恢复锚点）。
func (s *Session) ID() string { return s.id }

// Run 执行（或续跑）本会话的一个回合链。语义与 Runtime.Run 完全一致：
// 同一锚点共享历史、挂起等审批的会话幂等短路、进程重启后同 ID 恢复。
func (s *Session) Run(ctx context.Context, user string) (*agentloop.Result, error) {
	if err := s.rt.ensureOpen(); err != nil {
		return nil, err
	}
	return s.loopOf().Run(ctx, s.id, user)
}

// loopOf 本会话执行用的 Loop：带覆盖走会话级，否则共享。
func (s *Session) loopOf() *agentloop.Loop {
	if s.loop != nil {
		return s.loop
	}
	return s.rt.loop
}

// Status 读会话当前状态。首次执行前（无快照）返回 Exists=false 的零值，
// 不是错误——「还没跑过」是常态不是异常。
func (s *Session) Status(ctx context.Context) (SessionStatus, error) {
	if err := s.rt.ensureOpen(); err != nil {
		return SessionStatus{}, err
	}
	snap, err := s.rt.store.Load(ctx, s.id)
	if errors.Is(err, checkpoint.ErrNotFound) {
		return SessionStatus{}, nil
	}
	if err != nil {
		return SessionStatus{}, errors.Wrap(err, "读会话快照失败", "session", s.id)
	}
	return SessionStatus{Exists: true, Status: snap.Status, Seq: snap.Seq}, nil
}

// Approve 落审批决策并续跑挂起中的会话（中断 → 恢复的统一接缝，issue #38）。
//
// approved=true ⇒ 挂起的调用走标准施加面执行；false ⇒ 拒绝观察回喂模型。
// callID 必须精确匹配挂起的调用（fail-closed，防陈旧决策误放行新调用）。
// 会话不在挂起态时报错。语义与 agentloop.Loop.Approve 一致。
func (s *Session) Approve(ctx context.Context, callID string, approved bool) (*agentloop.Result, error) {
	if err := s.rt.ensureOpen(); err != nil {
		return nil, err
	}
	return s.loopOf().Approve(ctx, s.id, callID, approved)
}

// NewSession 打开会话（issue #38：应用开发入口的对象面；#41 配置分层）。
//
// 打开是轻操作：校验锚点、按覆盖装配会话级 Loop（仅当有覆盖）、挂观测，
// **不落任何新状态**——「会话存在」由快照说了算（Status.Exists），不由
// 打开动作说了算。同一 ID 重复打开返回等价对象（恢复 / 接力场景就是再
// 打开一次）。带覆盖时装配错误（工具重名、审批闸缺失等 fail-closed 判定）
// 在此即报——不等到第一次 Run。
func (r *Runtime) NewSession(_ context.Context, opts SessionOptions) (*Session, error) {
	if opts.ID == "" {
		return nil, errors.New("会话必须给 ID（空 ID = 匿名会话，恢复无锚点且历史互相碰撞）")
	}
	if err := r.ensureOpen(); err != nil {
		return nil, err
	}
	s := &Session{rt: r, id: opts.ID}
	if opts.System != "" || opts.Budget != nil || opts.Tools != nil || opts.Reconcile != nil {
		loop, err := r.buildLoop(opts)
		if err != nil {
			return nil, errors.Wrap(err, "会话覆盖装配失败", "session", opts.ID)
		}
		s.loop = loop
	}
	if opts.Observer != nil {
		sink := opts.Observer
		id := opts.ID
		// 过滤转发：会话级 Observer 只收本会话事件；Runtime 级照旧全收。
		detach := observe.Attach(context.Background(), r.loopBus, func(ev eventbus.Event) {
			if ev.SessionID == id {
				sink(ev)
			}
		})
		r.trackSessionStop(detach)
	}
	return s, nil
}

// Approve 落审批决策并续跑挂起中的会话。兼容入口（薄委托到 Session），
// 语义与 Session.Approve 一致。
func (r *Runtime) Approve(ctx context.Context, sessionID, callID string, approved bool) (*agentloop.Result, error) {
	s, err := r.NewSession(ctx, SessionOptions{ID: sessionID})
	if err != nil {
		return nil, err
	}
	return s.Approve(ctx, callID, approved)
}
