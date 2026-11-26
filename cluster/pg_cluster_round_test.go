package cluster_test

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/yi-nology/goagent-runtime/chat"
	"github.com/yi-nology/goagent-runtime/cluster"
	"github.com/yi-nology/goagent-runtime/pipeline"
)

// roundModel 按 Chat 调用次序给出可区分的终答（第 N 次 Chat ⇒ 「chat-N」）。
// 用途：让「第 1 轮」与「第 2 轮」的结果在文本上可区分——等待方若把上一轮
// 的结果当本轮返回，断言即红。
type roundModel struct {
	mu    sync.Mutex
	calls int
}

func (m *roundModel) Chat(_ context.Context, req chat.ChatRequest) (chat.ChatResponse, error) {
	m.mu.Lock()
	m.calls++
	n := m.calls
	m.mu.Unlock()
	// 历史里已有工具结果 ⇒ 模型给终答（真实模型的常态）。
	for _, msg := range req.Messages {
		if msg.Role == chat.RoleTool {
			return chat.ChatResponse{Content: fmt.Sprintf("chat-%d", n)}, nil
		}
	}
	return chat.ChatResponse{ToolCalls: []chat.ToolCall{{
		ID: fmt.Sprintf("call-%d", n), Name: "计数器", Arguments: "{}",
	}}}, nil
}

// TestClusterSecondRoundReturnsOwnResult 同一会话连续投递两轮：第二轮必须
// 拿到**本轮**的结果，不能把第一轮的结果当本轮的返回。
//
// 缺陷形态（修复前）：结果事件按 sessionID 过滤，而 sessionID 是**长期
// 会话身份**（两轮共用一个 ID）⇒ 等待方从事件流开头扫，先撞上第 1 轮的
// 结果并立刻返回——第二轮拿到的是上一轮的结论。
func TestClusterSecondRoundReturnsOwnResult(t *testing.T) {
	dsn := requireDSN(t)
	schema := testSchema(t, dsn)
	ctx := context.Background()

	var applied atomic.Int64
	c, err := cluster.New(ctx, cluster.Config{
		Model:   &roundModel{},
		Tools:   []pipeline.Tool{countingTool("计数器", &applied)},
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

	res1, err := c.Run(ctx, "sess-round", "第一轮")
	if err != nil {
		t.Fatalf("第一轮 Run 失败: %v", err)
	}
	if res1.Final != "chat-2" {
		t.Fatalf("第一轮终答不对：got=%q want=chat-2", res1.Final)
	}

	res2, err := c.Run(ctx, "sess-round", "第二轮")
	if err != nil {
		t.Fatalf("第二轮 Run 失败: %v", err)
	}
	if res2.Final == res1.Final {
		t.Fatalf("第二轮拿到的不是本轮结果（与第一轮相同）: %q", res2.Final)
	}
	if res2.Final != "chat-3" {
		t.Fatalf("第二轮终答不对：got=%q want=chat-3", res2.Final)
	}
}
