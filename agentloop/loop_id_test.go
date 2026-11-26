package agentloop_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/yi-nology/goagent-kernel/contract"

	"github.com/yi-nology/goagent-runtime/agentloop"
	"github.com/yi-nology/goagent-runtime/chat"
	"github.com/yi-nology/goagent-runtime/pipeline"
	"github.com/yi-nology/goagent-runtime/storage"
)

// TestNewIDMintedByLoop 模型不给 toolCallID 时由编排侧铸造：回填消息的
// ToolCallID 必须非空，且 assistant 消息里的 ID 与回填同源（幂等键的
// 原材料不能两边各长一个）。
func TestNewIDMintedByLoop(t *testing.T) {
	m := &fakeModel{errAt: -1, script: []chat.ChatResponse{
		// 模型返回**无 ID** 的工具调用。
		{ToolCalls: []chat.ToolCall{{Name: "calc", Arguments: "{}"}}},
		{Content: "完成"},
	}}
	c := &counter{}
	mem := storage.NewMem()
	pipe, err := pipeline.New(mem.Effects(), time.Minute, []pipeline.Tool{{Name: "calc", Safe: true, Handler: c.handler}})
	if err != nil {
		t.Fatalf("建管线失败: %v", err)
	}
	l, err := agentloop.New(agentloop.Config{Model: m, Tools: pipe, Store: mem.Checkpoints()})
	if err != nil {
		t.Fatalf("建循环失败: %v", err)
	}
	res, err := l.Run(context.Background(), "sess-1", "开始")
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	if res.Status != contract.StatusSucceeded {
		t.Fatalf("应为 succeeded，got %s", res.Status)
	}

	last := m.lastRequest()
	var assistantID, toolID string
	for _, msg := range last.Messages {
		if msg.Role == chat.RoleAssistant && len(msg.ToolCalls) > 0 {
			assistantID = msg.ToolCalls[0].ID
		}
		if msg.Role == chat.RoleTool {
			toolID = msg.ToolCallID
		}
	}
	if assistantID == "" {
		t.Fatal("assistant 消息的工具调用应由编排侧铸造 ID，不得为空")
	}
	if !strings.HasPrefix(assistantID, "call-") {
		t.Fatalf("默认 ID 应为 call- 前缀，got %q", assistantID)
	}
	if toolID != assistantID {
		t.Fatalf("回填 ID 与调用 ID 必须同源：%q vs %q", toolID, assistantID)
	}
}
