package cluster_test

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/oekit/pkg/errors"

	"github.com/yi-nology/goagent-kernel/eventbus"
	"github.com/yi-nology/goagent-kernel/taskboard"

	"github.com/yi-nology/goagent-runtime/chat"
	"github.com/yi-nology/goagent-runtime/cluster"
	"github.com/yi-nology/goagent-runtime/pgstore"
	"github.com/yi-nology/goagent-runtime/pipeline"
)

// 本文件是 W3（runtime#40）的**失败模式 e2e 矩阵**（真 PostgreSQL）：
// 在既有「投递/双副本/SIGKILL 接管」证据之上，补齐演进评审点名的多副本
// 复杂面——租约续期、双活窗口（旧持有者回归）、uncertain 跨副本回传、
// 取消 vs 在途、优雅关闭排空、资源收尾幂等。
//
// 跳过边界与 pg_cluster_test.go 同口径：唯一的 skip 在环境入口（无 DSN），
// 场景本身硬失败。

// blockingGate 测试侧的放行闸：在途工具卡在这里，测试决定何时放行。
type blockingGate struct {
	entered chan struct{}
	proceed chan struct{}
}

func newBlockingGate() *blockingGate {
	return &blockingGate{
		entered: make(chan struct{}, 1),
		proceed: make(chan struct{}),
	}
}

// waitEntered 等工具进入执行体（超时即失败——进不去说明任务没被认领执行）。
func (g *blockingGate) waitEntered(t *testing.T) {
	t.Helper()
	select {
	case <-g.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("工具 10s 内未进入执行体（任务未被认领执行）")
	}
}

// signalEntered 非阻塞报到：同一工具可能被执行多次（被封口后由接管方
// 重跑），重复执行体进入不能 panic（close 已关通道会炸）。
func (g *blockingGate) signalEntered() {
	select {
	case g.entered <- struct{}{}:
	default:
	}
}

// TestClusterLeaseRenewsDuringLongExecution 租约续期：执行时长 > 2×TTL 的
// 长任务，持有者的续期节拍（TTL/3）必须一直顶着——竞争者在整个执行窗口内
// 任何一次认领都拿不到（ErrTaken）；执行完任务终态，副作用恰一次。
//
// 续期失灵的后果：租约中途过期 ⇒ 别的副本可认领 ⇒ 同一会话双执行。
func TestClusterLeaseRenewsDuringLongExecution(t *testing.T) {
	dsn := requireDSN(t)
	schema := testSchema(t, dsn)
	ctx := context.Background()

	var applied atomic.Int64
	gate := newBlockingGate()
	model := &fakeModel{errAt: -1, toolName: "慢活", final: "完成", stableID: true}
	tool := pipeline.Tool{
		Name: "慢活",
		Handler: func(ctx context.Context, _ []byte) (string, error) {
			applied.Add(1)
			gate.signalEntered()
			// 执行 2.5s（> 2×LeaseTTL=1s）：期间靠续期顶住，ctx-aware。
			select {
			case <-time.After(2500 * time.Millisecond):
				return "ok", nil
			case <-ctx.Done():
				return "", ctx.Err()
			}
		},
	}

	c, err := cluster.New(ctx, cluster.Config{
		Model:        model,
		Tools:        []pipeline.Tool{tool},
		Workers:      1,
		LeaseTTL:     time.Second,
		PollInterval: 50 * time.Millisecond,
		WaitTimeout:  30 * time.Second,
		Schema:       schema,
	}, dsn)
	if err != nil {
		t.Fatalf("装配 cluster 失败: %v", err)
	}
	defer func() { _ = c.Close() }()

	if err := c.Submit(ctx, "sess-renew", "跑个慢活"); err != nil {
		t.Fatalf("投递失败: %v", err)
	}
	if err := c.Start(ctx); err != nil {
		t.Fatalf("启动失败: %v", err)
	}
	gate.waitEntered(t)

	// 执行窗口内反复抢认领：续期顶着 ⇒ 每次都该被拒（ErrTaken）。
	deadline := time.Now().Add(2 * time.Second)
	attempts := 0
	for time.Now().Before(deadline) {
		attempts++
		if _, err := c.Board().Claim(ctx, "sess-renew", "rival",
			taskboard.ClaimRequest{TTL: time.Second}); err == nil {
			t.Fatalf("第 %d 次抢认领成功——续期失灵，长任务租约中途过期（双执行入口）", attempts)
		} else if !strings.Contains(err.Error(), "已被他人认领") {
			t.Fatalf("第 %d 次抢认领的拒绝形态不对（应是活持有被拒）: %v", attempts, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if attempts < 5 {
		t.Fatalf("抢认领尝试次数过少（%d），测不到续期窗口", attempts)
	}

	res, err := c.Wait(ctx, "sess-renew")
	if err != nil {
		t.Fatalf("等待结果失败: %v", err)
	}
	if res.Status != "succeeded" {
		t.Fatalf("状态不对: got=%q", res.Status)
	}
	if got := applied.Load(); got != 1 {
		t.Fatalf("副作用应恰一次，got %d", got)
	}
	// 执行完毕（终态）后，竞争者面对的是终态不是活持有。
	if _, err := c.Board().Claim(ctx, "sess-renew", "rival",
		taskboard.ClaimRequest{TTL: time.Second}); err == nil {
		t.Fatal("终态任务不应可认领")
	}
}

// TestClusterStaleHolderFinishRejectedAfterTakeover 双活窗口：旧持有者 A
// 租约过期、B 接管之后 A 带着旧令牌回来收场——必须被拒（ErrLeaseLost），
// B 的收场站立，事件面只有 B 的一条结果（旧执行者的结论不得对外可见）。
func TestClusterStaleHolderFinishRejectedAfterTakeover(t *testing.T) {
	dsn := requireDSN(t)
	schema := testSchema(t, dsn)
	ctx := context.Background()

	model := &fakeModel{errAt: -1, toolName: "计数器", final: "done", stableID: true}
	c, err := cluster.New(ctx, cluster.Config{
		Model:        model,
		Tools:        []pipeline.Tool{countingTool("计数器", &atomic.Int64{})},
		Workers:      1,
		LeaseTTL:     time.Second,
		PollInterval: 50 * time.Millisecond,
		WaitTimeout:  10 * time.Second,
		Schema:       schema,
	}, dsn)
	if err != nil {
		t.Fatalf("装配 cluster 失败: %v", err)
	}
	defer func() { _ = c.Close() }()

	if err := c.Submit(ctx, "sess-stale", "数一下"); err != nil {
		t.Fatalf("投递失败: %v", err)
	}

	// A 认领（TTL 1s）后「假死」：不收场，租约自然到期。
	taskA, err := c.Board().Claim(ctx, "sess-stale", "copy-a", taskboard.ClaimRequest{TTL: time.Second})
	if err != nil {
		t.Fatalf("A 认领失败: %v", err)
	}
	time.Sleep(1200 * time.Millisecond) // 越过 TTL

	// B 接管（新令牌、代次递增）。
	taskB, err := c.Board().Claim(ctx, "sess-stale", "copy-b", taskboard.ClaimRequest{TTL: time.Second})
	if err != nil {
		t.Fatalf("B 接管失败: %v", err)
	}
	if taskB.Lease.Fence <= taskA.Lease.Fence {
		t.Fatalf("接管代次必须递增: A=%d B=%d", taskA.Lease.Fence, taskB.Lease.Fence)
	}

	// A 带旧令牌回来 Finish（写自己的结果事件）——必须整批拒绝。
	err = finishWithResult(ctx, t, dsn, schema, "sess-stale", taskA.Lease.Token,
		`{"status":"succeeded","final":"A 的过期结论","rounds":1,"tool_calls":0}`)
	if err == nil {
		t.Fatal("旧持有者的 Finish 必须被拒（租约已易主）")
	}
	if !strings.Contains(err.Error(), "lease") && !strings.Contains(err.Error(), "租约") &&
		!strings.Contains(err.Error(), "令牌") {
		t.Fatalf("拒绝原因应指向租约失格，got: %v", err)
	}

	// B 正常收场——B 的结果站立。
	if err := finishWithResult(ctx, t, dsn, schema, "sess-stale", taskB.Lease.Token,
		`{"status":"succeeded","final":"B 的接管结论","rounds":1,"tool_calls":0}`); err != nil {
		t.Fatalf("B 的 Finish 应成功: %v", err)
	}

	// 事件面只有 B 的一条结果（A 的过期结论不得对外可见）。
	evs, err := c.Events().ListAfter(ctx, 0, 100)
	if err != nil {
		t.Fatalf("读事件面失败: %v", err)
	}
	var results []string
	for _, ev := range evs {
		if ev.SessionID == "sess-stale" && ev.Type == cluster.ResultEventType {
			raw, _ := ev.Payload["result"].(string)
			results = append(results, raw)
		}
	}
	if len(results) != 1 {
		t.Fatalf("结果事件应恰一条（B 的），got %d: %v", len(results), results)
	}
	if !strings.Contains(results[0], "B 的接管结论") {
		t.Fatalf("对外可见的应是 B 的结论，got: %s", results[0])
	}
}

// TestClusterUncertainResultReachesWaiter 工具结果未知（Safe 未声明 + 执行体
// 报错 ⇒ uncertain）必须经事件面回传给等待方：res.Uncertain 携带调用 ID，
// 禁止盲目重试的提示不丢。
func TestClusterUncertainResultReachesWaiter(t *testing.T) {
	dsn := requireDSN(t)
	schema := testSchema(t, dsn)
	ctx := context.Background()

	// Safe 不声明（零值 false）⇒ 最弱处理：施加失败即结果未知。
	flaky := pipeline.Tool{
		Name: "远端写",
		Handler: func(context.Context, []byte) (string, error) {
			return "", errRemoteUnknown
		},
	}
	model := &fakeModel{errAt: -1, toolName: "远端写", final: "已尽力", stableID: true}

	c, err := cluster.New(ctx, cluster.Config{
		Model:        model,
		Tools:        []pipeline.Tool{flaky},
		Workers:      1,
		LeaseTTL:     5 * time.Second,
		PollInterval: 50 * time.Millisecond,
		WaitTimeout:  30 * time.Second,
		Schema:       schema,
	}, dsn)
	if err != nil {
		t.Fatalf("装配 cluster 失败: %v", err)
	}
	defer func() { _ = c.Close() }()

	if err := c.Submit(ctx, "sess-unc", "写远端"); err != nil {
		t.Fatalf("投递失败: %v", err)
	}
	if err := c.Start(ctx); err != nil {
		t.Fatalf("启动失败: %v", err)
	}
	res, err := c.Wait(ctx, "sess-unc")
	if err != nil {
		t.Fatalf("等待结果失败: %v", err)
	}
	if len(res.Uncertain) == 0 {
		t.Fatalf("结果未知必须回传到 Uncertain（等待方要能判「先对账再重试」），got %+v", res)
	}
	if res.Uncertain[0] != stableCallID {
		t.Fatalf("Uncertain 应携带调用 ID，got %q", res.Uncertain[0])
	}
	if res.Status != "succeeded" {
		t.Fatalf("uncertain 不阻断终答（模型观察后收口），状态应 succeeded，got %q", res.Status)
	}
}

// errRemoteUnknown 远端是否生效无法确认的执行体错误（uncertain 触发源）。
var errRemoteUnknown = errString("远端无响应，是否生效未知")

type errString string

func (e errString) Error() string { return string(e) }

// finishWithResult 走 worker 同一条收场面（pgstore.Finish：任务终态 + 结果
// 事件同事务）——测试自己开一个指向同 schema 的 Store 来扮演任一副本的
// 收场动作（Cluster 不对外暴露 Finish，那是 worker 内部面）。
func finishWithResult(ctx context.Context, t *testing.T, dsn, schema, sessionID, token, resultJSON string) error {
	t.Helper()
	return adminStore(t, dsn, schema).Finish(ctx, pgstore.FinishRequest{
		TaskID: taskboard.TaskID(sessionID),
		Token:  token,
		Event: eventbus.Event{
			SessionID:  sessionID,
			Type:       cluster.ResultEventType,
			Level:      "info",
			Payload:    map[string]any{"result": resultJSON},
			OccurredAt: time.Now(),
		},
	})
}

// adminStore 指向同 schema 的独立存储面。Cluster 收尾（Close /
// CloseGraceful）之后它的连接池已关——收尾后的状态断言必须走这里，
// 拿已收尾实例的 Board 查只会得到 closed pool（拿错误当断言 = 假绿）。
func adminStore(t *testing.T, dsn, schema string) *pgstore.Store {
	t.Helper()
	pcfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("解析 DSN 失败: %v", err)
	}
	if pcfg.ConnConfig.RuntimeParams == nil {
		pcfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	pcfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(context.Background(), pcfg)
	if err != nil {
		t.Fatalf("连接 PG 失败: %v", err)
	}
	t.Cleanup(pool.Close)
	return pgstore.NewStore(pool)
}

// retryModel 施加未成功（contended 观察）时重试同一工具调用（稳定 ID），
// 拿到施加/重放观察或到达尝试上限才收口——模拟真实模型「按观察建议重试」，
// 让接管后的闸恢复窗口（SideEffectTTL）能在测试时长内走完。
type retryModel struct {
	toolName   string
	final      string
	maxRetries int
}

func (m *retryModel) Chat(_ context.Context, req chat.ChatRequest) (chat.ChatResponse, error) {
	toolResults := 0
	last := ""
	for _, msg := range req.Messages {
		if msg.Role == chat.RoleTool {
			toolResults++
			last = msg.Content
		}
	}
	if toolResults == 0 || (strings.Contains(last, "占用") && toolResults <= m.maxRetries) {
		return chat.ChatResponse{ToolCalls: []chat.ToolCall{{
			ID: stableCallID, Name: m.toolName, Arguments: "{}",
		}}}, nil
	}
	return chat.ChatResponse{Content: m.final}, nil
}

// TestClusterCloseDuringInFlightTaskRecoverable 取消 vs 在途：Close 封口
// 在途执行——执行体必须感知 ctx 取消并收场；Close 有界返回（不被在途卡死）；
// 任务不悬空（租约到期后可被新副本接管），新副本接管后**成功施加恰一次**
// （被封口的那次没走到施加完成；重试撞上同键只判 replayed 不再进执行体），
// 会话跑通终态。
//
// 已知缺口（本用例顺带钉住其边界）：取消路径上 Safe 失败的闸回收
// （Abandon）用已取消的 ctx 静默失败 ⇒ 被扣的闸要等 SideEffectTTL 才回
// 到可认领，接管方首轮可能拿到 contended——安全不变量不破（不重复施加），
// 但接管延迟下限是 TTL。根治在 kernel 侧（清理 ctx 必须 cancel-proof），
// 本用例把 SideEffectTTL 调短让窗口在测试时长内走完。
func TestClusterCloseDuringInFlightTaskRecoverable(t *testing.T) {
	dsn := requireDSN(t)
	schema := testSchema(t, dsn)
	ctx := context.Background()

	var entered, succeeded atomic.Int64
	gate := newBlockingGate()
	var canceledDuringBlock atomic.Bool
	tool := pipeline.Tool{
		Name: "卡住的活",
		Safe: true, // 幂等：被封口的尝试可安全重试
		Handler: func(ctx context.Context, _ []byte) (string, error) {
			entered.Add(1)
			gate.signalEntered()
			select {
			case <-gate.proceed:
				succeeded.Add(1)
				return "ok", nil
			case <-ctx.Done():
				canceledDuringBlock.Store(true)
				return "", ctx.Err()
			}
		},
	}

	c, err := cluster.New(ctx, cluster.Config{
		Model:         &fakeModel{errAt: -1, toolName: "卡住的活", final: "接力完成", stableID: true},
		Tools:         []pipeline.Tool{tool},
		Workers:       1,
		LeaseTTL:      time.Second,
		SideEffectTTL: time.Second, // 被扣闸的回收窗口 = 1s（默认 30s 测试走不完）
		PollInterval:  50 * time.Millisecond,
		WaitTimeout:   30 * time.Second,
		Schema:        schema,
	}, dsn)
	if err != nil {
		t.Fatalf("装配 cluster 失败: %v", err)
	}

	if err := c.Submit(ctx, "sess-cancel", "跑"); err != nil {
		t.Fatalf("投递失败: %v", err)
	}
	if err := c.Start(ctx); err != nil {
		t.Fatalf("启动失败: %v", err)
	}
	gate.waitEntered(t)

	// Close：封口在途。执行体 ctx-aware ⇒ Close 有界返回。
	closed := make(chan error, 1)
	go func() { closed <- c.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Close 失败: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Close 10s 未返回——在途封口没有生效（执行体没感知取消）")
	}
	if !canceledDuringBlock.Load() {
		t.Fatal("在途执行体应感知到 ctx 取消（Close 的封口语义）")
	}
	if succeeded.Load() != 0 {
		t.Fatalf("被封口的尝试不应施加成功，got %d", succeeded.Load())
	}

	// 任务不悬空：租约到期后竞争者可接管（与 SIGKILL 接管同一入口）。
	// 收尾后 c 的池已关 ⇒ 走独立存储面查（adminStore）。
	board := adminStore(t, dsn, schema).Board()
	var takeover taskboard.Task
	deadline := time.Now().Add(5 * time.Second)
	for {
		takeover, err = board.Claim(ctx, "sess-cancel", "copy-b",
			taskboard.ClaimRequest{TTL: 5 * time.Second})
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Close 后任务 5s 内未变得可接管（悬空）: %v", err)
		}
		time.Sleep(200 * time.Millisecond)
	}
	// 归还接管的认领，交给新副本 worker 干净地跑（避免与下面的副本抢同一单）。
	if err := board.Release(ctx, "sess-cancel", takeover.Lease.Token); err != nil {
		t.Fatalf("归还接管认领失败: %v", err)
	}

	// 新副本（同 schema、新实例）接管：重试撞上被扣的闸（contended），
	// 闸 TTL 到期后施加成功；同键再重试只判 replayed——执行体恰进一次。
	close(gate.proceed)
	c2, err := cluster.New(ctx, cluster.Config{
		Model:         &retryModel{toolName: "卡住的活", final: "接力完成", maxRetries: 8},
		Tools:         []pipeline.Tool{tool},
		Workers:       1,
		LeaseTTL:      5 * time.Second,
		SideEffectTTL: time.Second,
		PollInterval:  50 * time.Millisecond,
		WaitTimeout:   30 * time.Second,
		Schema:        schema,
	}, dsn)
	if err != nil {
		t.Fatalf("装配副本 B 失败: %v", err)
	}
	defer func() { _ = c2.Close() }()
	if err := c2.Start(ctx); err != nil {
		t.Fatalf("副本 B 启动失败: %v", err)
	}
	res, err := c2.Wait(ctx, "sess-cancel")
	if err != nil {
		t.Fatalf("等待接管后的结果失败: %v", err)
	}
	if res.Status != "succeeded" {
		t.Fatalf("接管后应跑通终态，got %q", res.Status)
	}
	if succeeded.Load() != 1 {
		t.Fatalf("成功施加应恰一次（重试只 replayed 不再进执行体），got %d", succeeded.Load())
	}
	if entered.Load() < 2 {
		t.Fatalf("执行体至少进过两次（被封口一次 + 接管一次），got %d", entered.Load())
	}
}

// TestClusterCloseGracefulDrainsInFlight 优雅关闭：CloseGraceful 不封口
// 在途——执行体继续跑到自然收场，结果照常回传；排空期间不领新活；返回
// nil 表示排干净了。
func TestClusterCloseGracefulDrainsInFlight(t *testing.T) {
	dsn := requireDSN(t)
	schema := testSchema(t, dsn)
	ctx := context.Background()

	var applied, canceled atomic.Int64
	gate := newBlockingGate()
	model := &fakeModel{errAt: -1, toolName: "在途活", final: "收场完成", stableID: true}
	tool := pipeline.Tool{
		Name: "在途活",
		Handler: func(ctx context.Context, _ []byte) (string, error) {
			applied.Add(1)
			gate.signalEntered()
			select {
			case <-gate.proceed:
				return "ok", nil
			case <-ctx.Done():
				canceled.Add(1) // 排空不得封口在途——这个分支出现即语义破
				return "", ctx.Err()
			}
		},
	}

	c, err := cluster.New(ctx, cluster.Config{
		Model:        model,
		Tools:        []pipeline.Tool{tool},
		Workers:      1,
		LeaseTTL:     5 * time.Second,
		PollInterval: 50 * time.Millisecond,
		WaitTimeout:  30 * time.Second,
		Schema:       schema,
	}, dsn)
	if err != nil {
		t.Fatalf("装配 cluster 失败: %v", err)
	}
	if err := c.Submit(ctx, "sess-drain", "跑"); err != nil {
		t.Fatalf("投递失败: %v", err)
	}
	if err := c.Start(ctx); err != nil {
		t.Fatalf("启动失败: %v", err)
	}
	gate.waitEntered(t)

	// 排空开始：在途未收场 ⇒ CloseGraceful 必须还等着（不封口、不返回）。
	drained := make(chan error, 1)
	gctx, gcancel := context.WithTimeout(ctx, 15*time.Second)
	defer gcancel()
	go func() { drained <- c.CloseGraceful(gctx) }()
	select {
	case err := <-drained:
		t.Fatalf("在途未收场就返回了排空（应等待）: err=%v", err)
	case <-time.After(500 * time.Millisecond):
		// 期望：仍在排空等待中。
	}
	if canceled.Load() != 0 {
		t.Fatalf("排空不得封口在途（执行体感知到取消 %d 次）", canceled.Load())
	}

	// 放行在途 ⇒ 自然收场 ⇒ 排空返回 nil。
	close(gate.proceed)
	select {
	case err := <-drained:
		if err != nil {
			t.Fatalf("排空应干净完成，got %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("放行后 10s 排空未完成")
	}
	if canceled.Load() != 0 {
		t.Fatalf("排空路径不得取消在途，canceled=%d", canceled.Load())
	}
	if applied.Load() != 1 {
		t.Fatalf("在途应恰执行一次，got %d", applied.Load())
	}

	// 收场后：任务终态（结果已回传）、再启动被拒（已关闭）、重复收尾幂等。
	// 收尾后池已关 ⇒ 终态断言走独立存储面，并且**必须**判到 ErrTerminal
	// （closed pool 的报错被当成「拒绝认领」就是假绿）。
	_, terr := adminStore(t, dsn, schema).Board().Claim(ctx, "sess-drain", "rival",
		taskboard.ClaimRequest{TTL: time.Second})
	if terr == nil {
		t.Fatal("排空收场后任务应是终态（结果已回传）：竟被认领")
	}
	if !errors.Is(terr, taskboard.ErrTerminal) {
		t.Fatalf("收场后应判 ErrTerminal，got: %v", terr)
	}
	if err := c.Start(ctx); err == nil {
		t.Fatal("已收尾实例不得再启动")
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close 幂等应返回 nil，got %v", err)
	}
	if err := c.CloseGraceful(ctx); err != nil {
		t.Fatalf("CloseGraceful 幂等应返回 nil，got %v", err)
	}
}

// TestClusterCloseGracefulTimeoutFallsBackToCancel 排空超时的回落语义：
// 在途死不收场 ⇒ ctx 到期后 CloseGraceful 回落取消（封口），并把「没排
// 干净」如实返回（不静默降级成成功）；任务留待接管。
func TestClusterCloseGracefulTimeoutFallsBackToCancel(t *testing.T) {
	dsn := requireDSN(t)
	schema := testSchema(t, dsn)
	ctx := context.Background()

	gate := newBlockingGate()
	var canceled atomic.Int64
	model := &fakeModel{errAt: -1, toolName: "死活", final: "x", stableID: true}
	tool := pipeline.Tool{
		Name: "死活",
		Handler: func(ctx context.Context, _ []byte) (string, error) {
			gate.signalEntered()
			<-ctx.Done() // 只认取消：排空的自然收场等不到它
			canceled.Add(1)
			return "", ctx.Err()
		},
	}

	c, err := cluster.New(ctx, cluster.Config{
		Model:        model,
		Tools:        []pipeline.Tool{tool},
		Workers:      1,
		LeaseTTL:     time.Second,
		PollInterval: 50 * time.Millisecond,
		WaitTimeout:  30 * time.Second,
		Schema:       schema,
	}, dsn)
	if err != nil {
		t.Fatalf("装配 cluster 失败: %v", err)
	}
	if err := c.Submit(ctx, "sess-gdrain", "跑"); err != nil {
		t.Fatalf("投递失败: %v", err)
	}
	if err := c.Start(ctx); err != nil {
		t.Fatalf("启动失败: %v", err)
	}
	gate.waitEntered(t)

	gctx, gcancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer gcancel()
	start := time.Now()
	err = c.CloseGraceful(gctx)
	if err == nil {
		t.Fatal("排空超时必须返回错误（没排干净不许静默成成功）")
	}
	if canceled.Load() != 1 {
		t.Fatalf("超时回落应封口在途（执行体感知取消一次），got %d", canceled.Load())
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("回落收尾应有界，耗时 %v", elapsed)
	}

	// 任务留待接管（与取消语义同口径；收尾后池已关 ⇒ 走独立存储面）。
	board := adminStore(t, dsn, schema).Board()
	deadline := time.Now().Add(5 * time.Second)
	for {
		task, cerr := board.Claim(ctx, "sess-gdrain", "copy-b",
			taskboard.ClaimRequest{TTL: time.Second})
		if cerr == nil {
			if rerr := board.Release(ctx, "sess-gdrain", task.Lease.Token); rerr != nil {
				t.Fatalf("归还失败: %v", rerr)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("超时收尾后任务应可接管，got: %v", cerr)
		}
		time.Sleep(200 * time.Millisecond)
	}
}
