// cursor_fail_test.go —— 尾随游标**读失败**的判据（Task 5 评审 M2）。
//
// 契约（eventbus.CursorStore 的 LoadCursor）：「error ⇒ Tailer 停摆重试，
// **绝不静默回退 MaxID**」——回退就把停机窗口内的事件整个跳过了（宁可停摆
// 不可跳读）。
//
// 本文件钉住两条：
//
//  1. LoadCursor 读失败必须返 error（**不得**返 found=false + nil error——
//     那会被 Tailer 当成「首次部署」⇒ 回退 MaxID ⇒ 静默跳读）；
//  2. 端到端：真 Tailer 挂真实现，读失败时**一次都不许调用 MaxID**（用计数
//     包装证明「没有回退」——回退即跳过停机窗口）。
//
// 制造失败的手法是**引擎级**的真实故障（关掉只读句柄），不是 mock。
package sqliteadapter

import (
	"context"
	"testing"
	"time"

	"github.com/yi-nology/goagent-kernel/eventbus"
)

// TestTailCursorStoreLoadFailureReturnsError 游标读失败必须返 error。
//
// ⚠ 危险的失败形态是「吞错」：把读失败当成「从未存过游标」⇒ Tailer 回退
// MaxID 初始化 ⇒ 停机窗口内的事件被整个跳过（静默漏帧，不报错不告警）。
func TestTailCursorStoreLoadFailureReturnsError(t *testing.T) {
	ctx := curCtx(t)
	s := openStore(t)
	cs := s.TailCursors("replica-a")
	if err := s.read.Close(); err != nil {
		t.Fatalf("关只读句柄失败: %v", err)
	}
	got, found, err := cs.LoadCursor(ctx)
	if err == nil {
		t.Fatalf("读失败必须返 error（found=%v got=%d）：吞错会让 Tailer 当成「首次部署」回退 MaxID ⇒ 静默跳读",
			found, got)
	}
}

// TestTailCursorReadFailureStallsTailer 端到端：读失败 ⇒ 停摆，**绝不**回退 MaxID。
//
// 判据是「MaxID 一次都没被调用」：Tailer 的初始化顺序是「LoadCursor 命中 ⇒
// 从持久化游标起随；未命中 ⇒ 回退 MaxID」⇒ 只要 LoadCursor 返 error，MaxID
// 就一次都不该被调用（调用了 = 跳过停机窗口 = 静默漏帧）。
//
// 用真 Tailer + 真游标面（读句柄关掉制造真实读失败），不用替身——替身证明的
// 是内核 Tailer 的行为，证不了本包的错误传播。
func TestTailCursorReadFailureStallsTailer(t *testing.T) {
	ctx := curCtx(t)
	s := openStore(t)
	evs := s.Events()
	cs := s.TailCursors("replica-a")
	// 先落一个真实事件（证明「库里确实有帧可投」，不是库空导致的）。
	if _, err := evs.Insert(ctx, eventbus.Event{SessionID: "s", Type: "t"}); err != nil {
		t.Fatalf("插入失败: %v", err)
	}
	// 关掉只读句柄 ⇒ 游标读失败（与生产同形态的存储故障）。
	if err := s.read.Close(); err != nil {
		t.Fatalf("关只读句柄失败: %v", err)
	}

	counted := &countingTailStore{TailStore: evs}
	hub := eventbus.NewHub()
	ch, stop := hub.Subscribe("s")
	defer stop()
	run, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		eventbus.NewTailer(counted, hub,
			eventbus.WithTailInterval(10*time.Millisecond),
			eventbus.WithTailCursorStore(cs),
		).Run(run)
	}()

	select {
	case ev := <-ch:
		t.Fatalf("读失败时不得投递任何帧（静默跳读）：收到 %+v", ev)
	case <-time.After(200 * time.Millisecond):
	}
	if n := counted.maxIDCalls.Load(); n != 0 {
		t.Fatalf("读失败时 Tailer 绝不得回退 MaxID（调了 %d 次 = 跳过停机窗口）", n)
	}
	defer stopTailer(t, cancel, done, "读失败停摆尾随")
}
