// Package einotool 把 eino 工具面（tool.InvokableTool）适配成
// goagent-runtime 的 pipeline.Tool——与 llmchat 同一条「家族钦定适配器」纪律：
// 已有 eino 工具生态的宿主一族工具直接接入 runtime，不必逐个手写桥。
//
// # 副作用闸字段的边界
//
// pipeline.Tool 的 Safe / NeedsApproval / IdempotencyKey / ResourceKey 是
// runtime 副作用闸的语义面，eino 的 ToolInfo 表达不了——适配器**不猜**：
// 全部落零值（ Safe=false / NeedsApproval=false / 无幂等键），副作用闸语义
// 宿主在装配侧按业务补（见 pipeline.Tool 文档）。适配器的职责只有形状转换。
package einotool

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/cloudwego/eino/components/tool"
	"github.com/oagentkit/goagent-runtime/pipeline"
)

// Adapt 把一个 eino InvokableTool 适配成 pipeline.Tool。
// Info(ctx) 取名/描述/参数模式；InvokableRun 包成 Handler（[]byte → string）。
func Adapt(ctx context.Context, t tool.InvokableTool) (pipeline.Tool, error) {
	info, err := t.Info(ctx)
	if err != nil {
		return pipeline.Tool{}, fmt.Errorf("einotool: Info 失败: %w", err)
	}
	if info == nil {
		return pipeline.Tool{}, fmt.Errorf("einotool: Info 返回 nil")
	}

	tl := pipeline.Tool{
		Name:        info.Name,
		Description: info.Desc,
		Handler: func(c context.Context, args []byte) (string, error) {
			return t.InvokableRun(c, string(args))
		},
	}
	if info.ParamsOneOf != nil {
		// eino 没有公开的 ParamsOneOf→JSON Schema 提取面，走 MarshalJSON 的
		// 往返形状（json_schema 字段以字符串承载 schema 文本）——这是它
		// 自己的 roundtrip 不变量（见 eino schema/tool.go 的 MarshalJSON）。
		raw, merr := json.Marshal(info)
		if merr != nil {
			return pipeline.Tool{}, fmt.Errorf("einotool: 工具 %q 信息序列化失败: %w", info.Name, merr)
		}
		var shaped struct {
			JSONSchema json.RawMessage `json:"json_schema"`
		}
		if err := json.Unmarshal(raw, &shaped); err != nil {
			return pipeline.Tool{}, fmt.Errorf("einotool: 工具 %q 参数模式提取失败: %w", info.Name, err)
		}
		if len(shaped.JSONSchema) > 0 {
			// 两种版本形态都接：eino 某些版本把 json_schema 序列化成字符串
			//（schema 文本），某些版本直接是对象——字符串形态先解引。
			var asText string
			if err := json.Unmarshal(shaped.JSONSchema, &asText); err == nil {
				tl.Parameters = json.RawMessage(asText)
			} else {
				tl.Parameters = shaped.JSONSchema
			}
		}
	}
	return tl, nil
}

// AdaptAll 批量适配；任一失败即整体失败（fail-closed——半装配的工具目录
// 会让模型看到残缺工具面）。
func AdaptAll(ctx context.Context, tools []tool.InvokableTool) ([]pipeline.Tool, error) {
	out := make([]pipeline.Tool, 0, len(tools))
	for _, t := range tools {
		tl, err := Adapt(ctx, t)
		if err != nil {
			return nil, err
		}
		out = append(out, tl)
	}
	return out, nil
}
