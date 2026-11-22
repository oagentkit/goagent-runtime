package cluster

import (
	"context"
	"testing"

	"github.com/oekit/pkg/errors"

	"github.com/yi-nology/goagent-runtime/agentloop"
	"github.com/yi-nology/goagent-runtime/chat"
)

// TestNewRejectsIncompleteConfig 装配期 fail fast：缺模型面 / 缺 DSN 都是
// 编程错误，不许等到第一次投递才炸。
func TestNewRejectsIncompleteConfig(t *testing.T) {
	if _, err := New(context.Background(), Config{}, "postgres://x/y"); err == nil {
		t.Fatal("缺模型面却装配成功（应在装配期拒绝）")
	}
	if _, err := New(context.Background(), Config{Model: stubModel{}}, ""); err == nil {
		t.Fatal("缺 DSN 却装配成功（应在装配期拒绝）")
	}
}

// TestResultRoundTrip 结果事件载荷往返：字段类型不漂（这是跨副本回传的
// 唯一通道，载荷类型不稳就等于把结论传错）。
func TestResultRoundTrip(t *testing.T) {
	want := &agentloop.Result{
		Status:    "succeeded",
		Final:     "共 3 条",
		Rounds:    2,
		ToolCalls: 1,
		Uncertain: []string{"call-1"},
	}
	payload, err := encodeResult(want)
	if err != nil {
		t.Fatalf("结果编码失败: %v", err)
	}
	got, err := decodeResult(payload)
	if err != nil {
		t.Fatalf("结果解码失败: %v", err)
	}
	if got.Status != want.Status || got.Final != want.Final ||
		got.Rounds != want.Rounds || got.ToolCalls != want.ToolCalls {
		t.Fatalf("结果往返字段不一致：got=%+v want=%+v", got, want)
	}
	if len(got.Uncertain) != 1 || got.Uncertain[0] != "call-1" {
		t.Fatalf("Uncertain 往返不一致：got=%v want=%v", got.Uncertain, want.Uncertain)
	}
}

// TestDecodeResultRejectsGarbage 载荷缺字段/类型不对 ⇒ 报错，不给半个结论。
func TestDecodeResultRejectsGarbage(t *testing.T) {
	if _, err := decodeResult(map[string]any{}); err == nil {
		t.Fatal("载荷缺 result 字段却解码成功")
	}
	if _, err := decodeResult(map[string]any{"result": 42}); err == nil {
		t.Fatal("载荷类型不对却解码成功")
	}
	if _, err := decodeResult(map[string]any{"result": "{"}); err == nil {
		t.Fatal("载荷不是合法 JSON 却解码成功")
	}
}

// stubModel 只为装配期校验存在的最小模型面。
type stubModel struct{}

func (stubModel) Chat(context.Context, chat.ChatRequest) (chat.ChatResponse, error) {
	return chat.ChatResponse{}, errors.New("stub：不该被调用")
}
