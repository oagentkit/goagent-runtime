// event_tail_test.go —— 事件面挂 eventbus.Tailer 的端到端判据。
//
// 对位 PG 侧 pg_event_test.go 的 TestEventStoreTailerEndToEnd：承诺形态端到端
// ——中途回滚一铸（不烧号），随后全部提交的帧按**铸造序**送达，空洞哨兵全程
// 静默（回滚空洞不是「跳号」，哨兵不得响）。
//
// 为什么这条不能省：TailStore 的「无空洞推进」是**机制**保证的，不是观测保证
// 的——「读失败游标不动 ⇒ 不丢」的完整前提是「ID 序 == 提交可见序」。只有把
// 真实现挂上真 Tailer 跑一遍，才验得到「尾随读侧确实按铸造序补灌」。
package sqliteadapter

import (
	"context"
	"testing"
	"time"

	"github.com/yi-nology/goagent-kernel/eventbus"
)

// TestEventTailerEndToEnd 承诺形态端到端：尾随按铸造序补灌，回滚不造成空洞。
func TestEventTailerEndToEnd(t *testing.T) {
	ctx := evtCtx(t)
	s := openStore(t)
	evs := s.Events()

	publish := func(typ string) {
		t.Helper()
		if _, err := evs.Insert(ctx, eventbus.Event{SessionID: "s", Type: typ}); err != nil {
			t.Fatalf("发布 %s 失败: %v", typ, err)
		}
	}
	rollbackOne := func(typ string) {
		t.Helper()
		tx, err := s.DB().BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("开事务失败: %v", err)
		}
		if _, err := evs.PublishInTx(ctx, tx, eventbus.Event{SessionID: "s", Type: typ}); err != nil {
			_ = tx.Rollback()
			t.Fatalf("事务内发布 %s 失败: %v", typ, err)
		}
		if err := tx.Rollback(); err != nil {
			t.Fatalf("回滚失败: %v", err)
		}
	}

	publish("e1")       // 1
	rollbackOne("gone") // 铸 2 后回滚：不烧号
	publish("e2")       // 2（复用回滚号）
	publish("e3")       // 3

	hub := eventbus.NewHub()
	ch, stop := hub.Subscribe("s")
	defer stop()

	gaps := make(chan [2]uint64, 8)
	run, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		eventbus.NewTailer(evs, hub,
			eventbus.WithTailInterval(10*time.Millisecond),
			eventbus.WithTailStartCursor(0),
			eventbus.WithTailOnGap(func(from, to uint64) { gaps <- [2]uint64{from, to} }),
		).Run(run)
	}()

	for _, want := range []struct {
		id  uint64
		typ string
	}{{1, "e1"}, {2, "e2"}, {3, "e3"}} {
		ev := waitTailEvent(t, ch, "尾随帧")
		if ev.ID != want.id || ev.Type != want.typ {
			t.Fatalf("应按铸造序送达 %d(%s), got %d(%s)——回滚空洞被跳过或读法违约",
				want.id, want.typ, ev.ID, ev.Type)
		}
	}
	publish("e4") // 4：尾随进行中继续发布
	ev := waitTailEvent(t, ch, "后续帧")
	if ev.ID != 4 || ev.Type != "e4" {
		t.Fatalf("后续发布应照常补达, got %d(%s)", ev.ID, ev.Type)
	}
	assertNoGap(t, gaps, 100*time.Millisecond)
	defer stopTailer(t, cancel, done, "端到端尾随")
}
