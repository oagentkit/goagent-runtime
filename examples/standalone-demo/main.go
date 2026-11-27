// standalone-demo —— standalone 模板演示：真 SQLite 持久化。
//
// 「进程 1」跑完一个工具回合后 Close（库文件落盘）；「进程 2」打开同一
// 库文件、同一 sessionID 续跑——历史从 SQLite 恢复，不丢上下文。
//
//	CGO_ENABLED=0 go run ./examples/standalone-demo
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/yi-nology/goagent-runtime/chat"
	"github.com/yi-nology/goagent-runtime/pipeline"
	"github.com/yi-nology/goagent-runtime/runtime"
)

// scriptModel 脚本化假模型。
type scriptModel struct {
	mu    sync.Mutex
	step  int
	steps []chat.ChatResponse
}

func (m *scriptModel) Chat(_ context.Context, req chat.ChatRequest) (chat.ChatResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	hist := len(req.Messages)
	if m.step < len(m.steps) {
		out := m.steps[m.step]
		m.step++
		fmt.Printf("  [模型] 第 %d 次调用（历史 %d 条）\n", m.step, hist)
		return out, nil
	}
	return chat.ChatResponse{Content: "（剧本耗尽）"}, nil
}

func main() {
	if err := runDemo(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, "演示失败:", err)
		os.Exit(1)
	}
}

func runDemo(ctx context.Context) error {
	dir, err := os.MkdirTemp("", "goagent-runtime-demo-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	dbPath := filepath.Join(dir, "demo.db")

	tools := []pipeline.Tool{{
		Name: "echo", Description: "回显工具",
		Handler: func(_ context.Context, args []byte) (string, error) {
			return "echo: " + string(args), nil
		},
	}}

	fmt.Println("── 进程 1 ──")
	rt1, err := runtime.NewStandalone(runtime.Config{
		Model: &scriptModel{steps: []chat.ChatResponse{
			{ToolCalls: []chat.ToolCall{{ID: "call-1", Name: "echo", Arguments: "{\"msg\":\"hi\"}"}}},
			{Content: "第一段完成"},
		}},
		Tools: tools,
	}, dbPath)
	if err != nil {
		return err
	}
	s1, err := rt1.NewSession(ctx, runtime.SessionOptions{ID: "demo-session"})
	if err != nil {
		return err
	}
	res1, err := s1.Run(ctx, "开始")
	if err != nil {
		return err
	}
	fmt.Println("进程 1:", res1.Status, "-", res1.Final)
	if err := rt1.Close(); err != nil {
		return err
	}
	fmt.Println("库文件已落盘:", dbPath)

	fmt.Println("── 进程 2（新开库，同一 sessionID 续跑） ──")
	rt2, err := runtime.NewStandalone(runtime.Config{
		Model: &scriptModel{steps: []chat.ChatResponse{
			{Content: "我记得之前调过 echo 了，历史还在"},
		}},
		Tools: tools,
	}, dbPath)
	if err != nil {
		return err
	}
	defer func() { _ = rt2.Close() }()
	s2, err := rt2.NewSession(ctx, runtime.SessionOptions{ID: "demo-session"})
	if err != nil {
		return err
	}
	res2, err := s2.Run(ctx, "接着来")
	if err != nil {
		return err
	}
	fmt.Println("进程 2:", res2.Status, "-", res2.Final)
	fmt.Println("模型第二次进程拿到的历史条数包含进程 1 的完整记录（见上方 [模型] 日志）")
	return nil
}
