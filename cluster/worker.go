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
	"github.com/yi-nology/goagent-runtime/pgstore"
	"github.com/yi-nology/goagent-runtime/pipeline"
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
		// Joint=c.store ⇒ 事件随快照单事务落 PG（issue #9：孤儿事件/终态事件
		// 丢失的崩溃窗口结构闭合）。Emitter 不设：pgEmitter 的落库职责由联合
		// 提交承担，本副本不广播——Observer 的事件流走 forwardEvents 从 PG
		// 事件面统一转发（Joint 提交后事件已在 PG，转发语义不变）。
		loop, err := agentloop.New(agentloop.Config{
			Model:        c.cfg.Model,
			Tools:        pipe,
			Store:        c.store.Snapshots(),
			Joint:        c.store,
			Budget:       c.cfg.Budget,
			System:       c.cfg.System,
			ApprovalGate: c.cfg.ApprovalGate,
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
// ——下拍再来。
//
// **单次 Claim 形态**（runtime#24 方案 C①）：筛出第一个可领候选后只对它
// Claim 一次。列表量随任务数单调涨的 O(n) List 是另一层病灶（方案 C②：
// pgstore 实现 taskboard.ReadyBoard.ClaimReady 带就绪索引的单查询），
// 与本改动正交、另行排期（见 issue runtime#24 裁定）。
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
			// **单次 Claim 形态**（runtime#24 方案 C①，对齐 kernel/worker.Loop）：
			// 不再逐个试后续候选——那会在竞争激烈时每拍打出 n 个 FOR UPDATE
			// 认领事务（认领风暴）；被抢说明竞争在场，本拍收工下拍再来。
			return taskboard.Task{}, false
		}
		return claimed, true
	}
	return taskboard.Task{}, false
}

// execute 执行一次已认领的任务，并回传结果。
func (c *Cluster) execute(ctx context.Context, w *executor, task taskboard.Task) {
	lease, err := kw.NewLeaseSession(ctx, c.board, task, kw.LeaseSessionConfig{
		TTL:           c.cfg.LeaseTTL,
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

	// 结果回传 + 任务收场：**同一事务**（pgstore.Finish）。
	//
	// 为什么必须同事务（分开写会留半截状态）：
	//
	//   - 事件写失败、任务仍被完成 ⇒ 任务已是终态，等待方永远等不到结果；
	//   - 事件写成功、任务完成失败（租约已被接管）⇒ 旧执行者的结果对外可见，
	//     等待方可能拿到过期执行者的结论。
	//
	// ⇒ 任一失败整批回滚：任务保持可恢复状态（租约到期后由别的副本接管
	// 重跑；幂等闸保证已施加的副作用不会被第二次施加）。
	payload, err := encodeResult(res)
	if err != nil {
		// 结果编码失败（不该发生）：归还任务，让别的副本接手。
		_ = c.board.Release(ctx, task.ID, lease.Token())
		return
	}
	if err := c.store.Finish(ctx, pgstore.FinishRequest{
		TaskID: task.ID,
		Token:  lease.Token(),
		Event: eventbus.Event{
			SessionID:  string(task.ID),
			Type:       ResultEventType,
			Level:      "info",
			Payload:    payload,
			OccurredAt: time.Now(),
		},
	}); err != nil {
		// 收尾失败（整批已回滚）：任务保持可恢复状态，等接管重跑。
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
