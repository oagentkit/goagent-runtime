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

	rt, err := runtime.NewEmbedded(runtime.Config{
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
	})
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = rt.Close() }()

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

	// 两位专家 = 两个会话（会话级覆盖：各自的 System 与工具面，互不泄漏）。
	inv, err := rt.NewSession(ctx, runtime.SessionOptions{
		ID:     "expert-inventory",
		System: "你是库存专家：只核对数量与库存结论，输出一条核对发现。",
		Tools: []pipeline.Tool{{Name: "noop",
			Handler: func(context.Context, []byte) (string, error) { return "", nil }}},
	})
	if err != nil {
		log.Fatal(err)
	}
	copyExpert, err := rt.NewSession(ctx, runtime.SessionOptions{
		ID:     "expert-copy",
		System: "你是文案专家：只审表述与单位规范，输出一条修改建议。",
		Tools: []pipeline.Tool{{Name: "noop",
			Handler: func(context.Context, []byte) (string, error) { return "", nil }}},
	})
	if err != nil {
		log.Fatal(err)
	}

	// 专家 Act：观察黑板增量 → 与己相关则贡献（各自跑自己的 Run）。
	specialists := []blackboard.Specialist{
		{
			Name: "库存专家",
			Act: func(ctx context.Context, b *blackboard.Board, since int64) (bool, error) {
				return contribute(ctx, b, since, inv, "库存专家")
			},
		},
		{
			Name: "文案专家",
			Act: func(ctx context.Context, b *blackboard.Board, since int64) (bool, error) {
				return contribute(ctx, b, since, copyExpert, "文案专家")
			},
		},
	}

	result, err := blackboard.Convene(ctx, board, specialists, &blackboard.ConveneOptions{MaxRounds: 2})
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("Convene: rounds=%d entries=%d\n", result.Rounds, result.Total)
	for _, e := range board.Entries() {
		fmt.Printf("  [%d] %s/%s: %s\n", e.Seq, e.Author, e.Kind, e.Content)
	}
}

// contribute 专家贡献一次：读增量 → 跑会话 Run → 写发现。
func contribute(ctx context.Context, b *blackboard.Board, since int64,
	session *runtime.Session, expert string) (bool, error) {

	news := b.EntriesAfter(since)
	if len(news) == 0 {
		return false, nil // 无增量：本专家本轮无事可做
	}
	var sb strings.Builder
	for _, e := range news {
		fmt.Fprintf(&sb, "- [%s/%s] %s\n", e.Author, e.Kind, e.Content)
	}
	res, err := session.Run(ctx, "观察以下黑板增量，给出你的发现：\n"+sb.String())
	if err != nil {
		return false, err
	}
	if strings.Contains(res.Final, "无补充") {
		return false, nil
	}
	b.Write(expert, "finding", res.Final)
	return true, nil
}
