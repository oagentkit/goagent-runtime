package agentiface

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/oagentkit/agent/iface"
	"github.com/yi-nology/goagent-kernel/contract"
	"github.com/yi-nology/goagent-runtime/agentloop"
	"github.com/yi-nology/goagent-runtime/chat"
	"github.com/yi-nology/goagent-runtime/pipeline"
	"github.com/yi-nology/goagent-runtime/storage"
)

// 脚本化模型：一次 Chat 返回固定终答（loop_test 同型，独立副本避免跨包耦合）。
type scriptedModel struct {
	final string
	err   error
}

func (m *scriptedModel) Chat(context.Context, chat.ChatRequest) (chat.ChatResponse, error) {
	if m.err != nil {
		return chat.ChatResponse{}, m.err
	}
	return chat.ChatResponse{Content: m.final}, nil
}

func newLoop(t *testing.T, m chat.Model) *agentloop.Loop {
	t.Helper()
	mem := storage.NewMem()
	pipe, err := pipeline.New(mem.Effects(), time.Minute, nil)
	if err != nil {
		t.Fatalf("建管线失败: %v", err)
	}
	l, err := agentloop.New(agentloop.Config{
		Model:  m,
		Tools:  pipe,
		Store:  mem.Checkpoints(),
		System: "you are a test bot",
	})
	if err != nil {
		t.Fatalf("建循环失败: %v", err)
	}
	return l
}

func TestAgentloopRunSuccess(t *testing.T) {
	a := NewAgentloop(newLoop(t, &scriptedModel{final: "循环终答"}))
	got, err := a.Run(context.Background(), iface.Input{SessionID: "s-1", Query: "问题"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Status 是 contract 词表值（loop 内直接引常量）——透传即对齐
	if got.Status != contract.StatusSucceeded {
		t.Fatalf("Status = %q, want %q", got.Status, contract.StatusSucceeded)
	}
	if got.Final != "循环终答" {
		t.Fatalf("Final = %q", got.Final)
	}
}

func TestAgentloopRunModelError(t *testing.T) {
	a := NewAgentloop(newLoop(t, &scriptedModel{err: errors.New("模型爆炸")}))
	got, err := a.Run(context.Background(), iface.Input{SessionID: "s-1", Query: "问题"})
	if err == nil {
		t.Fatal("模型错误应当上抛")
	}
	if got.Status != contract.StatusFailed {
		t.Fatalf("Status = %q, want %q", got.Status, contract.StatusFailed)
	}
}

func TestAgentloopRejectsOnEvent(t *testing.T) {
	// 事件面是 kernel eventbus（ADR-0010）——显式拒绝，不静默丢事件
	a := NewAgentloop(newLoop(t, &scriptedModel{final: "x"}))
	_, err := a.Run(context.Background(), iface.Input{
		SessionID: "s-1", Query: "q",
		OnEvent: func(iface.Event) {},
	})
	if err == nil {
		t.Fatal("OnEvent 非 nil 必须显式报错")
	}
}

func TestAgentloopRequiresSessionID(t *testing.T) {
	a := NewAgentloop(newLoop(t, &scriptedModel{final: "x"}))
	_, err := a.Run(context.Background(), iface.Input{Query: "q"})
	if err == nil {
		t.Fatal("会话制入口空 SessionID 必须报错")
	}
}
