// Package llmchat 把家族模型面 github.com/oagentkit/llm（eino 栈）适配成
// goagent-runtime 的 chat.Model——家族钦定适配器：宿主不必各写一座桥。
//
// 为什么是独立嵌套模块：runtime 主 module 的声明面是「模型接缝只有 chat.Model，
// 生产代码零 LLM SDK import」（ADR-0003）。本包要 import llm（它背后是
// cloudwego/eino），放主 module 会打破那条声明；放 llm 仓则构成 llm(t2)→
// runtime(装配层) 的反向依赖。嵌套模块让「LLM SDK 边」只出现在 bridge 内，
// 与 sqliteadapter/pgstore 的「驱动边只在此包」是同一条纪律的第三个实例。
//
// # 归并语义边界
//
// 流式路径只负责**搬运**：eino 的分片按 Index 归组是 chat.DeltaAggregator 的
// 职责（框架一份实现），本包只把它喂给归并器得出最终响应——宿主侧的
// OnDelta 仍逐片收到原始片段。裸模型路径的降级语义跟随底层模型：RawModel()
// 返回的若是 llm 的 FailoverModel，首块前失败仍会切模型；若是普通模型，
// 则无降级——这是 llm.Generator.RawModel() 接口的既有契约，本包不另造
// 第二套降级。
package llmchat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	ejsonschema "github.com/eino-contrib/jsonschema"
	"github.com/yi-nology/goagent-runtime/chat"
	"github.com/oagentkit/llm"
)

// Option 桥接选项。
type Option func(*options)

type options struct {
	// stage Generate 路径的 stage 标识（llm 的预算/观测按 stage 分账）。
	stage string
}

// WithStage 设置 Generate 路径的 stage 标识（缺省 "chat"）。
func WithStage(stage string) Option {
	return func(o *options) { o.stage = stage }
}

// Model llm.Generator → chat.Model 适配器。
type Model struct {
	g     llm.Generator
	base  model.BaseChatModel // RawModel 摘出的裸模型：工具调用与流式经它下发
	stage string
}

// New 构造适配器。g 的 RawModel() 必须非 nil（llm.Client/Resilient 均满足）。
func New(g llm.Generator, opts ...Option) (*Model, error) {
	base := g.RawModel()
	if base == nil {
		return nil, errors.New("llmchat: Generator.RawModel() 为 nil，无法承接工具调用路径")
	}
	o := &options{stage: "chat"}
	for _, f := range opts {
		f(o)
	}
	return &Model{g: g, base: base, stage: o.stage}, nil
}

// Chat 实现 chat.Model：带工具走裸模型路径（WithTools）；不带工具且要流式
// 走 Stream 搬运；否则走 Generator.Generate（保留 llm 的 stage 分账面）。
func (m *Model) Chat(ctx context.Context, req chat.ChatRequest) (chat.ChatResponse, error) {
	msgs, err := toEinoMessages(req.Messages)
	if err != nil {
		return chat.ChatResponse{}, err
	}
	toolInfos, err := toEinoTools(req.Tools)
	if err != nil {
		return chat.ChatResponse{}, err
	}

	var resp *schema.Message
	switch {
	case len(req.Tools) > 0:
		opts := []model.Option{}
		if len(toolInfos) > 0 {
			opts = append(opts, model.WithTools(toolInfos))
		}
		resp, err = m.base.Generate(ctx, msgs, opts...)
	case req.OnDelta != nil:
		return m.chatStream(ctx, msgs, req.OnDelta)
	default:
		resp, err = m.g.Generate(ctx, m.stage, msgs)
	}
	if err != nil {
		return chat.ChatResponse{}, err
	}
	return fromEinoMessage(resp), nil
}

// chatStream 流式搬运：text/reasoning 逐片转发给 OnDelta，工具调用片段带
// Index 原样转发，usage 从 ResponseMeta.TokenUsage 派生；同时喂给
// DeltaAggregator 得出最终响应（归并规则框架侧一份，本包不重写）。
func (m *Model) chatStream(ctx context.Context, msgs []*schema.Message, onDelta func(chat.Delta)) (chat.ChatResponse, error) {
	sr, err := m.base.Stream(ctx, msgs)
	if err != nil {
		return chat.ChatResponse{}, err
	}
	defer sr.Close()

	agg := chat.DeltaAggregator{}
	indexOf := map[*schema.ToolCall]int{}
	nextIndex := 0
	for {
		msg, rerr := sr.Recv()
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				break
			}
			return chat.ChatResponse{}, rerr
		}
		if msg == nil {
			continue
		}
		if msg.ReasoningContent != "" {
			onDelta(chat.Delta{Kind: chat.DeltaKindReasoning, Text: msg.ReasoningContent})
		}
		if msg.Content != "" {
			onDelta(chat.Delta{Kind: chat.DeltaKindText, Text: msg.Content})
			agg.Add(chat.Delta{Kind: chat.DeltaKindText, Text: msg.Content})
		}
		for i := range msg.ToolCalls {
			tc := &msg.ToolCalls[i]
			idx, ok := indexOf[tc]
			if !ok {
				if tc.Index != nil {
					idx = *tc.Index
				} else {
					idx = nextIndex
					nextIndex++
				}
				indexOf[tc] = idx
			}
			frag := chat.ToolCallFragment{Index: idx, ID: tc.ID, Name: tc.Function.Name, Arguments: tc.Function.Arguments}
			onDelta(chat.Delta{Kind: chat.DeltaKindToolCall, ToolCall: &frag})
			agg.Add(chat.Delta{Kind: chat.DeltaKindToolCall, ToolCall: &frag})
		}
		if msg.ResponseMeta != nil && msg.ResponseMeta.Usage != nil {
			u := chat.Usage{
				PromptTokens:     msg.ResponseMeta.Usage.PromptTokens,
				CompletionTokens: msg.ResponseMeta.Usage.CompletionTokens,
				TotalTokens:      msg.ResponseMeta.Usage.TotalTokens,
			}
			onDelta(chat.Delta{Kind: chat.DeltaKindUsage, Usage: &u})
			agg.Add(chat.Delta{Kind: chat.DeltaKindUsage, Usage: &u})
		}
	}
	out := agg.Response()
	if len(out.ToolCalls) == 0 && out.Content == "" {
		// 流式全空：宁可上抛错误也不返回空响应（空响应会骗过 loop 的终答判定）。
		return chat.ChatResponse{}, errors.New("llmchat: 流式无任何片段")
	}
	return out, nil
}

// toEinoMessages chat.Message → eino schema.Message。
// role 词表按 chat 包常量逐值映射；未知 role 显式报错（不猜 = 不静默降级）。
func toEinoMessages(msgs []chat.Message) ([]*schema.Message, error) {
	out := make([]*schema.Message, 0, len(msgs))
	for _, m := range msgs {
		switch m.Role {
		case chat.RoleSystem:
			out = append(out, schema.SystemMessage(m.Content))
		case chat.RoleUser:
			out = append(out, schema.UserMessage(m.Content))
		case chat.RoleAssistant:
			out = append(out, schema.AssistantMessage(m.Content, toEinoToolCalls(m.ToolCalls)))
		case chat.RoleTool:
			out = append(out, schema.ToolMessage(m.Content, m.ToolCallID))
		default:
			return nil, fmt.Errorf("llmchat: 未知 role %q（chat 词表外，拒绝猜测）", m.Role)
		}
	}
	return out, nil
}

func toEinoToolCalls(tcs []chat.ToolCall) []schema.ToolCall {
	if len(tcs) == 0 {
		return nil
	}
	out := make([]schema.ToolCall, 0, len(tcs))
	for _, tc := range tcs {
		out = append(out, schema.ToolCall{
			ID:       tc.ID,
			Function: schema.FunctionCall{Name: tc.Name, Arguments: tc.Arguments},
		})
	}
	return out
}

func fromEinoMessage(m *schema.Message) chat.ChatResponse {
	resp := chat.ChatResponse{Content: m.Content}
	if len(m.ToolCalls) > 0 {
		resp.ToolCalls = make([]chat.ToolCall, 0, len(m.ToolCalls))
		for _, tc := range m.ToolCalls {
			resp.ToolCalls = append(resp.ToolCalls, chat.ToolCall{
				ID:        tc.ID,
				Name:      tc.Function.Name,
				Arguments: tc.Function.Arguments,
			})
		}
	}
	return resp
}

// toEinoTools chat.ToolSpec → eino ToolInfo。Parameters 是 JSON Schema 文本：
// 解析为 openapi3.Schema（空串 = 不声明参数模式）。
func toEinoTools(specs []chat.ToolSpec) ([]*schema.ToolInfo, error) {
	if len(specs) == 0 {
		return nil, nil
	}
	out := make([]*schema.ToolInfo, 0, len(specs))
	for _, s := range specs {
		info := &schema.ToolInfo{Name: s.Name, Desc: s.Description}
		if s.Parameters != "" {
			var sch ejsonschema.Schema
			if err := json.Unmarshal([]byte(s.Parameters), &sch); err != nil {
				return nil, fmt.Errorf("llmchat: 工具 %q 的 Parameters 不是合法 JSON Schema: %w", s.Name, err)
			}
			info.ParamsOneOf = schema.NewParamsOneOfByJSONSchema(&sch)
		}
		out = append(out, info)
	}
	return out, nil
}
