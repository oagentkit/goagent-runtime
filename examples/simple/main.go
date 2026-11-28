// simple —— Simple Agent 入口样例：模型 + 工具调用，最小闭环。
//
// 七词词汇侧重：Run（一次执行）。
//
//	CGO_ENABLED=0 go run ./examples/simple
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/yi-nology/goagent-runtime/chat"
	"github.com/yi-nology/goagent-runtime/pipeline"
	"github.com/yi-nology/goagent-runtime/runtime"
)

// scriptModel 脚本化假模型（demo 不带真模型供应商）。
type scriptModel struct {
	steps  []chat.ChatResponse
	called int
}

func (m *scriptModel) Chat(_ context.Context, _ chat.ChatRequest) (chat.ChatResponse, error) {
	i := m.called
	m.called++
	if i < len(m.steps) {
		return m.steps[i], nil
	}
	return chat.ChatResponse{Content: "（剧本耗尽）"}, nil
}

func main() {
	ctx := context.Background()

	// 1) 装配：一个模型 + 一个工具。
	rt, err := runtime.NewEmbedded(runtime.Config{
		Model: &scriptModel{steps: []chat.ChatResponse{
			{ToolCalls: []chat.ToolCall{{ID: "c1", Name: "lookup", Arguments: `{"sku":"A"}`}}},
			{Content: "A 仓：42 件"},
		}},
		Tools: []pipeline.Tool{{
			Name:        "lookup",
			Description: "查库存",
			Handler: func(context.Context, []byte) (string, error) {
				return "A 仓：42 件", nil
			},
		}},
	})
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = rt.Close() }()

	// 2) 打开会话，跑一个 Run。
	session, err := rt.NewSession(ctx, runtime.SessionOptions{ID: "simple"})
	if err != nil {
		log.Fatal(err)
	}
	res, err := session.Run(ctx, "查一下 A 仓库存")
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("Run: status=%s rounds=%d toolCalls=%d\n", res.Status, res.Rounds, res.ToolCalls)
	fmt.Printf("终答: %s\n", res.Final)
}
