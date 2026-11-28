// react —— ReAct Agent 入口样例：自主决策循环（模型决定下一步）。
//
// 七词词汇侧重：Run / Turn / ToolCall——一个 Run 里多个 Turn，
// 每个 Turn 由模型自主决定调不调工具、调哪个。
//
//	CGO_ENABLED=0 go run ./examples/react
//
// 生命周期: Experimental（v0.10.0 定级基线；ADR-004 波次接入 api-lifecycle 清单）
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/yi-nology/goagent-runtime/chat"
	"github.com/yi-nology/goagent-runtime/pipeline"
	"github.com/yi-nology/goagent-runtime/preset"
	"github.com/yi-nology/goagent-runtime/runtime"
)

// scriptModel 脚本化假模型：按剧本逐 Turn 前进（Turn 1 要查库、Turn 2 要换算、
// Turn 3 收口）——模拟「观察 → 再决策」的自主循环。
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

	cfg := runtime.Config{
		Model: &scriptModel{steps: []chat.ChatResponse{
			// Turn 1：模型自主决定先查库存。
			{ToolCalls: []chat.ToolCall{{ID: "c1", Name: "lookup", Arguments: `{"sku":"A"}`}}},
			// Turn 2：观察结果后自主决定再换算单位。
			{ToolCalls: []chat.ToolCall{{ID: "c2", Name: "toCartons", Arguments: `{"units":42}`}}},
			// Turn 3：信息齐了，收口。
			{Content: "A 仓 42 件 = 3 箱（14 件/箱），库存充足"},
		}},
		Tools: []pipeline.Tool{
			{Name: "lookup", Description: "查库存", Handler: func(context.Context, []byte) (string, error) {
				return "42", nil
			}},
			{Name: "toCartons", Description: "件换箱", Handler: func(context.Context, []byte) (string, error) {
				return "3 箱", nil
			}},
		},
	}

	// 多轮工具循环：ReAct 形态的额度口径（零值 Budget 补默认上限）由 preset 给。
	res, err := preset.ReAct(ctx, cfg, "react", "A 仓库存够不够发一车？")
	if err != nil {
		log.Fatal(err)
	}

	// Run 的 Turn/ToolCall 计数：自主循环走了几轮、调了几次工具。
	fmt.Printf("Run: status=%s turns=%d toolCalls=%d\n", res.Status, res.Rounds, res.ToolCalls)
	fmt.Printf("终答: %s\n", res.Final)
}
