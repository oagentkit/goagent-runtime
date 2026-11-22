package cluster_test

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/oekit/pkg/errors"

	"github.com/yi-nology/goagent-kernel/sideeffect"
	"github.com/yi-nology/goagent-kernel/taskboard"

	"github.com/yi-nology/goagent-runtime/chat"
	"github.com/yi-nology/goagent-runtime/cluster"
	"github.com/yi-nology/goagent-runtime/pipeline"
)

// 本文件是 cluster 模板的**高可用证据**（真 PostgreSQL，非替身）：
//
//   - TestClusterRunEndToEnd      投递 → worker 执行 → 结果经事件面回传；
//   - TestClusterTwoCopiesOneExecution 两个**独立副本**（各自连接池、各自
//     worker 池）抢同一个会话 ⇒ 工具只被施加一次（认领原子性 + 幂等闸）；
//   - TestClusterTakeoverNoDoubleApply 副本 A 领了任务却**没有收场**（模拟
//     进程被 SIGKILL）⇒ 租约到期后副本 B 接管 ⇒ 已施加的副作用**不再施加**。
//
// # 跳过与硬失败的边界
//
// 场景本身硬失败；唯一的 skip 在环境入口：没有可连的 PostgreSQL 时无从
// 构造被测实现（不伪造结果）。CI 的 integration job 必跑本文件。
const dsnEnv = "GAK_PGREF_DSN"

// requireDSN 测试用 DSN；缺失即显式跳过（写明原因与起法）。
func requireDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv(dsnEnv)
	if dsn == "" {
		t.Skipf("%s 未设置：cluster 高可用验证需要 PostgreSQL（不会在无库时伪造结果）。"+
			"起法：make integration，或 docker compose -f examples/docker-compose.yml up -d postgres 后"+
			" %s=postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable go test ./cluster",
			dsnEnv, dsnEnv)
	}
	return dsn
}

// testSchema 为每个用例建独立 schema（隔离：并发用例互不串味）。
func testSchema(t *testing.T, dsn string) string {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("连接 PostgreSQL 失败: %v", err)
	}
	defer pool.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("PostgreSQL 不可达: %v", err)
	}
	schema := fmt.Sprintf("gak_cluster_%d", time.Now().UnixNano())
	if _, err := pool.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatalf("建 schema 失败: %v", err)
	}
	t.Cleanup(func() {
		pool2, err2 := pgxpool.New(context.Background(), dsn)
		if err2 != nil {
			return
		}
		defer pool2.Close()
		_, _ = pool2.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+schema+" CASCADE")
	})
	return schema
}

// fakeModel 合成模型面：第一次请求工具，之后给终答。
//
// errAt 语义（与本仓约定一致）：-1 = 不注入错误；>= 0 = 该次调用（0 基）
// 报错。**零值 0 会让首次调用就报错**——不显式设 -1 就是注入错误。
type fakeModel struct {
	mu       sync.Mutex
	calls    int
	errAt    int
	toolName string
	final    string
	// stableID 让模型给出稳定的工具调用 ID（模拟供应商复述同一 ID）。
	stableID bool
}

// stableCallID 稳定工具调用 ID（模型复述同一个 ID ⇒ 接管重跑时同键）。
const stableCallID = "stable-call-1"

// stableCallID 稳定工具调用 ID（模型复述同一个 ID）。

func (m *fakeModel) Chat(ctx context.Context, req chat.ChatRequest) (chat.ChatResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	idx := m.calls
	m.calls++
	if m.errAt >= 0 && idx == m.errAt {
		return chat.ChatResponse{}, errors.New("fake: 注入的模型错误")
	}
	// 历史里已有工具结果 ⇒ 模型给终答（真实模型的常态）。
	for _, msg := range req.Messages {
		if msg.Role == chat.RoleTool {
			return chat.ChatResponse{Content: m.final}, nil
		}
	}
	id := ""
	if m.stableID {
		id = stableCallID
	}
	return chat.ChatResponse{ToolCalls: []chat.ToolCall{{
		ID: id, Name: m.toolName, Arguments: "{}",
	}}}, nil
}

// countingTool 记录施加次数的执行体（跨副本共享同一个计数器）。
func countingTool(name string, applied *atomic.Int64) pipeline.Tool {
	return pipeline.Tool{
		Name: name,
		Handler: func(ctx context.Context, args []byte) (string, error) {
			applied.Add(1)
			return "ok", nil
		},
	}
}

// TestClusterRunEndToEnd 投递 → 执行 → 结果回传。
func TestClusterRunEndToEnd(t *testing.T) {
	dsn := requireDSN(t)
	schema := testSchema(t, dsn)
	ctx := context.Background()

	var applied atomic.Int64
	c, err := cluster.New(ctx, cluster.Config{
		Model:  &fakeModel{errAt: -1, toolName: "计数器", final: "共 1 次"},
		Tools:  []pipeline.Tool{countingTool("计数器", &applied)},
		Workers: 2,
		Schema:  schema,
	}, dsn)
	if err != nil {
		t.Fatalf("装配 cluster 失败: %v", err)
	}
	defer c.Close()
	if err := c.Start(ctx); err != nil {
		t.Fatalf("启动 worker 池失败: %v", err)
	}

	res, err := c.Run(ctx, "sess-e2e", "数一下")
	if err != nil {
		t.Fatalf("cluster.Run 失败: %v", err)
	}
	if res.Status != "succeeded" {
		t.Fatalf("状态不对：got=%q want=succeeded", res.Status)
	}
	if res.Final != "共 1 次" {
		t.Fatalf("终答不对：got=%q", res.Final)
	}
	if got := applied.Load(); got != 1 {
		t.Fatalf("工具施加次数不对：got=%d want=1", got)
	}
}

// TestClusterTwoCopiesOneExecution 两个独立副本抢同一个会话 ⇒ 只执行一次。
func TestClusterTwoCopiesOneExecution(t *testing.T) {
	dsn := requireDSN(t)
	schema := testSchema(t, dsn)
	ctx := context.Background()

	var applied atomic.Int64
	model := &fakeModel{errAt: -1, toolName: "计数器", final: "共 1 次"}

	newCopy := func() *cluster.Cluster {
		c, err := cluster.New(ctx, cluster.Config{
			Model:   model,
			Tools:   []pipeline.Tool{countingTool("计数器", &applied)},
			Workers: 2,
			Schema:  schema,
		}, dsn)
		if err != nil {
			t.Fatalf("装配副本失败: %v", err)
		}
		if err := c.Start(ctx); err != nil {
			t.Fatalf("启动副本 worker 失败: %v", err)
		}
		return c
	}
	a := newCopy()
	defer a.Close()
	b := newCopy()
	defer b.Close()

	if err := a.Submit(ctx, "sess-two", "数一下"); err != nil {
		t.Fatalf("投递失败: %v", err)
	}
	// 结果由 b 侧等（证明结果确实经事件面跨副本回传）。
	res, err := b.Wait(ctx, "sess-two")
	if err != nil {
		t.Fatalf("跨副本等待结果失败: %v", err)
	}
	if res.Status != "succeeded" {
		t.Fatalf("状态不对：got=%q", res.Status)
	}
	if got := applied.Load(); got != 1 {
		t.Fatalf("两副本抢同一会话时工具被施加 %d 次（want=1：认领原子 + 幂等闸）", got)
	}
}

// TestClusterTakeoverNoDoubleApply **崩溃接管不重复施加**（cluster 形态的
// 核心承诺）：
//
// 现场：副本 A 认领了任务、**已经把副作用施加下去**，然后进程被 SIGKILL
// （既没释放租约也没收场）⇒ 租约到期后副本 B 接管同一任务并重跑 ⇒ 模型
// 再次请求同一个工具 ⇒ 副作用闸按**同一幂等键**判 replayed ⇒ 执行体
// 不会被第二次调用。
//
// 这里刻意让模型每次都请求同一个工具（不因历史里有结果就收手）——
// 接管重跑时它一定会再次请求 ⇒ 幂等键是否稳定成了唯一的防线。
func TestClusterTakeoverNoDoubleApply(t *testing.T) {
	dsn := requireDSN(t)
	schema := testSchema(t, dsn)
	ctx := context.Background()

	var applied atomic.Int64
	model := &fakeModel{errAt: -1, toolName: "计数器", final: "共 1 次", stableID: true}

	c, err := cluster.New(ctx, cluster.Config{
		Model:        model,
		Tools:        []pipeline.Tool{countingTool("计数器", &applied)},
		Workers:      1,
		LeaseTTL:     time.Second,
		PollInterval: 100 * time.Millisecond,
		WaitTimeout:  30 * time.Second,
		Schema:       schema,
	}, dsn)
	if err != nil {
		t.Fatalf("装配 cluster 失败: %v", err)
	}
	defer c.Close()

	// 副本 A：投递 + 认领（模拟「领了活就死了」）。
	if err := c.Submit(ctx, "sess-crash", "数一下"); err != nil {
		t.Fatalf("投递失败: %v", err)
	}
	_, err = c.Board().Claim(ctx, "sess-crash", "copy-a", taskboard.ClaimRequest{TTL: time.Second})
	if err != nil {
		t.Fatalf("副本 A 认领失败: %v", err)
	}

	// 副本 A 在崩溃前已把副作用施加下去（同幂等键、同资源）。
	runner := sideeffect.NewRunner(c.Effects(), 30*time.Second)
	outcome, _, err := runner.Execute(ctx, sideeffect.Action{
		Key:      sideeffect.Key(stableCallID),
		Resource: "sess-crash/" + model.toolName,
		Payload:  []byte("{}"),
	}, func(ctx context.Context, lease sideeffect.Lease) (sideeffect.Result, error) {
		applied.Add(1)
		return sideeffect.Result{Payload: []byte("ok")}, nil
	})
	if err != nil {
		t.Fatalf("副本 A 施加副作用失败: %v", err)
	}
	if outcome != sideeffect.OutcomeApplied {
		t.Fatalf("副本 A 的副作用未施加：outcome=%v", outcome)
	}
	// 副本 A 此刻被 SIGKILL：不释放、不完成（租约自然到期）。

	// 副本 B：worker 接管（租约到期后自动认领）并重跑。
	if err := c.Start(ctx); err != nil {
		t.Fatalf("启动副本 B 的 worker 失败: %v", err)
	}
	res, err := c.Wait(ctx, "sess-crash")
	if err != nil {
		t.Fatalf("等待接管后的结果失败: %v", err)
	}
	if res.Status != "succeeded" {
		t.Fatalf("状态不对：got=%q", res.Status)
	}
	if got := applied.Load(); got != 1 {
		t.Fatalf("接管后副作用被重复施加：施加 %d 次（want=1：闸按同一幂等键判 replayed）", got)
	}
}
