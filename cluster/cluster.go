// Package cluster —— 多副本高可用装配模板（PostgreSQL + worker 池）。
//
// # 它是什么
//
// embedded（内存）与 standalone（SQLite 单文件）都是**单进程**形态：
// 进程退出历史即失、进程被换掉任务即悬空。本包给出第三种形态——
// **多个副本共享一个 PostgreSQL**：任一副本可接单，任一副本可执行，
// 同一份会话的执行权在集群内**有且只有一个副本**（认领 + 租约 + 接管代次，
// 全部走 kernel 的 taskboard 判据），副本崩了任务由别的副本接手，
// 已施加的工具副作用**不会被第二次执行**（幂等闸在共享的 PG 里）。
//
// # 分层（不发明第二套）
//
//	宿主产品      只写领域：系统提示、工具实现、模型供应商适配
//	cluster       本层：接单 → 派单 → 认领 → 执行 → 回传结果
//	kernel        判据：taskboard 抢占/租约/fence、sideeffect 幂等闸、
//	              roundguard 预算、eventbus 事件面
//	pgstore       参考实现：11 面存储门面（PG）
//
// 认领的原子性、租约的失格、接管的代次单调，全部是 kernel 的判据；本包
// 只做**接线**：把「投递 → 认领 → 执行 → 回传」串起来。
//
// # 任务模型
//
// 一个任务 = **一次待执行的会话**：
//
//   - 任务 ID = 会话 ID（taskboard.Task.ID）；
//   - taskboard.Task.Title 承载本次的用户输入（本模板的约定；kernel 的
//     Title 语义是「展示用标题」，这里借它当输入载体——taskboard 契约里
//     没有 payload 字段，本模板不为此新造一个存储面）；
//   - 会话历史由 checkpoint 累积（同 sessionID 的多次执行共享历史，
//     与 runtime.Run 的语义一致）。
//
// 投递（同 sessionID 再次投递 = 同一会话的下一轮）：
//
//   - 任务不存在   → 插入 open；
//   - 已终态       → 重新打开（清租约，**不重置 fence**：接管代次必须单调）；
//   - 正在进行中   → taskboard.ErrTaken（覆盖会让在途令牌失效且可能双执行）；
//   - 已排队       → 幂等无操作。
//
// # 结果回传
//
// 执行副本把结果作为**事件**写进 PG 事件面（eventbus.Store 面，kernel
// 判据：ID 单调、水位线无空洞），等待方按 sessionID 过滤取回。
// 走事件面而不是新增「结果表」：结果面不是判据面，事件面已经有跨副本
// 的次序与补灌语义，另造一张表等于让「同一事实有两份真相」。
//
// # 并发前提
//
// agentloop.Loop 的快照序号是**进程内单调游标**，同一 Loop 的 Run 不得并发
// 调用——本包的 worker 池**每个 worker 独占一个 Loop 实例**，互不共用；
// 同一会话也不会被两个 worker 同时执行（认领 + 租约保证）。
package cluster

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/oekit/pkg/errors"

	"github.com/yi-nology/goagent-kernel/eventbus"
	"github.com/oagentkit/agent/roundguard"
	"github.com/yi-nology/goagent-kernel/taskboard"

	"github.com/yi-nology/goagent-runtime/agentloop"
	"github.com/yi-nology/goagent-runtime/chat"
	"github.com/yi-nology/goagent-runtime/observe"
	"github.com/yi-nology/goagent-runtime/pgstore"
	"github.com/yi-nology/goagent-runtime/pipeline"
)

// ResultEventType 结果事件类型：执行副本把一次执行的 Result 写进事件面，
// 等待方按 (sessionID, 本类型) 取回。
const ResultEventType = "session.result"

// 默认参数（显式可配，零值走默认——不猜）。
const (
	// DefaultWorkers 默认 worker 数。
	DefaultWorkers = 4
	// DefaultLeaseTTL 默认认领租约有效期。取 30s：足够一次典型回合，
	// 又不至于让崩溃副本的任务被锁太久。
	DefaultLeaseTTL = 30 * time.Second
	// DefaultPollInterval 默认轮询间隔（worker 空闲时的等待步长）。
	DefaultPollInterval = 300 * time.Millisecond
	// DefaultWaitTimeout 默认等待结果的超时。
	DefaultWaitTimeout = 2 * time.Minute
	// DefaultSchema 默认 schema 名（PG 的 public）。
	DefaultSchema = "public"
	// sideEffectTTL 工具副作用闸的租约有效期（与 runtime 模板同口径：30s）。
	// 它与**任务认领租约**是两回事：前者管「同一副作用至多施加一次」，
	// 后者管「同一任务同一时刻只有一个副本在跑」。
	sideEffectTTL = 30 * time.Second
)

// Config 集群模板装配配置（宿主注入面 + 集群口径）。
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
	// 装配闸——否则集群启动直接失败（fail-closed）。装配层不提供「默认放行」
	// 这条路：审批被静默绕过是安全缺陷。
	ApprovalGate agentloop.ApprovalGateFunc
	// Workers worker 池大小；零值 = DefaultWorkers。
	Workers int
	// LeaseTTL 认领租约有效期；零值 = DefaultLeaseTTL。
	LeaseTTL time.Duration
	// PollInterval worker 空闲轮询间隔；零值 = DefaultPollInterval。
	PollInterval time.Duration
	// WaitTimeout Wait 的默认超时；零值 = DefaultWaitTimeout。
	WaitTimeout time.Duration
	// Schema PG schema 名；零值 = DefaultSchema。
	Schema string
}

// Cluster 多副本集群运行时。
type Cluster struct {
	cfg     Config
	pool    *pgxpool.Pool
	store   *pgstore.Store
	board   *pgstore.BoardStore
	events  *pgstore.EventStore
	bus     *eventbus.Bus
	agentID taskboard.AgentID

	workers []*executor
	stop    func()
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	closed  bool

	// mu 保护 baselines（等待起点登记表）。
	mu sync.Mutex
	// baselines 会话 → 本次**投递前**的事件面水位线（等待起点）。
	//
	// 为什么需要它：结果事件按 sessionID 过滤，而 sessionID 是**长期会话
	// 身份**——同一会话的第 2 轮与第 1 轮共用一个 ID。等待方若从事件流开头
	// 扫，会先撞上第 1 轮的结果并立刻返回（拿到的不是本轮结果）。
	// 投递前记下的水位线把「上一轮的结果」挡在起点之外。
	baselines map[string]uint64
}

// New 装配集群模板：连 PG → 建表（幂等）→ 打开各存储面。
//
// dsn 是 PostgreSQL 连接串；schema 里的参考表由本函数幂等建好
// （pgstore.EnsureSchema），**不必**由宿主预先执行迁移脚本。
func New(ctx context.Context, cfg Config, dsn string) (*Cluster, error) {
	if cfg.Model == nil {
		return nil, errors.New("cluster 缺模型面（nil Model = 没有回合的另一端）")
	}
	if dsn == "" {
		return nil, errors.New("cluster 必须给 PG 连接串（空 DSN = 无处安放会话与租约）")
	}
	cfg = withDefaults(cfg)

	// 连接池级指定 search_path：**建表与读写必须在同一个 schema**——
	// 只在建表时切 schema 而读写走连接缺省 schema，等于「表建了但读不到」。
	pcfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, errors.Wrap(err, "cluster: 解析 PG 连接串失败")
	}
	if pcfg.ConnConfig.RuntimeParams == nil {
		pcfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	pcfg.ConnConfig.RuntimeParams["search_path"] = cfg.Schema
	pool, err := pgxpool.NewWithConfig(ctx, pcfg)
	if err != nil {
		return nil, errors.Wrap(err, "cluster: 连接 PG 失败")
	}
	if err := pgstore.EnsureSchema(ctx, pool, cfg.Schema); err != nil {
		pool.Close()
		return nil, errors.Wrap(err, "cluster: 建参考表失败", "schema", cfg.Schema)
	}
	store := pgstore.NewStore(pool)
	c := &Cluster{
		cfg:       cfg,
		pool:      pool,
		store:     store,
		board:     store.Board(),
		events:    store.Events(),
		bus:       eventbus.New(),
		agentID:   taskboard.AgentID("cluster-" + randomID()),
		baselines: map[string]uint64{},
	}
	return c, nil
}

// withDefaults 零值走默认（不猜宿主意图，但也绝不静默降级成别的口径）。
func withDefaults(cfg Config) Config {
	if cfg.Workers <= 0 {
		cfg.Workers = DefaultWorkers
	}
	if cfg.LeaseTTL <= 0 {
		cfg.LeaseTTL = DefaultLeaseTTL
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = DefaultPollInterval
	}
	if cfg.WaitTimeout <= 0 {
		cfg.WaitTimeout = DefaultWaitTimeout
	}
	if cfg.Schema == "" {
		cfg.Schema = DefaultSchema
	}
	return cfg
}

// Board 暴露任务认领面（宿主要播种拓扑、或自己做运维视图时用）。
func (c *Cluster) Board() *pgstore.BoardStore { return c.board }

// Events 暴露事件面（宿主要跨副本订阅时用）。
func (c *Cluster) Events() *pgstore.EventStore { return c.events }

// Effects 暴露副作用闸面（宿主做对账裁决时用；也可用于演练崩溃现场）。
func (c *Cluster) Effects() *pgstore.EffectStore { return c.store.Effects() }

// Submit 投递一次待执行的会话（同 sessionID 再次投递 = 同一会话的下一轮）。
//
// 投递**前**记下事件面水位线（等待起点）：本轮的结果事件必然在它之后
// （事件 ID 单调无空洞）⇒ 等待方从这条线往后取，不会把上一轮的结果当成
// 本轮的返回。
func (c *Cluster) Submit(ctx context.Context, sessionID, user string) error {
	if sessionID == "" {
		return errors.New("cluster: 会话 ID 不能为空")
	}
	base, err := c.events.MaxID(ctx)
	if err != nil {
		return errors.Wrap(err, "cluster: 读事件面水位线失败", "session", sessionID)
	}
	if err := c.board.Submit(ctx, taskboard.Task{
		ID:    taskboard.TaskID(sessionID),
		Title: user,
		State: taskboard.StateOpen,
	}); err != nil {
		return err
	}
	c.setBaseline(sessionID, base)
	return nil
}

// setBaseline 登记会话的等待起点。
func (c *Cluster) setBaseline(sessionID string, cursor uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.baselines[sessionID] = cursor
}

// baseline 取会话的等待起点；未登记过 = 0（从事件流开头找）。
func (c *Cluster) baseline(sessionID string) uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.baselines[sessionID]
}

// Run 投递并等待结果（同步语义：内部 = Submit + Wait）。
func (c *Cluster) Run(ctx context.Context, sessionID, user string) (*agentloop.Result, error) {
	if err := c.Submit(ctx, sessionID, user); err != nil {
		return nil, err
	}
	return c.Wait(ctx, sessionID)
}

// Wait 等待某会话的执行结果（结果经事件面回传）。
//
// 起点 = 本会话本次投递前的水位线（见 Submit）：上一轮的结果在起点之前，
// 不会被当成这一轮的返回。
//
// 超时口径：ctx 取消 ⇒ 返回 ctx.Err()；到达 WaitTimeout ⇒ ErrWaitTimeout。
func (c *Cluster) Wait(ctx context.Context, sessionID string) (*agentloop.Result, error) {
	return c.WaitSince(ctx, sessionID, c.baseline(sessionID))
}

// ResultCursor 返回当前事件面水位线（等待起点）。
//
// 跨副本等待用：本副本投递、另一副本等待时，把这个游标交给 WaitSince
// （另一副本没有本会话的投递记录，取不到起点）。
func (c *Cluster) ResultCursor(ctx context.Context) (uint64, error) {
	return c.events.MaxID(ctx)
}

// WaitSince 从 since 之后的事件里等本会话的结果（跨副本等待用）。
func (c *Cluster) WaitSince(ctx context.Context, sessionID string, since uint64) (*agentloop.Result, error) {
	if sessionID == "" {
		return nil, errors.New("cluster: 等待结果的会话 ID 不能为空")
	}
	deadline := time.Now().Add(c.cfg.WaitTimeout)
	cursor := since
	for {
		evs, err := c.events.ListAfter(ctx, cursor, 200)
		if err != nil {
			return nil, errors.Wrap(err, "cluster: 读事件面失败", "session", sessionID)
		}
		for _, ev := range evs {
			if ev.ID > cursor {
				cursor = ev.ID
			}
			if ev.SessionID != sessionID || ev.Type != ResultEventType {
				continue
			}
			res, err := decodeResult(ev.Payload)
			if err != nil {
				return nil, errors.Wrap(err, "cluster: 结果事件解码失败", "session", sessionID)
			}
			return res, nil
		}
		if time.Now().After(deadline) {
			return nil, errors.Wrap(ErrWaitTimeout, "等待会话结果超时", "session", sessionID)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(c.cfg.PollInterval):
		}
	}
}

// ErrWaitTimeout 等待结果超时（结果仍未到达事件面）。
var ErrWaitTimeout = errors.New("cluster: 等待会话结果超时")

// resultPayload 结果事件的载荷（JSON 字符串形态——payload 是 map[string]any，
// 结构体经 JSON 往返会退化成 map[string]any，类型不稳；故载荷本身是
// 一个 JSON 字符串，取回时按固定结构解码，类型稳定）。
type resultPayload struct {
	Status    string   `json:"status"`
	Final     string   `json:"final"`
	Rounds    int      `json:"rounds"`
	ToolCalls int      `json:"tool_calls"`
	Uncertain []string `json:"uncertain,omitempty"`
}

// encodeResult 结果 → 事件载荷。
func encodeResult(res *agentloop.Result) (map[string]any, error) {
	if res == nil {
		return nil, errors.New("cluster: 结果为空（无内容可回传）")
	}
	raw, err := json.Marshal(resultPayload{
		Status:    res.Status,
		Final:     res.Final,
		Rounds:    res.Rounds,
		ToolCalls: res.ToolCalls,
		Uncertain: res.Uncertain,
	})
	if err != nil {
		return nil, errors.Wrap(err, "cluster: 结果序列化失败")
	}
	return map[string]any{"result": string(raw)}, nil
}

// decodeResult 事件载荷 → 结果。
func decodeResult(payload map[string]any) (*agentloop.Result, error) {
	raw, ok := payload["result"].(string)
	if !ok || raw == "" {
		return nil, errors.New("cluster: 事件载荷缺 result 字段")
	}
	var p resultPayload
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return nil, errors.Wrap(err, "cluster: 结果载荷解析失败")
	}
	return &agentloop.Result{
		Status:    p.Status,
		Final:     p.Final,
		Rounds:    p.Rounds,
		ToolCalls: p.ToolCalls,
		Uncertain: p.Uncertain,
	}, nil
}

// Close 停 worker 池、关连接池。
func (c *Cluster) Close() error {
	if c.closed {
		return nil
	}
	c.closed = true
	if c.stop != nil {
		c.stop()
	}
	if c.cancel != nil {
		c.cancel()
	}
	c.wg.Wait()
	if c.pool != nil {
		c.pool.Close()
	}
	return nil
}
