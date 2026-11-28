// workflow —— Workflow Agent 入口样例：固定步骤、明确依赖、确定性流程。
//
// 七词词汇侧重：Step（固定步骤）——流程由本样例自己拼装（普通 Go 函数
// 顺序调用），只在需要语言能力的那一步嵌一个 Run。
//
// ⚠️ 边界（ADR-0010 / runtime#39）：真正的定步执行器（依赖图、恢复语义、
// 与 Loop 的组合面）是**条件立项**，正主在 #39——本样例只做**样例级拼装**，
// 不抢跑、不自造执行器。
//
//	CGO_ENABLED=0 go run ./examples/workflow
//
// 生命周期: Experimental（v0.10.0 定级基线；ADR-004 波次接入 api-lifecycle 清单）
package main

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/yi-nology/goagent-runtime/chat"
	"github.com/yi-nology/goagent-runtime/pipeline"
	"github.com/yi-nology/goagent-runtime/preset"
	"github.com/yi-nology/goagent-runtime/runtime"
)

// scriptModel 脚本化假模型。
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

	// 固定流程：Step 1 提取 → Step 2 校验 → Step 3 汇总（内嵌一次 Run）。
	// 依赖是顺序的、确定的——谁是下一步由代码说了算，不由模型说了算。

	// Step 1：提取（纯函数）。
	raw := "订单 #1024：A 仓 42 件，B 仓 7 件"
	order := extract(raw)
	fmt.Printf("Step 1 提取: %+v\n", order)

	// Step 2：校验（纯函数，确定性判定）。
	if order.qty <= 0 {
		log.Fatal("Step 2 校验失败：数量非法")
	}
	fmt.Printf("Step 2 校验: 通过（qty=%d）\n", order.qty)

	// Step 3：汇总（固定流程里嵌一个 Run——语言能力只出现在这一步）。
	// 定步骨架是宿主的代码（真定步执行器归 runtime#39，本样例不抢跑）；
	// 这一步的装配→会话→Run→Close 走 preset.Simple（与 simple 形态互为锚）。
	cfg := runtime.Config{
		Model: &scriptModel{steps: []chat.ChatResponse{
			{Content: "订单 #1024 校验通过：A 仓 42 件，可直接履约"},
		}},
		Tools: []pipeline.Tool{{
			Name: "noop", Handler: func(context.Context, []byte) (string, error) { return "", nil },
		}},
	}

	res, err := preset.Simple(ctx, cfg, "workflow", "一句话汇总这份订单："+raw)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("Step 3 汇总（Run）: %s\n", res.Final)
	fmt.Println("流程完成：3 个 Step，1 个 Run")
}

// order 提取结果（样例数据结构）。
type order struct {
	id  string
	qty int
}

// extract Step 1：从原文提取订单（样例用纯字符串处理）。
func extract(raw string) order {
	o := order{}
	if i := strings.Index(raw, "#"); i >= 0 {
		o.id = raw[i:]
		if j := strings.Index(o.id, "："); j >= 0 {
			o.id = o.id[:j]
		}
	}
	if i := strings.Index(raw, "A 仓 "); i >= 0 {
		rest := raw[i+len("A 仓 "):]
		fmt.Sscanf(rest, "%d", &o.qty)
	}
	return o
}
