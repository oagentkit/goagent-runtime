package preset

import (
	"context"
	"testing"

	"github.com/oagentkit/agent/roundguard"

	"github.com/yi-nology/goagent-runtime/chat"
	"github.com/yi-nology/goagent-runtime/pipeline"
	"github.com/yi-nology/goagent-runtime/runtime"
)

// loopModel 无限工具调用剧本：每轮都要调工具、永不收口——
// 预算上限是唯一能停它的东西（零值 Budget 若没被 preset 填上，本用例会挂死
// 到测试超时；填上了则在 DefaultReActMaxRounds 处收口）。
type loopModel struct{ calls int }

func (m *loopModel) Chat(_ context.Context, _ chat.ChatRequest) (chat.ChatResponse, error) {
	m.calls++
	return chat.ChatResponse{ToolCalls: []chat.ToolCall{
		{ID: "c", Name: "noop", Arguments: "{}"},
	}}, nil
}

// TestReActZeroBudgetCapsAtDefault 钉住 ReAct 形态的默认口径：零值 Budget
// 必须补 DefaultReActMaxRounds——「自主循环没有上限在生产上不可验收」
// （ADR-004 §四的形态差异面）。Simple 不补（一次性执行无循环语义）。
func TestReActZeroBudgetCapsAtDefault(t *testing.T) {
	m := &loopModel{}
	res, err := ReAct(context.Background(), runtime.Config{
		Model: m,
		Tools: []pipeline.Tool{{Name: "noop",
			Handler: func(context.Context, []byte) (string, error) { return "", nil }}},
	}, "budget", "go")
	if err != nil {
		t.Fatalf("ReAct 执行失败: %v", err)
	}
	// 收口即证明补上了上限（loopModel 永不收口——没补上限则本用例挂到超时）；
	// rounds = 工具轮 + PolicyForceFinal 收口轮，故上界是默认上限 +1。
	if res.Rounds > DefaultReActMaxRounds+1 {
		t.Fatalf("零值 Budget 应在默认上限 %d 附近收口，实得 rounds=%d（模型调用 %d 次）",
			DefaultReActMaxRounds, res.Rounds, m.calls)
	}
}

// TestReActExplicitBudgetRespected 宿主显式声明的上限一律尊重（preset 只填零值）。
func TestReActExplicitBudgetRespected(t *testing.T) {
	m := &loopModel{}
	res, err := ReAct(context.Background(), runtime.Config{
		Model: m,
		Tools: []pipeline.Tool{{Name: "noop",
			Handler: func(context.Context, []byte) (string, error) { return "", nil }}},
		Budget: roundguard.Budget{MaxRounds: 1},
	}, "budget-explicit", "go")
	if err != nil {
		t.Fatalf("ReAct 执行失败: %v", err)
	}
	// 显式上限同样尊重：1 个工具轮 + 收口轮，且绝不能跑到默认上限去。
	if res.Rounds > 2 {
		t.Fatalf("显式 MaxRounds=1 应在 2 轮内收口（工具轮+收口轮），实得 rounds=%d", res.Rounds)
	}
}
