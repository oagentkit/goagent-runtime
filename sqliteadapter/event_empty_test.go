// event_empty_test.go —— 事件面读侧的**空库形态**与**失败形态**判据。
//
// 本文件钉住两条此前只有注释、没有判据的承重行为（Task 5 评审 M1）：
//
//  1. **空库 ⇒ 水位线读成 0，不返 error**：事件面在 SQLite 侧的水位行是
//     **惰性建出**的（PG 侧在建表时种下 tenant=” 的那行）⇒ 全新部署的库里
//     根本没有那一行。契约明写「MaxID …（空表 = 0）」⇒ 缺行必须读成 0，
//     而不是「no rows」错误。若有人把 COALESCE(...)「向 PG 对齐」改成裸
//     SELECT committed …，全新部署的 Tailer 初始化就会拿到 sql: no rows
//     ⇒ 停摆 ⇒ 静默不投递——而那正是本条判据要挡的形态。
//  2. **读失败 ⇒ error，且不得返回半个水位线 / 部分结果**：契约明写「读失败
//     返回 error 且不得返回部分结果」⇒ 行扫描中途出错即整体作废（连已经读到
//     的前半段也不返）。
//
// 制造失败的手法都是**引擎级**的真实故障，不是 mock：关掉只读句柄（读路径
// 整体失败）/ 故意写坏一行内容（行扫描中途失败）。
package sqliteadapter

import (
	"testing"

	"github.com/yi-nology/goagent-kernel/eventbus"
)

// TestEventWatermarkOnEmptyStoreIsZero 空库上的水位线必须读成 0（**不返 error**）。
//
// 契约：eventbus.TailStore 的 MaxID「空表 = 0」；WatermarkTailStore 的
// CommittedWatermark「读失败返回 error 且不得返回半个水位线」⇒ 「缺行」不是
// 「读失败」，是「还没有事件」。
func TestEventWatermarkOnEmptyStoreIsZero(t *testing.T) {
	ctx := evtCtx(t)
	s := openStore(t)
	evs := s.Events()

	// 前置条件：水位行尚未被惰性建出（全新部署的库里没有那一行）。
	var rows int
	if err := s.DB().QueryRowContext(ctx,
		`SELECT count(*) FROM gak_event_watermark`).Scan(&rows); err != nil {
		t.Fatalf("查水位行数失败: %v", err)
	}
	if rows != 0 {
		t.Fatalf("本用例的前置条件不成立：水位行已有 %d 行（惰性建行应发生在首次发布）", rows)
	}

	max, err := evs.MaxID(ctx)
	if err != nil {
		t.Fatalf("空库读 MaxID 必须返 0 且不返 error（缺行不是读失败），实得 err=%v", err)
	}
	if max != 0 {
		t.Fatalf("空库 MaxID 必须为 0，实得 %d", max)
	}
	wm, err := evs.CommittedWatermark(ctx)
	if err != nil {
		t.Fatalf("空库读已提交水位线必须返 0 且不返 error，实得 err=%v", err)
	}
	if wm != 0 {
		t.Fatalf("空库已提交水位线必须为 0，实得 %d", wm)
	}
	// 空库取段：空段 + 无 error（不是「读失败」）。
	seg, err := evs.ListAfter(ctx, 0, 10)
	if err != nil {
		t.Fatalf("空库取段不得报错: %v", err)
	}
	if len(seg) != 0 {
		t.Fatalf("空库取段必须为空，实得 %d 帧", len(seg))
	}
}

// TestEventReadFailureReturnsError 读失败必须返 error（不得返半个水位线）。
//
// 把只读句柄关掉 ⇒ 读路径整体失败。这是**引擎级**的失败形态（存储句柄不可
// 用），不是 mock：调用方拿到的必须是 error，否则它会把「读不出来」当成
// 「水位线是 0」⇒ 无空洞边界被当成 0 ⇒ 初始化从 0 起随 ⇒ 重放全量存量。
func TestEventReadFailureReturnsError(t *testing.T) {
	ctx := evtCtx(t)
	s := openStore(t)
	evs := s.Events()
	if err := s.read.Close(); err != nil {
		t.Fatalf("关只读句柄失败: %v", err)
	}

	if got, err := evs.MaxID(ctx); err == nil {
		t.Fatalf("读失败时 MaxID 必须返 error（实得 %d——调用方会把它当「无空洞边界 0」用）", got)
	}
	if got, err := evs.CommittedWatermark(ctx); err == nil {
		t.Fatalf("读失败时已提交水位线必须返 error（不得返半个水位线），实得 %d", got)
	}
	seg, err := evs.ListAfter(ctx, 0, 10)
	if err == nil {
		t.Fatalf("读失败时取段必须返 error，实得 %d 帧", len(seg))
	}
	if seg != nil {
		t.Fatalf("读失败不得返回部分结果，实得 %d 帧: %+v", len(seg), seg)
	}
}

// TestEventListAfterRowScanFailureDiscardsPartial 行扫描中途失败 ⇒ 整体作废。
//
// 契约第四条：「读失败返回 error 且**不得返回部分结果**」——行扫描中途出错即
// 整体作废，**连已经读到的前半段也不返**（返回前半段会让「失败」与「成功
// 读到前半段」不可分 ⇒ 调用方无法判断该不该推进游标）。
//
// 这条判据只能靠「内容损坏的行」逼出来：故意把第二行的事件时刻写成非数值
// （SQLite 动态类型 ⇒ 存成 TEXT）⇒ 扫到它时类型转换失败。
func TestEventListAfterRowScanFailureDiscardsPartial(t *testing.T) {
	ctx := evtCtx(t)
	s := openStore(t)
	evs := s.Events()
	for i := 0; i < 2; i++ {
		if _, err := evs.Insert(ctx, eventbus.Event{SessionID: "s", Type: "t"}); err != nil {
			t.Fatalf("插入失败: %v", err)
		}
	}
	// 故意损坏第二行的事件时刻（INTEGER 列写入非数值 ⇒ SQLite 按 TEXT 存）。
	if _, err := s.DB().ExecContext(ctx,
		`UPDATE gak_event SET occurred_at = 'not-a-number' WHERE id = 2`); err != nil {
		t.Fatalf("制造损坏行失败: %v", err)
	}
	seg, err := evs.ListAfter(ctx, 0, 10)
	if err == nil {
		t.Fatalf("行扫描失败必须返 error，实得 %d 帧: %+v", len(seg), seg)
	}
	if seg != nil {
		t.Fatalf("行扫描失败不得返回部分结果（前半段也不返），实得 %d 帧: %+v", len(seg), seg)
	}
}
