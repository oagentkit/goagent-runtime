package cluster

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"time"

	"github.com/oekit/concurrency/async"
	"github.com/oekit/pkg/errors"

	"github.com/yi-nology/goagent-kernel/eventbus"
	"github.com/yi-nology/goagent-kernel/taskboard"
	kw "github.com/yi-nology/goagent-kernel/worker"

	"github.com/yi-nology/goagent-runtime/agentloop"
	"github.com/yi-nology/goagent-runtime/observe"
	"github.com/yi-nology/goagent-runtime/pipeline"
	"github.com/yi-nology/goagent-runtime/pgstore"
)

// executor 一个执行工位：独占一个 agentloop.Loop（同一 Loop 的 Run 不得并发）。
type executor struct {
	id   string
	loop *agentloop.Loop
}

// Start 启动 worker 池与事件转发器。
//
// worker 池 = 本副本的执行能力；**副本之间不共享内存**——协调全靠 PG
// 里的任务板（认领 + 租约 + 代次）与事件面。
func (c *Cluster) Start(ctx context.Context) error {
	if c.closed {
		return errors.New("cluster: 已关闭的实例不能启动")
	}
	if len(c.workers) > 0 {
		return errors.New("cluster: 已在运行（重复启动会起两套 worker 抢同一批任务）")
	}
	runCtx, cancel := context.WithCancel(ctx)
	c.cancel = cancel

	pipe, err := pipeline.New(c.store.Effects(), sideEffectTTL, c.cfg.Tools)
	if err != nil {
		cancel()
		return errors.Wrap(err, "cluster: 装配工具管线失败")
	}
	for i := 0; i < c.cfg.Workers; i++ {
		// 每 worker 一个 Loop：Loop 的快照序号是进程内单调游标，共用即串扰。
		loop, err := agentloop.New(agentloop.Config{
			Model:   c.cfg.Model,
			Tools:   pipe,
			Store:   c.store.Snapshots(),
			Emitter: pgEmitter{store: c.events},
			Budget:  c.cfg.Budget,
			System:  c.cfg.System,
		})
		if err != nil {
			cancel()
			return errors.Wrap(err, "cluster: 装配回合循环失败")
		}
		w := &executor{id: string(c.agentID) + "/w" + itoa(i), loop: loop}
		c.workers = append(c.workers, w)
		c.wg.Add(1)
		async.GoSafe(func() {
			defer c.wg.Done()
			c.runWorker(runCtx, w)
		})
	}

	// 宿主观测：订阅本地 bus（事件由下面的转发器统一灌入）。
	if c.cfg.Observer != nil {
		c.stop = observe.Attach(runCtx, c.bus, c.cfg.Observer)
	}

	// 事件转发器：把 PG 事件面（**全集群**的事件）搬到本地 bus，
	// 宿主的 Observer 只接一份——事件面是单一真相，不各副本各播一遍。
	c.wg.Add(1)
	async.GoSafe(func() {
		defer c.wg.Done()
		c.forwardEvents(runCtx)
	})
	return nil
}

// runWorker 一个 worker 的执行循环：领任务 → 跑 → 回传结果 → 收场。
func (c *Cluster) runWorker(ctx context.Context, w *executor) {
	for {
		if ctx.Err() != nil {
			return
		}
		task, ok := c.claimNext(ctx, w)
		if !ok {
			if !sleepCtx(ctx, c.cfg.PollInterval) {
				return
			}
			continue
		}
		c.execute(ctx, w, task)
	}
}

// claimNext 领下一个可执行的任务。
//
// 认领用 kernel 的 taskboard 判据：Claim 原子（并发认领同一任务只有
// 一个赢家）、令牌与接管代次由存储铸造。被别人抢走（ErrTaken）不是错误
// —— 换一个任务即可。
func (c *Cluster) claimNext(ctx context.Context, w *executor) (taskboard.Task, bool) {
	tasks, err := c.board.List(ctx)
	if err != nil {
		return taskboard.Task{}, false
	}
	now := time.Now()
	for _, t := range tasks {
		// 可认领判据用 kernel 的单点函数（taskboard.LeaseHeld）：
		// 终态不可领；**活持有**（claimed 且租约未过期）不可领；
		// open 与「租约已过期的 claimed」可领——后者正是崩溃接管的入口
		// （持有者进程死了，租约到期后任务必须能被别人接手，
		//  否则就是「永久占用」：任务再也动不了）。
		if t.State.Terminal() {
			continue
		}
		if taskboard.LeaseHeld(t, now) {
			continue
		}
		claimed, err := c.board.Claim(ctx, t.ID, taskboard.AgentID(w.id), taskboard.ClaimRequest{TTL: c.cfg.LeaseTTL})
		if err != nil {
			// ErrTaken = 别的副本抢到了；ErrNotFound/ErrTerminal = 状态已变。
			continue
		}
		return claimed, true
	}
	return taskboard.Task{}, false
}

// execute 执行一次已认领的任务，并回传结果。
func (c *Cluster) execute(ctx context.Context, w *executor, task taskboard.Task) {
	lease, err := kw.NewLeaseSession(ctx, c.board, task, kw.LeaseSessionConfig{
		TTL:          c.cfg.LeaseTTL,
		RenewInterval: c.cfg.LeaseTTL / 3,
	})
	if err != nil {
		// 租约会话开不起来（凭据缺失等）⇒ 归还任务，让别的副本接手。
		_ = c.board.Release(ctx, task.ID, task.Lease.Token)
		return
	}
	defer func() { _ = lease.Close(context.WithoutCancel(ctx)) }()

	// 会话 ID = 任务 ID；本次用户输入 = 任务标题（Task.Title）。
	res, runErr := w.loop.Run(lease.Context(), string(task.ID), task.Title)
	if runErr != nil {
		// 执行失败或租约失格（lease.Context 被取消）：归还任务。
		// 归还用认领时的令牌——租约已易主时它会被判 ErrLeaseLost，忽略即可
		// （新持有者已经接手，迟到者放不掉别人的闸正是判据要的）。
		_ = c.board.Release(ctx, task.ID, lease.Token())
		return
	}

	// 结果回传：写事件面（先写结果再收场——反过来会丢结果）。
	payload, err := encodeResult(res)
	if err == nil {
		_, _ = c.events.Insert(ctx, eventbus.Event{
			SessionID:  string(task.ID),
			Type:       ResultEventType,
			Level:      "info",
			Payload:    payload,
			OccurredAt: time.Now(),
		})
	}
	if err := c.board.Complete(ctx, task.ID, lease.Token()); err != nil {
		// 完成失败（多半是租约已被接管）：任务会被别的副本接手重跑；
		// 幂等闸保证已施加的副作用不会被第二次施加。
		return
	}
}

// forwardEvents 把 PG 事件面的新事件搬到本地 bus（全集群事件单点入本地）。
func (c *Cluster) forwardEvents(ctx context.Context) {
	cursor := uint64(0)
	for {
		if ctx.Err() != nil {
			return
		}
		evs, err := c.events.ListAfter(ctx, cursor, 200)
		if err == nil {
			for _, ev := range evs {
				if ev.ID > cursor {
					cursor = ev.ID
				}
				c.bus.Broadcast(ev)
			}
		}
		if !sleepCtx(ctx, c.cfg.PollInterval) {
			return
		}
	}
}

// pgEmitter 把事件写进 PG 事件面（**不广播**——广播由 forwardEvents 统一做，
// 否则本地会收到两份：一份直接广播，一份从事件面读回来）。
type pgEmitter struct{ store *pgstore.EventStore }

// Publish 落库一条事件。
func (e pgEmitter) Publish(ctx context.Context, sessionID, caseID string, stepID *uint64, typ, level string, payload map[string]any) error {
	_, err := e.store.Insert(ctx, eventbus.Event{
		SessionID:  sessionID,
		CaseID:     caseID,
		StepID:     stepID,
		Type:       typ,
		Level:      level,
		Payload:    payload,
		OccurredAt: time.Now(),
	})
	return err
}

// sleepCtx 睡一段；ctx 取消返回 false（调用方据此退出）。
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// randomID 随机标识（worker ID 后缀）。
func randomID() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "fallback"
	}
	return hex.EncodeToString(b[:])
}

// itoa 小整数转字符串（worker 序号）。
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(buf[pos:])
}
