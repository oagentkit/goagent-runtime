// embedded-demo —— embedded 模板演示：零外部依赖跑通「模型 ↔ 工具」回合链，
// 并演示进程重启后的恢复语义（共享存储的两个 Runtime 接力，副作用不重放）。
//
// 模型是脚本化假模型（demo 不带真模型供应商）；工具是一个计数器，
// 「施加次数」就是「副作用发生次数」——恢复后它必须仍是 1。
//
//	CGO_ENABLED=0 go run ./examples/embedded-demo
package main

import (
	"context"
	"fmt"
	"os"
	"sync"

	"github.com/yi-nology/goagent-runtime/chat"
	"github.com/yi-nology/goagent-runtime/pipeline"
	"github.com/yi-nology/goagent-runtime/runtime"
	"github.com/yi-nology/goagent-runtime/storage"
)

// scriptModel 脚本化假模型：每次 Chat 按剧本前进一步；errAt >= 0 时
// 该步返回错误（演示「进程死在模型调用上」）。
type scriptModel struct {
	mu    sync.Mutex
	step  int
	errAt int
	steps []chat.ChatResponse
}

func (m *scriptModel) Chat(_ context.Context, _ chat.ChatRequest) (chat.ChatResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	i := m.step
	m.step++
	if m.errAt >= 0 && i == m.errAt {
		return chat.ChatResponse{}, fmt.Errorf("进程死亡（剧本注入）")
	}
	if i < len(m.steps) {
		return m.steps[i], nil
	}
	return chat.ChatResponse{Content: "（剧本耗尽）"}, nil
}

// calc 计数工具：施加次数即副作用次数。
type calc struct {
	mu    sync.Mutex
	calls int
}

func (c *calc) handler(_ context.Context, _ []byte) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	return fmt.Sprintf("第 %d 次施加", c.calls), nil
}

func (c *calc) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

func main() {
	if err := runDemo(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, "演示失败:", err)
		os.Exit(1)
	}
}

func runDemo(ctx context.Context) error {
	mem := storage.NewMem()
	c := &calc{}
	tools := []pipeline.Tool{{Name: "calc", Description: "计数工具", Safe: true, Handler: c.handler}}

	// 进程 1：第一回合请求工具并施加，第二回合「进程死亡」（errAt 注入）。
	fmt.Println("── 进程 1 ──")
	rt1, err := runtime.NewEmbedded(runtime.Config{
		Model: &scriptModel{errAt: 1, steps: []chat.ChatResponse{
			{ToolCalls: []chat.ToolCall{{ID: "call-1", Name: "calc", Arguments: "{}"}}},
		}},
		Tools: tools,
	}, runtime.WithStorage(mem))
	if err != nil {
		return err
	}
	s1, sErr := rt1.NewSession(ctx, runtime.SessionOptions{ID: "demo-session"})
	if sErr != nil {
		_ = rt1.Close()
		return sErr
	}
	_, runErr := s1.Run(ctx, "帮我执行一次计数")
	_ = rt1.Close()
	fmt.Println("进程 1 在第二轮模型调用前死亡，Run 错误:", runErr)
	fmt.Println("此刻副作用施加次数:", c.count(), "（应为 1）")

	// 进程 2：全新 Runtime，共享同一存储。假模型按恢复的历史重放同一调用。
	fmt.Println("── 进程 2（恢复） ──")
	rt2, err := runtime.NewEmbedded(runtime.Config{
		Model: &scriptModel{errAt: -1, steps: []chat.ChatResponse{
			{ToolCalls: []chat.ToolCall{{ID: "call-1", Name: "calc", Arguments: "{}"}}},
			{Content: "恢复完成：计数结果已在上次施加中拿到"},
		}},
		Tools: tools,
	}, runtime.WithStorage(mem))
	if err != nil {
		return err
	}
	defer func() { _ = rt2.Close() }()
	s2, err := rt2.NewSession(ctx, runtime.SessionOptions{ID: "demo-session"})
	if err != nil {
		return err
	}
	res, err := s2.Run(ctx, "继续")
	if err != nil {
		return err
	}
	fmt.Println("恢复后状态:", res.Status)
	fmt.Println("终答:", res.Final)
	fmt.Println("副作用施加次数:", c.count(), "（恢复重放不得重复施加，仍应为 1）")
	return nil
}
