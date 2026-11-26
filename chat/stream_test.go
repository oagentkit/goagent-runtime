// 流式承载面与归并器的判据（issue #11）。
//
// 为什么这些用例存在：归并规则（按 Index 归并、usage 累计）是**各宿主各写
// 一遍**的那段代码——规则收进框架后，它就成了「一处实现、一处判据」的
// 契约面：规则写错一次，所有保流式的宿主一起错。
package chat_test

import (
	"testing"

	"github.com/yi-nology/goagent-runtime/chat"
)

// TestDeltaAggregatorMergesToolCallsByIndex 工具调用片段按 Index 归并：
// 同 Index 的参数片段按到达顺序拼接；不同 Index 是不同的调用；
// 输出顺序 = 首次出现顺序。
func TestDeltaAggregatorMergesToolCallsByIndex(t *testing.T) {
	agg := &chat.DeltaAggregator{}
	agg.Add(chat.Delta{Kind: chat.DeltaKindToolCall, ToolCall: &chat.ToolCallFragment{
		Index: 0, ID: "call-1", Name: "calc", Arguments: `{"a":`}})
	agg.Add(chat.Delta{Kind: chat.DeltaKindToolCall, ToolCall: &chat.ToolCallFragment{
		Index: 1, ID: "call-2", Name: "search"}})
	agg.Add(chat.Delta{Kind: chat.DeltaKindToolCall, ToolCall: &chat.ToolCallFragment{
		Index: 0, Arguments: `1}`}})

	resp := agg.Response()
	if len(resp.ToolCalls) != 2 {
		t.Fatalf("应归并出 2 次调用，got %d (%+v)", len(resp.ToolCalls), resp.ToolCalls)
	}
	first := resp.ToolCalls[0]
	if first.ID != "call-1" || first.Name != "calc" {
		t.Fatalf("第 0 次调用的 id/name 应取首个片段：got %+v", first)
	}
	if first.Arguments != `{"a":1}` {
		t.Fatalf("参数片段应按到达顺序拼接：got %q", first.Arguments)
	}
	second := resp.ToolCalls[1]
	if second.ID != "call-2" || second.Name != "search" {
		t.Fatalf("第 1 次调用应独立归并：got %+v", second)
	}
}

// TestDeltaAggregatorAccumulatesUsage usage 按增量语义累加（多次上报）。
func TestDeltaAggregatorAccumulatesUsage(t *testing.T) {
	agg := &chat.DeltaAggregator{}
	agg.Add(chat.Delta{Kind: chat.DeltaKindUsage, Usage: &chat.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}})
	agg.Add(chat.Delta{Kind: chat.DeltaKindUsage, Usage: &chat.Usage{PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3}})
	u := agg.Usage()
	if u.PromptTokens != 11 || u.CompletionTokens != 7 || u.TotalTokens != 18 {
		t.Fatalf("usage 应累加：got %+v", u)
	}
}

// TestDeltaAggregatorConcatenatesText 文本片段按到达顺序拼接（不累积重复）。
func TestDeltaAggregatorConcatenatesText(t *testing.T) {
	agg := &chat.DeltaAggregator{}
	agg.Add(chat.Delta{Kind: chat.DeltaKindText, Text: "你"})
	agg.Add(chat.Delta{Kind: chat.DeltaKindText, Text: "好"})
	agg.Add(chat.Delta{Kind: chat.DeltaKindReasoning, Text: "（想）"})
	if got := agg.Text(); got != "你好（想）" {
		t.Fatalf("文本拼接: got %q", got)
	}
	if got := agg.Response().Content; got != "你好（想）" {
		t.Fatalf("Response.Content 应是拼接文本: got %q", got)
	}
}

// TestDeltaAggregatorDropsFragmentWithoutIndex Index 非法（< 0）的片段无法
// 归组 ⇒ 丢弃并记账——不猜（不塞进上一条调用），也不静默。
func TestDeltaAggregatorDropsFragmentWithoutIndex(t *testing.T) {
	agg := &chat.DeltaAggregator{}
	agg.Add(chat.Delta{Kind: chat.DeltaKindToolCall, ToolCall: &chat.ToolCallFragment{
		Index: -1, ID: "call-x", Name: "calc", Arguments: "{}"}})
	if agg.Dropped() != 1 {
		t.Fatalf("应记账 1 片丢弃，got %d", agg.Dropped())
	}
	if len(agg.Response().ToolCalls) != 0 {
		t.Fatalf("非法片段不得产生调用：got %+v", agg.Response().ToolCalls)
	}
}

// TestDeltaAggregatorZeroValueUsable 零值即可用（宿主不需要先构造任何东西）。
func TestDeltaAggregatorZeroValueUsable(t *testing.T) {
	var agg chat.DeltaAggregator
	if resp := agg.Response(); resp.Content != "" || len(resp.ToolCalls) != 0 {
		t.Fatalf("零值应返回空响应：got %+v", resp)
	}
	if u := agg.Usage(); u.TotalTokens != 0 {
		t.Fatalf("零值 usage 应全零：got %+v", u)
	}
}

// TestDeltaKindVocabulary 词表常量（宿主按 Kind 分流 ⇒ 取值不能漂）。
func TestDeltaKindVocabulary(t *testing.T) {
	cases := map[string]string{
		chat.DeltaKindText:      "text",
		chat.DeltaKindReasoning: "reasoning",
		chat.DeltaKindToolCall:  "tool_call",
		chat.DeltaKindUsage:     "usage",
	}
	for got, want := range cases {
		if got != want {
			t.Fatalf("词表漂移：got %q want %q", got, want)
		}
	}
}
