// event_fields_test.go —— 事件行的字段往返与零值归一判据。
//
// 对位 PG 侧 pg_event_test.go 的 TestEventStoreRoundTripsEventFields：行→Event
// 转换不改写字段（TailStore 契约第三条）——权威行不是读侧的产物，改写即篡改。
//
// 另两条落库形态判据：零值 OccurredAt 由**存储时钟**补齐（事件是跨进程共享的
// 事实，发生时刻读唯一权威时钟源，调用方带了时刻则原样落库）；零值 Level /
// Payload 归一后落库（落库行与广播帧同一内容，读回不得引入 NULL 态）。
package sqliteadapter

import (
	"testing"
	"time"

	"github.com/oagentkit/goagent-kernel/eventbus"
)

// TestEventRoundTripsEventFields 行→Event 转换不改写字段（TailStore 契约第三条）：
// 全字段往返一致——权威行不是读侧的产物，改写即篡改。
func TestEventRoundTripsEventFields(t *testing.T) {
	ctx := evtCtx(t)
	evs := openStore(t).Events()
	// SQLite 存储时钟是毫秒级 ⇒ 往返比较用毫秒对齐的时刻。
	at := time.Date(2026, 11, 17, 9, 30, 0, 123000000, time.UTC)
	step := uint64(7)
	in := eventbus.Event{
		SessionID:  "sess-1",
		CaseID:     "case-1",
		StepID:     &step,
		Type:       "step_finished",
		Level:      "warn",
		Payload:    map[string]any{"k": "v", "n": float64(3), "b": true},
		OccurredAt: at,
	}
	id, err := evs.Insert(ctx, in)
	if err != nil {
		t.Fatalf("插入失败: %v", err)
	}
	if id != 1 {
		t.Fatalf("首铸应为 1, got %d", id)
	}
	rows, err := evs.ListAfter(ctx, 0, 10)
	if err != nil {
		t.Fatalf("读事件段失败: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("应读回 1 帧, got %d", len(rows))
	}
	got := rows[0]
	if got.SessionID != in.SessionID || got.CaseID != in.CaseID || got.Type != in.Type ||
		got.Level != in.Level || !got.OccurredAt.Equal(in.OccurredAt) {
		t.Fatalf("字段往返必须一致:\n in=%+v\nout=%+v", in, got)
	}
	if got.StepID == nil || *got.StepID != step {
		t.Fatalf("StepID 往返必须一致, got %v", got.StepID)
	}
	if got.Payload["k"] != "v" || got.Payload["b"] != true {
		t.Fatalf("Payload 往返必须一致, got %+v", got.Payload)
	}
	if n, ok := got.Payload["n"].(float64); !ok || n != 3 {
		t.Fatalf("Payload 数值往返应经 JSON 归一为 float64(3), got %#v", got.Payload["n"])
	}
}

// TestEventOccurredAtDefaultsToStorageClock 零值 OccurredAt 由**存储时钟**补齐；
// 调用方带了时刻则原样落库（不改写语义字段）。
func TestEventOccurredAtDefaultsToStorageClock(t *testing.T) {
	ctx := evtCtx(t)
	s := openStore(t)
	evs := s.Events()
	before := time.Now().UTC().Add(-2 * time.Second)
	if _, err := evs.Insert(ctx, eventbus.Event{SessionID: "s", Type: "no-time"}); err != nil {
		t.Fatalf("插入失败: %v", err)
	}
	at := time.Date(2026, 11, 17, 9, 30, 0, 0, time.UTC)
	if _, err := evs.Insert(ctx, eventbus.Event{SessionID: "s", Type: "with-time", OccurredAt: at}); err != nil {
		t.Fatalf("插入失败: %v", err)
	}
	rows, err := evs.ListAfter(ctx, 0, 10)
	if err != nil {
		t.Fatalf("读事件段失败: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("应读回 2 帧, got %d", len(rows))
	}
	if rows[0].OccurredAt.IsZero() {
		t.Fatal("零值 OccurredAt 必须由存储时钟补齐（不得落零值时刻）")
	}
	if rows[0].OccurredAt.Before(before) {
		t.Fatalf("存储时钟补齐的时刻应在当前附近: %v", rows[0].OccurredAt)
	}
	if !rows[1].OccurredAt.Equal(at) {
		t.Fatalf("调用方给的时刻必须原样落库: got %v want %v", rows[1].OccurredAt, at)
	}
}

// TestEventLevelAndPayloadDefaults 零值 Level 归一为 info、零值 Payload 归一为空对象。
//
// 与 DurablePublisher 的零值归一同口径（eventbus/durable.go）：落库行与广播帧
// 同一内容，读回不得引入 NULL 态。
func TestEventLevelAndPayloadDefaults(t *testing.T) {
	ctx := evtCtx(t)
	evs := openStore(t).Events()
	if _, err := evs.Insert(ctx, eventbus.Event{SessionID: "s", Type: "t"}); err != nil {
		t.Fatalf("插入失败: %v", err)
	}
	rows, err := evs.ListAfter(ctx, 0, 10)
	if err != nil {
		t.Fatalf("读事件段失败: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("应读回 1 帧, got %d", len(rows))
	}
	if rows[0].Level != "info" {
		t.Fatalf("零值 Level 必须归一为 info, got %q", rows[0].Level)
	}
	if rows[0].Payload == nil {
		t.Fatal("零值 Payload 必须归一为空对象（不得读回 NULL）")
	}
	if len(rows[0].Payload) != 0 {
		t.Fatalf("空对象载荷应无键, got %+v", rows[0].Payload)
	}
}
