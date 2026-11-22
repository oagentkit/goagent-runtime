// cursor_test.go —— 尾随游标持久化（eventbus.CursorStore）的判据。
//
// 对位 pgstore（v0.2.0 自 goagent-kernel 迁入）/cursor.go（PG 侧 pg_cursor_test.go）：一行一个消费者
// key、游标**单调不减**、读失败即停摆（不静默回退 MaxID）。
//
// 语义要点：
//   - Load 未命中 = found=false（首次部署）；读失败返回 error ⇒ Tailer 停摆
//     重试，**绝不**静默回退 MaxID（回退即跳过停机窗口）。
//   - SaveCursor 单调不减：陈旧副本的迟到保存不得把游标拉回去；等值保存幂等。
//   - 不同 consumer key 互不干扰（共用一条会让后跑的推过对方整段窗口）。
package sqliteadapter

import (
	"context"
	"testing"
	"time"
)

// curCtx 用例的 ctx（带超时：回归要**快速红**，而不是挂在 60s 超时上）。
func curCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// TestTailCursorStoreNotFoundOnFirstDeploy 首次部署未存过游标 ⇒ found=false。
func TestTailCursorStoreNotFoundOnFirstDeploy(t *testing.T) {
	ctx := curCtx(t)
	cs := openStore(t).TailCursors("replica-a")
	got, found, err := cs.LoadCursor(ctx)
	if err != nil {
		t.Fatalf("读游标失败: %v", err)
	}
	if found {
		t.Fatalf("首次部署必须 found=false，实得 %v（游标=%d）", found, got)
	}
	if got != 0 {
		t.Fatalf("未命中时游标应为 0，实得 %d", got)
	}
}

// TestTailCursorStoreMonotonicSave 游标单调不减：陈旧副本的迟到保存不得把游标
// 拉回去；等值保存幂等；不同 consumer key 互不干扰。
func TestTailCursorStoreMonotonicSave(t *testing.T) {
	ctx := curCtx(t)
	s := openStore(t)
	cs := s.TailCursors("replica-a")

	if err := cs.SaveCursor(ctx, 5); err != nil {
		t.Fatalf("保存游标失败: %v", err)
	}
	if err := cs.SaveCursor(ctx, 3); err != nil { // 迟到回退写：吸收，不落库
		t.Fatalf("回退写不得报错: %v", err)
	}
	got, found, err := cs.LoadCursor(ctx)
	if err != nil || !found || got != 5 {
		t.Fatalf("回退写不得拉低游标：found=%v got=%d err=%v", found, got, err)
	}
	if err := cs.SaveCursor(ctx, 5); err != nil { // 等值保存：幂等
		t.Fatalf("等值保存不得报错: %v", err)
	}
	if got, _, _ := cs.LoadCursor(ctx); got != 5 {
		t.Fatalf("等值保存后游标应仍为 5, got %d", got)
	}

	other := s.TailCursors("replica-b")
	if _, found, err := other.LoadCursor(ctx); err != nil || found {
		t.Fatalf("不同 consumer key 必须互不干扰（应 found=false）：found=%v err=%v", found, err)
	}
}

// TestTailCursorStoreRejectsEmptyConsumer 空 consumer key 是装配期编程错误：
// 所有尾随者共用一条游标 ⇒ 后跑的推过对方整段窗口 ⇒ 静默漏帧。
//
// fail fast 在构造点（与 PG 侧 NewTailCursorStore、eventbus.NewTailer 同口径）：
// 不留到运行时。
func TestTailCursorStoreRejectsEmptyConsumer(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("空 consumer key 必须 panic（装配期编程错误不容静默通过）")
		}
	}()
	openStore(t).TailCursors("")
}

// TestTailCursorStoreRejectsOverflow 超出 BIGINT 范围的游标必须显式报错。
//
// 游标列是 INTEGER（有符号 64 位）：uint64 的高位一旦被截断成负数，读出来
// 的游标会倒退 ⇒ 尾随从停摆点**回退** ⇒ 重放整段（比漏帧更难排查）。
func TestTailCursorStoreRejectsOverflow(t *testing.T) {
	ctx := curCtx(t)
	cs := openStore(t).TailCursors("replica-a")
	if err := cs.SaveCursor(ctx, uint64(1<<63)); err == nil {
		t.Fatal("超出 BIGINT 范围的游标必须报错（截断成负数会让游标倒退）")
	}
	if _, found, err := cs.LoadCursor(ctx); err != nil || found {
		t.Fatalf("越界保存不得落库：found=%v err=%v", found, err)
	}
}
