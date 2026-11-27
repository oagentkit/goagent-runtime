package runtime

import (
	"context"

	"github.com/oekit/pkg/errors"

	"github.com/yi-nology/goagent-kernel/checkpoint"

	"github.com/yi-nology/goagent-runtime/agentloop"
)

// SessionOptions NewSession 的选项。
type SessionOptions struct {
	// ID 会话 ID（恢复锚点）：同一 ID 的多次 Run 共享历史；进程重启后用同一
	// ID 再 Run 即从上次快照恢复。必填——空 ID 是编程错误：匿名会话无处可寻，
	// 且所有空 ID 会话的历史会在同一锚点上互相碰撞。
	ID string
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
// Session 值可安全并发使用：本对象无易变字段，执行的可变状态全在
// agentloop 的 runState（每次 Run 局部）与 kernel 快照序号闸里。
type Session struct {
	rt *Runtime
	id string
}

// ID 会话 ID（恢复锚点）。
func (s *Session) ID() string { return s.id }

// Run 执行（或续跑）本会话的一个回合链。语义与 Runtime.Run 完全一致：
// 同一锚点共享历史、挂起等审批的会话幂等短路、进程重启后同 ID 恢复。
func (s *Session) Run(ctx context.Context, user string) (*agentloop.Result, error) {
	if err := s.rt.ensureOpen(); err != nil {
		return nil, err
	}
	return s.rt.loop.Run(ctx, s.id, user)
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
	return s.rt.loop.Approve(ctx, s.id, callID, approved)
}

// NewSession 打开会话（issue #38：应用开发入口的对象面）。
//
// 打开是轻操作：校验锚点、挂上存储读面，不落任何新状态——「会话存在」
// 由快照说了算（Status.Exists），不由打开动作说了算。同一 ID 重复打开
// 返回等价对象（恢复 / 接力场景就是再打开一次）。
func (r *Runtime) NewSession(_ context.Context, opts SessionOptions) (*Session, error) {
	if opts.ID == "" {
		return nil, errors.New("会话必须给 ID（空 ID = 匿名会话，恢复无锚点且历史互相碰撞）")
	}
	if err := r.ensureOpen(); err != nil {
		return nil, err
	}
	return &Session{rt: r, id: opts.ID}, nil
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
