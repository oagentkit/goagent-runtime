// multiagent —— Multi-Agent 入口样例：多专家协作（黑板轮转 + 派发守卫）。
//
// 七词词汇侧重：Handoff（coordinator → 专家的派发边由 dispatch.Guard
// 白名单守卫）+ Run（每位专家的分析各是一个会话执行）。
//
// 装配的上游原语（均 eino-free——主模块零 LLM SDK 边界，#46 硬红线）：
//   - oagentkit/agent/blackboard：共享黑板 + Convene 多专家轮转协作；
//   - oagentkit/agent/dispatch：allow 矩阵 + 深度上限的派发守卫。
//
// 专家会话用 SessionOptions 会话级覆盖（System/Tools 各自一套，互不泄漏）
// ——W4 配置分层的直接消费；模型面按会话的 System 提示分流到各自剧本。
//
//	CGO_ENABLED=0 go run ./examples/multiagent
//
// 生命周期: Experimental（v0.10.0 定级基线；ADR-004 波次接入 api-lifecycle 清单）
package main

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/oagentkit/agent/blackboard"
	"github.com/oagentkit/agent/dispatch"

	"github.com/yi-nology/goagent-runtime/chat"
	"github.com/yi-nology/goagent-runtime/pipeline"
	"github.com/yi-nology/goagent-runtime/preset"
	"github.com/yi-nology/goagent-runtime/runtime"
)

// expertModel 按会话 System 提示分流的假模型：库存专家与文案专家各走各的剧本
// （runtime 级 Model 是装配必填、会话不覆盖模型面——分流发生在模型适配层）。
type expertModel struct {
	inv   []chat.ChatResponse
	copy  []chat.ChatResponse
	calls map[string]int // 专家剧本键 → 已调用次数
}

func (m *expertModel) Chat(_ context.Context, req chat.ChatRequest) (chat.ChatResponse, error) {
	key := "copy"
	for _, msg := range req.Messages {
		if msg.Role == chat.RoleSystem && strings.Contains(msg.Content, "库存") {
			key = "inv"
			break
		}
	}
	if m.calls == nil {
		m.calls = map[string]int{}
	}
	i := m.calls[key]
	m.calls[key]++
	steps := m.inv
	if key == "copy" {
		steps = m.copy
	}
	if i < len(steps) {
		return steps[i], nil
	}
	return chat.ChatResponse{Content: "（无补充）"}, nil
}

// edges dispatch.EdgeSource：coordinator 可 Handoff 给两位专家（编译期拓扑）。
type edges struct{}

func (edges) Slugs() []string { return []string{"coordinator"} }

func (edges) DispatchAllow(slug string) []string {
	if slug == "coordinator" {
		return []string{"库存专家", "文案专家"}
	}
	return nil
}

func main() {
	ctx := context.Background()

	cfg := runtime.Config{
		Model: &expertModel{
			inv: []chat.ChatResponse{
				{Content: "库存核对：42 件与台账一致，「充足」结论成立"},
				{Content: "库存侧无补充"},
			},
			copy: []chat.ChatResponse{
				{Content: "文案建议：数量带单位「42 件」，避免裸数字"},
				{Content: "文案侧无补充"},
			},
		},
		Tools: []pipeline.Tool{{
			Name: "noop", Handler: func(context.Context, []byte) (string, error) { return "", nil },
		}},
	}

	// Handoff 判据：coordinator → 专家 必须在 allow 矩阵里（maxDepth=1 =
	// 允许一层派发，coordinator 位于深度 0）。拓扑是宿主的编译期事实——
	// 守卫运行期只读，拒绝即 *DenyError 如实上报、不转述不降级。
	guard := dispatch.NewGuard(edges{}, 1)
	for _, expert := range []string{"库存专家", "文案专家"} {
		if err := guard.Assert("coordinator", expert, 0); err != nil {
			log.Fatalf("Handoff 被拒: %v", err)
		}
	}

	// 共享黑板：起草人先放材料。
	board := blackboard.NewBoard()
	board.Seed("material", "发布说明草稿：A 仓库存 42 件，充足，可立即发货。")

	// 两位专家 = 两份装配（会话级 System/工具面互不泄漏）——「无补充」这类
	// 内容谓词是宿主词表，经 Filter 注入（preset 不解释语义）。
	experts := []preset.Expert{
		{
			ID:     "expert-inventory",
			Name:   "库存专家",
			System: "你是库存专家：只核对数量与库存结论，输出一条核对发现。",
			Tools: []pipeline.Tool{{Name: "noop",
				Handler: func(context.Context, []byte) (string, error) { return "", nil }}},
			Filter: func(final string) bool { return !strings.Contains(final, "无补充") },
		},
		{
			ID:     "expert-copy",
			Name:   "文案专家",
			System: "你是文案专家：只审表述与单位规范，输出一条修改建议。",
			Tools: []pipeline.Tool{{Name: "noop",
				Handler: func(context.Context, []byte) (string, error) { return "", nil }}},
			Filter: func(final string) bool { return !strings.Contains(final, "无补充") },
		},
	}

	// 会话装配 + 贡献闭包 + Convene 轮转由 preset.MultiAgent 收口（互为锚）；
	// 轮次用 preset 默认（=2，与本样例原口径一致）。
	result, err := preset.MultiAgent(ctx, cfg, experts, board, nil)
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("Convene: rounds=%d entries=%d\n", result.Rounds, result.Total)
	for _, e := range board.Entries() {
		fmt.Printf("  [%d] %s/%s: %s\n", e.Seq, e.Author, e.Kind, e.Content)
	}
}
