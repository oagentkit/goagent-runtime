// Package chat —— AgentLoop 与模型之间的**供应商中立窄接口**。
//
// # 为什么本包存在（装配层的接缝设计）
//
// goagent-kernel 的纪律是「内核不调 LLM」；装配层必须把模型接进来，但
// 接成什么形状决定 runtime 的可测试性与供应商中立性：
//
//  1. **窄接口**：Model 只有一个 Chat 方法。任何供应商（eino / OpenAI SDK /
//     自研网关）包一层适配即可接入，runtime 不 import 任何具体 SDK。
//  2. **纯数据结构**：Message/ToolCall 是普通结构体，序列化进 checkpoint
//     快照后恢复，不携带任何供应商特有句柄。
//  3. **工具调用 ID 由 loop 铸造**（见 agentloop 的 NewID 注入点）：
//     ID 是幂等键的原材料，必须由编排侧生成并在恢复后保持一致，
//     不能让模型供应商自由发挥。
package chat

import "context"

// 消息角色词表（与主流 chat API 同形；本包不解释语义，只做传输）。
const (
	RoleSystem    = "system"
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleTool      = "tool"
)

// ToolCall 模型请求的一次工具调用。
type ToolCall struct {
	// ID 本次调用的标识——**编排侧铸造**（不是模型编的），
	// 它同时是副作用幂等键的原材料：恢复重放时同一调用必须带同一 ID。
	ID string
	// Name 工具名（pipeline 注册表查它）。
	Name string
	// Arguments JSON 文本，原样传给工具的 Handler。
	Arguments string
}

// Message 一轮对话中的一条消息。
type Message struct {
	Role    string
	Content string
	// ToolCalls assistant 消息携带的工具调用请求。
	ToolCalls []ToolCall
	// ToolCallID tool 消息回填时对应的调用 ID。
	ToolCallID string
}

// ToolSpec 暴露给模型的工具面（描述 + 参数模式）。
type ToolSpec struct {
	Name        string
	Description string
	// Parameters JSON Schema 文本；空串 = 不声明参数模式。
	Parameters string
}

// Delta 流式增量（OnDelta 回调输入；模型支持流式时逐片调用）。
//
// 结构化片段（工具调用 / 计量）是**可选**字段：只吐文本的实现不必构造它们；
// 归并规则见 stream.go 的 DeltaAggregator（框架与宿主共用同一份）。
type Delta struct {
	// Kind 增量种类："text" 正文 / "reasoning" 推理过程 / "tool_call"
	// 工具调用片段 / "usage" 计量。词表开放——模型不产某类即不回调，
	// 宿主按 Kind 分流即可。
	Kind string
	// Text 本片增量文本（非累积）。
	Text string
	// ToolCall 工具调用片段（Kind=DeltaKindToolCall 时带；Text 留空）。
	ToolCall *ToolCallFragment
	// Usage 计量片段（Kind=DeltaKindUsage 时带）。
	Usage *Usage
}

// 增量种类词表（Kind 取值；开放集，宿主可识别未知 Kind 透传）。
const (
	DeltaKindText      = "text"
	DeltaKindReasoning = "reasoning"
	DeltaKindToolCall  = "tool_call"
	DeltaKindUsage     = "usage"
)

// ChatRequest 一次模型调用请求。
type ChatRequest struct {
	Messages []Message
	Tools    []ToolSpec
	// OnDelta 可选流式增量回调：非 nil 且模型支持流式时逐片回调（文本
	// 旁路面），最终结果仍经 ChatResponse 返回——回调不改变响应语义；
	// 不支持流式的实现可不调用（宿主降级整段接收，nil 语义同缺省）。
	OnDelta func(Delta)
}

// ChatResponse 一次模型调用响应。
type ChatResponse struct {
	// Content 终答文本（ToolCalls 为空时即最终回答）。
	Content string
	// ToolCalls 非空 = 模型请求执行工具，loop 将进入工具回合。
	ToolCalls []ToolCall
	// Usage 可选计量（模型/适配层上报时填；nil = 未上报）。
	//
	// 非流式契约的计量面：流式走 Delta.Usage（DeltaAggregator 归并），
	// 非流式适配层拿到供应商的 usage 字段时填这里——loop 会把它带进
	// round 事件，Trace 聚合出 Token/成本（issue #42）。不填不报错，
	// 只是聚合面少一份数据。
	Usage *Usage
}

// Model 供应商中立的模型调用接口——runtime 唯一需要的模型面。
type Model interface {
	Chat(ctx context.Context, req ChatRequest) (ChatResponse, error)
}
