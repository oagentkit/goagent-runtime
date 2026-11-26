package llmchat

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/yi-nology/goagent-runtime/chat"
	"github.com/oagentkit/llm"
)

// 编译期断言：fakeGen 满足家族模型面接口（New 的入参类型）。
var _ llm.Generator = (*fakeGen)(nil)

// fakeBase 可编程的裸模型：记录收到的工具面与消息，回放预设响应。
type fakeBase struct {
	gotTools   []*schema.ToolInfo
	gotMsgs    []*schema.Message
	genResp    *schema.Message
	streamFunc func() *schema.StreamReader[*schema.Message]
	streamErr  error
}

func (f *fakeBase) Generate(ctx context.Context, in []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	common := model.GetCommonOptions(&model.Options{}, opts...)
	f.gotTools = common.Tools
	f.gotMsgs = in
	return f.genResp, nil
}

func (f *fakeBase) Stream(ctx context.Context, in []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	if f.streamErr != nil {
		return nil, f.streamErr
	}
	return f.streamFunc(), nil
}

// fakeGen 只为满足 llm.Generator；本适配器经 RawModel 取裸模型。
type fakeGen struct{ base model.BaseChatModel }

func (f *fakeGen) Generate(ctx context.Context, stage string, msgs []*schema.Message) (*schema.Message, error) {
	return f.base.Generate(ctx, msgs)
}
func (f *fakeGen) GenerateJSON(ctx context.Context, stage string, msgs []*schema.Message, out any) error {
	return errors.New("fakeGen: 不应走到 GenerateJSON")
}
func (f *fakeGen) UsedTokens() int               { return 0 }
func (f *fakeGen) RawModel() model.BaseChatModel { return f.base }

func newTestModel(t *testing.T, base model.BaseChatModel) *Model {
	t.Helper()
	m, err := New(&fakeGen{base: base})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return m
}

func chatReq(msgs ...chat.Message) chat.ChatRequest {
	return chat.ChatRequest{Messages: msgs}
}

func TestChatWithoutToolsUsesGeneratePath(t *testing.T) {
	base := &fakeBase{genResp: schema.AssistantMessage("终答", nil)}
	m := newTestModel(t, base)
	resp, err := m.Chat(context.Background(), chatReq(chat.Message{Role: chat.RoleUser, Content: "你好"}))
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Content != "终答" || len(resp.ToolCalls) != 0 {
		t.Fatalf("resp = %+v", resp)
	}
	if len(base.gotMsgs) != 1 || base.gotMsgs[0].Role != schema.User || base.gotMsgs[0].Content != "你好" {
		t.Fatalf("msgs 未正确转换: %+v", base.gotMsgs)
	}
}

func TestChatWithToolsPassesToolInfos(t *testing.T) {
	base := &fakeBase{genResp: schema.AssistantMessage("", nil)}
	m := newTestModel(t, base)
	req := chat.ChatRequest{
		Messages: []chat.Message{{Role: chat.RoleUser, Content: "查库存"}},
		Tools: []chat.ToolSpec{{
			Name: "查库存", Description: "按 SKU 查库存",
			Parameters: `{"type":"object","properties":{"sku":{"type":"string"}}}`,
		}},
	}
	if _, err := m.Chat(context.Background(), req); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if len(base.gotTools) != 1 || base.gotTools[0].Name != "查库存" {
		t.Fatalf("工具面未透传: %+v", base.gotTools)
	}
}

func TestChatWithToolsReturnsToolCalls(t *testing.T) {
	base := &fakeBase{genResp: schema.AssistantMessage("", []schema.ToolCall{
		{ID: "call-1", Function: schema.FunctionCall{Name: "查库存", Arguments: `{"sku":"A"}`}},
	})}
	m := newTestModel(t, base)
	req := chat.ChatRequest{
		Messages: []chat.Message{{Role: chat.RoleUser, Content: "查库存"}},
		Tools:    []chat.ToolSpec{{Name: "查库存", Description: "按 SKU 查库存"}},
	}
	resp, err := m.Chat(context.Background(), req)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].ID != "call-1" || resp.ToolCalls[0].Name != "查库存" {
		t.Fatalf("tool calls 未透传: %+v", resp.ToolCalls)
	}
}

func TestStreamForwardsDeltasAndAggregates(t *testing.T) {
	base := &fakeBase{streamFunc: func() *schema.StreamReader[*schema.Message] {
		sr, sw := schema.Pipe[*schema.Message](8)
		go func() {
			defer sw.Close()
			sw.Send(schema.UserMessage("你"), nil)
			sw.Send(schema.UserMessage("好"), nil)
			idx := 0
			sw.Send(&schema.Message{Role: schema.Assistant, ToolCalls: []schema.ToolCall{
				{Index: &idx, ID: "call-1", Function: schema.FunctionCall{Name: "查库存", Arguments: `{"sku"`}},
			}}, nil)
			sw.Send(&schema.Message{Role: schema.Assistant, ToolCalls: []schema.ToolCall{
				{Index: &idx, Function: schema.FunctionCall{Arguments: `:"A"}`}},
			}}, nil)
		}()
		return sr
	}}
	m := newTestModel(t, base)

	var kinds []string
	var texts []string
	resp, err := m.Chat(context.Background(), chat.ChatRequest{
		Messages: []chat.Message{{Role: chat.RoleUser, Content: "查库存"}},
		OnDelta: func(d chat.Delta) {
			kinds = append(kinds, d.Kind)
			if d.Text != "" {
				texts = append(texts, d.Text)
			}
		},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	// OnDelta 逐片收到 text 与 tool_call 片段。
	if len(kinds) < 4 || kinds[0] != chat.DeltaKindText || kinds[2] != chat.DeltaKindToolCall {
		t.Fatalf("kinds = %v", kinds)
	}
	if strings.Join(texts, "") != "你好" {
		t.Fatalf("texts = %v", texts)
	}
	// 最终响应经归并器得出：工具调用按 Index 归并成一次完整调用。
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Arguments != `{"sku":"A"}` {
		t.Fatalf("归并结果 = %+v", resp.ToolCalls)
	}
}

func TestStreamEmptyIsError(t *testing.T) {
	base := &fakeBase{streamFunc: func() *schema.StreamReader[*schema.Message] {
		sr, sw := schema.Pipe[*schema.Message](1)
		go func() { sw.Close() }()
		return sr
	}}
	m := newTestModel(t, base)
	_, err := m.Chat(context.Background(), chat.ChatRequest{
		Messages: []chat.Message{{Role: chat.RoleUser, Content: "hi"}},
		OnDelta:  func(chat.Delta) {},
	})
	if err == nil || !strings.Contains(err.Error(), "流式无任何片段") {
		t.Fatalf("期望空流报错，got %v", err)
	}
}

func TestUnknownRoleRejected(t *testing.T) {
	base := &fakeBase{}
	m := newTestModel(t, base)
	_, err := m.Chat(context.Background(), chatReq(chat.Message{Role: "developer", Content: "x"}))
	if err == nil || !strings.Contains(err.Error(), "未知 role") {
		t.Fatalf("期望未知 role 报错，got %v", err)
	}
}

func TestNilRawModelRejected(t *testing.T) {
	if _, err := New(&fakeGen{base: nil}); err == nil {
		t.Fatal("期望 RawModel 为 nil 报错")
	}
}
