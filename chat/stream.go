// 流式承载面与归并器（issue #11）。

// # 为什么要有这一份

// 流式形态下，模型的产出是**分片**的：正文按 token 片段吐，工具调用的
// 参数也按片段吐（同一调用跨多个片段），usage 另行上报。宿主若自己写
// 「按 Index 归并 + usage 累计」那座桥，每个保流式的宿主就各写一遍、
// 各踩各的坑（归并错位、usage 重复计、片段顺序错）。

// ⇒ 归并规则收进**框架侧的一份实现**（本文件的 DeltaAggregator）：
// 宿主用它拼响应（不再各写一份）；事件面只透传**原样**片段（观测面要的是
// 原样，不是归并后的结果）——同一段语义、同一份代码。
// 宿主剩下的活只有「把 SDK 的 chunk 搬成片段」与 EOF/错误上抛。

// # 边界

//   - 本文件**不引入新接口**：chat.Model 契约一字未改（老宿主照常编译）。
//   - 片段的**必填分组键是 Index**（≥ 0）；Index < 0 的片段无法归组，
//     被丢弃并计入 Dropped()——不猜、不硬塞进上一条调用。
//   - Usage 按**增量**语义累加（多数 SDK 只在末片给一次总量 ⇒ 累加与
//     直接取值等价；给多次增量的 SDK 也对）。给累计值的 SDK 由宿主适配
//     层先做差。
package chat

import "strings"

// ToolCallFragment 工具调用的流式片段：一次调用的参数跨多个片段到达，
// 同一 Index 的片段属于同一次调用。
type ToolCallFragment struct {
	// Index 分组键（≥ 0）：同一 Index 的片段归并成一次调用。
	Index int
	// ID 调用标识（首个片段带，后续片段可空）。
	ID string
	// Name 工具名（首个片段带，后续片段可空）。
	Name string
	// Arguments 参数片段（JSON 文本，按到达顺序拼接）。
	Arguments string
}

// Usage token 计量（增量语义；见本文件顶部「边界」）。
type Usage struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
}

// DeltaAggregator 流式片段归并器：
//
//	Add(d)         收一片增量（乱序到达也按 Index 归位）
//	Response()     归并后的响应（文本拼接 + 工具调用归并）
//	Usage()        累计计量
//	Dropped()      被丢弃的片段数（Index 非法 ⇒ 无法归组）
//
// 零值即可用；非并发安全（单次模型调用内使用）。
type DeltaAggregator struct {
	texts   []string
	order   []int
	frags   map[int]*fragCall
	usage   Usage
	dropped int
}

// fragCall 归并中的工具调用。
type fragCall struct {
	id   string
	name string
	args strings.Builder
}

// Add 收一片增量。文本片段按到达顺序拼接；工具调用片段按 Index 归组
// （先到先得 ID/Name，参数按到达顺序拼接）；usage 累加。
func (a *DeltaAggregator) Add(d Delta) {
	if d.ToolCall != nil {
		a.addFragment(*d.ToolCall)
	}
	if d.Usage != nil {
		a.usage.PromptTokens += d.Usage.PromptTokens
		a.usage.CompletionTokens += d.Usage.CompletionTokens
		a.usage.TotalTokens += d.Usage.TotalTokens
	}
	if d.Text != "" {
		a.texts = append(a.texts, d.Text)
	}
}

// addFragment 按 Index 归组一个工具调用片段。
func (a *DeltaAggregator) addFragment(f ToolCallFragment) {
	if f.Index < 0 {
		// 无分组键 ⇒ 无法归组。不猜（不塞进上一条调用），只记账。
		a.dropped++
		return
	}
	if a.frags == nil {
		a.frags = map[int]*fragCall{}
	}
	c, ok := a.frags[f.Index]
	if !ok {
		c = &fragCall{}
		a.frags[f.Index] = c
		a.order = append(a.order, f.Index)
	}
	if c.id == "" {
		c.id = f.ID
	}
	if c.name == "" {
		c.name = f.Name
	}
	if f.Arguments != "" {
		c.args.WriteString(f.Arguments)
	}
}

// Text 归并后的正文（片段按到达顺序拼接）。
func (a *DeltaAggregator) Text() string {
	return strings.Join(a.texts, "")
}

// Response 归并后的响应：正文 + 工具调用（按首次出现顺序）。
func (a *DeltaAggregator) Response() ChatResponse {
	resp := ChatResponse{Content: a.Text()}
	for _, idx := range a.order {
		c := a.frags[idx]
		resp.ToolCalls = append(resp.ToolCalls, ToolCall{
			ID:        c.id,
			Name:      c.name,
			Arguments: c.args.String(),
		})
	}
	return resp
}

// Usage 累计计量。
func (a *DeltaAggregator) Usage() Usage {
	return a.usage
}

// Dropped 被丢弃的片段数（Index 非法 ⇒ 无法归组）。
func (a *DeltaAggregator) Dropped() int {
	return a.dropped
}
