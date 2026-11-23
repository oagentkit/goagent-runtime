// Joint 模式端到端与原子性直证（runtime issue #2 主体）。
//
// 依赖方向：agentloop 不 import 具体存储（红线），故集成测试落 storage
// 包——它是装配邻域，可同时 import agentloop（装配）与 sqliteadapter
// （联合提交面实现）。
package storage_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/yi-nology/goagent-kernel/checkpoint"
	"github.com/yi-nology/goagent-kernel/contract"
	"github.com/yi-nology/goagent-kernel/eventbus"

	"github.com/yi-nology/goagent-runtime/agentloop"
	"github.com/yi-nology/goagent-runtime/chat"
	"github.com/yi-nology/goagent-runtime/pipeline"
	"github.com/yi-nology/goagent-runtime/sqliteadapter"
)

// jointFakeModel 脚本化模型（与 agentloop 测试同款判据，包内独立最小形态）。
type jointFakeModel struct {
	calls  int
	script []chat.ChatResponse
}

func (f *jointFakeModel) Chat(_ context.Context, _ chat.ChatRequest) (chat.ChatResponse, error) {
	i := f.calls
	f.calls++
	if i < len(f.script) {
		return f.script[i], nil
	}
	return chat.ChatResponse{Content: "fallback-final"}, nil
}

// TestJointCommitEndToEnd Joint 装配跑一轮：事件行与快照行同库同落——
// 一轮四事件（两轮 round_start + tool_call + session_state）全部持久化，
// 且提交后 Bus 缺省时无广播面也不丢数据。
func TestJointCommitEndToEnd(t *testing.T) {
	ctx := context.Background()
	sq, err := sqliteadapter.Open(filepath.Join(t.TempDir(), "joint-e2e.db"))
	if err != nil {
		t.Fatalf("开库失败: %v", err)
	}
	if err := sq.EnsureSchema(ctx); err != nil {
		t.Fatalf("建 schema 失败: %v", err)
	}
	defer func() { _ = sq.Close() }()

	calls := 0
	pipe, err := pipeline.New(sq.Effects(), time.Minute, []pipeline.Tool{
		{Name: "calc", Safe: true, Handler: func(_ context.Context, _ []byte) (string, error) {
			calls++
			return "ok", nil
		}},
	})
	if err != nil {
		t.Fatalf("建管线失败: %v", err)
	}
	m := &jointFakeModel{script: []chat.ChatResponse{
		{ToolCalls: []chat.ToolCall{{ID: "call-1", Name: "calc", Arguments: "{}"}}},
		{Content: "终答"},
	}}
	loop, err := agentloop.New(agentloop.Config{
		Model: m, Tools: pipe, Store: sq.Snapshots(),
		Joint: sq,
	})
	if err != nil {
		t.Fatalf("建循环失败: %v", err)
	}

	res, err := loop.Run(ctx, "sess-joint", "开始")
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	if res.Status != contract.StatusSucceeded {
		t.Fatalf("应 succeeded，got %s", res.Status)
	}
	if calls != 1 {
		t.Fatalf("工具应执行 1 次，got %d", calls)
	}

	events, err := sq.Events().ListAfter(ctx, 0, 10)
	if err != nil {
		t.Fatalf("读事件失败: %v", err)
	}
	if len(events) != 4 {
		t.Fatalf("一轮应持久化 4 事件（2 round_start+tool_call+session_state），got %d", len(events))
	}
	if events[len(events)-1].Type != agentloop.EventSessionState {
		t.Fatalf("末事件应为 session_state，got %s", events[len(events)-1].Type)
	}
}

// TestJointCommitAtomicOnFailure 原子性直证：联合提交失败（快照 CAS 拒收
// 陈旧快照）⇒ 整批回滚 ⇒ 事件行一个都不落——同生共死的另一半。
func TestJointCommitAtomicOnFailure(t *testing.T) {
	ctx := context.Background()
	sq, err := sqliteadapter.Open(filepath.Join(t.TempDir(), "joint-atomic.db"))
	if err != nil {
		t.Fatalf("开库失败: %v", err)
	}
	if err := sq.EnsureSchema(ctx); err != nil {
		t.Fatalf("建 schema 失败: %v", err)
	}
	defer func() { _ = sq.Close() }()

	joint := sq
	pendings := []eventbus.Event{
		{SessionID: "sess-atomic", Type: "x", Level: "info", OccurredAt: time.Now()},
	}
	// 先落高序快照建基线（seq=2），再试 seq=1 的陈旧快照——CAS 拒收语义。
	if err := joint.CommitWithEvents(ctx, snapshotAtSeq(2, "sess-atomic"), nil); err != nil {
		t.Fatalf("基线快照落库失败: %v", err)
	}
	if err := joint.CommitWithEvents(ctx, snapshotAtSeq(1, "sess-atomic"), pendings); err == nil {
		t.Fatal("陈旧快照必须被 CAS 拒收")
	}
	events, err := sq.Events().ListAfter(ctx, 0, 10)
	if err != nil {
		t.Fatalf("读事件失败: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("联合提交失败后事件行必须零落，got %d", len(events))
	}
}

// snapshotAtSeq 构造指定 seq 的快照（CAS 用例）。
func snapshotAtSeq(seq uint64, runID string) checkpoint.Snapshot {
	return checkpoint.Snapshot{
		RunID:  runID,
		Seq:    seq,
		Status: "succeeded",
		Fields: map[string]string{"history": "[]"},
	}
}
