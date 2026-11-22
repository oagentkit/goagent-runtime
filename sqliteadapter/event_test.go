// event_test.go —— 事件面（eventbus.Store / TailStore / WatermarkTailStore）的判据。
//
// 对位 pgstore（v0.2.0 自 goagent-kernel 迁入）/event.go（PG 侧 pg_event_test.go）：同一个 Outbox 形态，
// 两套方言。核心机制只有两条，其余全是它的推论：
//
//   - **事件 ID 在水位行上铸造**（gak_event_watermark 每租户一行）：
//     UPDATE … RETURNING 拿号的同时持行锁到提交 ⇒ 铸造序 == 提交序；
//   - **回滚随事务回滚** ⇒ 不烧号 ⇒ 序列空洞结构性地不存在。
//
// 读侧的口径（TailStore 契约）：返回段以**已提交水位线**为上界
// （id > cursor AND id ≤ 水位线）——水位线之上哪怕已可见也扣住不返
// （fail-closed 方向是扣住，不是放行）。
//
// ⚠ **已提交水位线 ≠ 事件表 MAX(id)**：水位线读水位行，MAX(id) 读事件行；
// 上界关系是「**水位线 ≤ MAX(id)**」（宿主绕过写入协议落的行会把 MAX(id)
// 顶高，但不得推进水位线）。这条关系由 TestEventWatermarkNeverExceedsEventMax
// 钉住。
package sqliteadapter

import (
	"context"
	"database/sql"
	"sync"
	"testing"
	"time"

	"github.com/oekit/pkg/errors"

	"github.com/yi-nology/goagent-kernel/eventbus"
)

// evtCtx 用例的 ctx（带超时：回归要**快速红**，而不是挂在 60s 超时上）。
func evtCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// TestEventIDsContiguousNoGap 事件 ID 连续（铸造序 == 提交序）。
func TestEventIDsContiguousNoGap(t *testing.T) {
	ctx := evtCtx(t)
	evs := openStore(t).Events()
	for i := uint64(1); i <= 5; i++ {
		got, err := evs.Insert(ctx, eventbus.Event{SessionID: "s", Type: "t"})
		if err != nil {
			t.Fatalf("插入事件失败: %v", err)
		}
		if got != i {
			t.Fatalf("事件 ID 不连续: 期望 %d 实得 %d", i, got)
		}
	}
}

// TestEventRollbackDoesNotBurnID 回滚不烧号：回滚后拿到的号与回滚前相同。
//
// 这是「序列空洞结构性不存在」的落库形态证明：拿号与插行、业务写入全在同一
// 事务里，回滚一并消失 ⇒ 下一次分配复用同一 ID。对比自增序列：回滚烧号 ⇒
// 永久空洞 ⇒ 水位线读法在那里永久停摆（后续帧永不送达，与丢帧等效）。
func TestEventRollbackDoesNotBurnID(t *testing.T) {
	ctx := evtCtx(t)
	s := openStore(t)
	evs := s.Events()
	first, err := evs.Insert(ctx, eventbus.Event{SessionID: "s", Type: "t"})
	if err != nil {
		t.Fatalf("插入失败: %v", err)
	}
	// 在事务内铸造后回滚。
	tx, err := s.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("开事务失败: %v", err)
	}
	burned, err := evs.PublishInTx(ctx, tx, eventbus.Event{SessionID: "s", Type: "t"})
	if err != nil {
		_ = tx.Rollback()
		t.Fatalf("事务内发布失败: %v", err)
	}
	if burned != first+1 {
		_ = tx.Rollback()
		t.Fatalf("在途铸造应拿 %d，实得 %d", first+1, burned)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("回滚失败: %v", err)
	}
	// 回滚同灭：水位线回到已提交的那一档（拿号随事务一起消失）。
	wm, err := evs.CommittedWatermark(ctx)
	if err != nil {
		t.Fatalf("读水位线失败: %v", err)
	}
	if wm != first {
		t.Fatalf("回滚后水位线必须回到 %d，实得 %d", first, wm)
	}
	next, err := evs.Insert(ctx, eventbus.Event{SessionID: "s", Type: "t"})
	if err != nil {
		t.Fatalf("插入失败: %v", err)
	}
	if next != burned {
		t.Fatalf("回滚烧了号: 回滚前 %d 回滚后 %d（回滚后应复用）", burned, next)
	}
	wm2, err := evs.CommittedWatermark(ctx)
	if err != nil {
		t.Fatalf("读水位线失败: %v", err)
	}
	if wm2 != burned {
		t.Fatalf("复用后水位线应推进到 %d，实得 %d", burned, wm2)
	}
}

// TestEventPublishInTxSameTransaction 事件面与业务写入**同事务**：
// 提交则同现、回滚则同灭；在途不可见（水位线停在上一笔已提交态）。
func TestEventPublishInTxSameTransaction(t *testing.T) {
	ctx := evtCtx(t)
	s := openStore(t)
	evs := s.Events()

	// 提交路径：业务写入与事件发布在同一事务——同现。
	tx, err := s.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("开事务失败: %v", err)
	}
	id, err := evs.PublishInTx(ctx, tx, eventbus.Event{SessionID: "s", Type: "committed"})
	if err != nil {
		_ = tx.Rollback()
		t.Fatalf("事务内发布失败: %v", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO gak_snapshot (run_id, seq, status) VALUES ('biz-tx', 1, 'running')`); err != nil {
		_ = tx.Rollback()
		t.Fatalf("写业务行失败: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("提交失败: %v", err)
	}
	if id != 1 {
		t.Fatalf("首铸应为 1，实得 %d", id)
	}
	var seq int64
	if err := s.DB().QueryRowContext(ctx,
		`SELECT seq FROM gak_snapshot WHERE run_id = 'biz-tx'`).Scan(&seq); err != nil {
		t.Fatalf("提交后业务行必须可见: %v", err)
	}
	got, err := evs.ListAfter(ctx, 0, 10)
	if err != nil {
		t.Fatalf("读事件段失败: %v", err)
	}
	if len(got) != 1 || got[0].ID != 1 || got[0].Type != "committed" {
		t.Fatalf("提交后事件必须可见（水位线读法）, got %+v", got)
	}

	// 在途不可见：水位线与在途行对外停留在上一笔已提交态。
	tx, err = s.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("开事务失败: %v", err)
	}
	rolledID, err := evs.PublishInTx(ctx, tx, eventbus.Event{SessionID: "s", Type: "rolled"})
	if err != nil {
		_ = tx.Rollback()
		t.Fatalf("事务内发布失败: %v", err)
	}
	if rolledID != 2 {
		_ = tx.Rollback()
		t.Fatalf("在途铸造应拿 2，实得 %d", rolledID)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO gak_snapshot (run_id, seq, status) VALUES ('biz-rolled', 1, 'running')`); err != nil {
		_ = tx.Rollback()
		t.Fatalf("写业务行失败: %v", err)
	}
	if w, err := evs.CommittedWatermark(ctx); err != nil || w != 1 {
		_ = tx.Rollback()
		t.Fatalf("在途事务不得推进可见水位线（got %d, err=%v）", w, err)
	}
	if evs2, err := evs.ListAfter(ctx, 1, 10); err != nil || len(evs2) != 0 {
		_ = tx.Rollback()
		t.Fatalf("在途行不得可见（got %d 帧, err=%v）", len(evs2), err)
	}

	// 回滚路径：事件行与业务写入同灭，水位线不动，ID 不烧号。
	if err := tx.Rollback(); err != nil {
		t.Fatalf("回滚失败: %v", err)
	}
	if err := s.DB().QueryRowContext(ctx,
		`SELECT seq FROM gak_snapshot WHERE run_id = 'biz-rolled'`).Scan(&seq); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("回滚后业务行必须同灭（ErrNoRows）, err=%v", err)
	}
	if w, err := evs.CommittedWatermark(ctx); err != nil || w != 1 {
		t.Fatalf("回滚后水位线必须回到 1（同灭）, got %d err=%v", w, err)
	}
	nextID, err := evs.Insert(ctx, eventbus.Event{SessionID: "s", Type: "reused"})
	if err != nil {
		t.Fatalf("插入失败: %v", err)
	}
	if nextID != 2 {
		t.Fatalf("回滚不烧号：下一次发布必须复用 ID=2（实得 %d——烧号即制造永久空洞）", nextID)
	}
}

// TestEventWatermarkBoundedRead 水位线读法的 fail-closed 口径：
// 宿主绕过写入协议落的行（id 越过水位线）被扣住不返；MaxID 停在无空洞边界。
func TestEventWatermarkBoundedRead(t *testing.T) {
	ctx := evtCtx(t)
	s := openStore(t)
	evs := s.Events()
	for _, typ := range []string{"e1", "e2", "e3"} {
		if _, err := evs.Insert(ctx, eventbus.Event{SessionID: "s", Type: typ}); err != nil {
			t.Fatalf("插入失败: %v", err)
		}
	}
	// 绕过协议直插一行（宿主误用形态）：id 越过水位线。
	if _, err := s.DB().ExecContext(ctx,
		`INSERT INTO gak_event (tenant, id, session_id, type) VALUES ('', 99, 's', 'out-of-band')`); err != nil {
		t.Fatalf("造越界行失败: %v", err)
	}
	w, err := evs.CommittedWatermark(ctx)
	if err != nil {
		t.Fatalf("读水位线失败: %v", err)
	}
	if w != 3 {
		t.Fatalf("水位线应停在 3（绕过协议的行不得推进它）, got %d", w)
	}
	max, err := evs.MaxID(ctx)
	if err != nil {
		t.Fatalf("读 MaxID 失败: %v", err)
	}
	if max != 3 {
		t.Fatalf("MaxID 必须是无空洞边界 3（返回 99 = 初始化即跳帧）, got %d", max)
	}
	got, err := evs.ListAfter(ctx, 0, 100)
	if err != nil {
		t.Fatalf("读事件段失败: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("水位线之上的行必须扣住不返（fail-closed）, got %d 帧: %+v", len(got), got)
	}
	for i, ev := range got {
		if ev.ID != uint64(i+1) {
			t.Fatalf("返回段必须从 cursor+1 起连续, got %+v", got)
		}
	}
}

// TestEventWatermarkNeverExceedsEventMax 已提交水位线与事件表 MAX(id) 的关系。
//
// ⚠ **两者不是同一个数**：水位线读水位行、MAX(id) 读事件行。上界关系是
// 「**水位线 ≤ MAX(id)**」——正常协议写入下两者相等；宿主绕过协议落的行把
// MAX(id) 顶高，但**不得**推进水位线（否则无空洞边界被推到在途小 ID 之上）。
//
// 这一条是本轮实测挖出来的形态，写死它 ⇒ 将来谁把读法改成「读 MAX(id) 当
// 水位线」，用例会红。
func TestEventWatermarkNeverExceedsEventMax(t *testing.T) {
	ctx := evtCtx(t)
	s := openStore(t)
	evs := s.Events()
	for i := 0; i < 3; i++ {
		if _, err := evs.Insert(ctx, eventbus.Event{SessionID: "s", Type: "t"}); err != nil {
			t.Fatalf("插入失败: %v", err)
		}
	}
	wm, err := evs.CommittedWatermark(ctx)
	if err != nil {
		t.Fatalf("读水位线失败: %v", err)
	}
	var maxID int64
	if err := s.DB().QueryRowContext(ctx,
		`SELECT COALESCE(MAX(id), 0) FROM gak_event`).Scan(&maxID); err != nil {
		t.Fatalf("读事件表最大值失败: %v", err)
	}
	if int64(wm) > maxID {
		t.Fatalf("水位线(%d) 不得高于事件表最大 ID(%d)", wm, maxID)
	}
	if int64(wm) != maxID {
		t.Fatalf("正常协议写入下两者应同源: 水位线=%d 事件表最大=%d", wm, maxID)
	}
	// 绕过协议落一行 ⇒ MAX(id) 顶高、水位线不动 ⇒ 两者**不再相等**（上界关系仍在）。
	if _, err := s.DB().ExecContext(ctx,
		`INSERT INTO gak_event (tenant, id, session_id, type) VALUES ('', 50, 's', 'out-of-band')`); err != nil {
		t.Fatalf("造越界行失败: %v", err)
	}
	wm2, err := evs.CommittedWatermark(ctx)
	if err != nil {
		t.Fatalf("读水位线失败: %v", err)
	}
	var maxID2 int64
	if err := s.DB().QueryRowContext(ctx,
		`SELECT COALESCE(MAX(id), 0) FROM gak_event`).Scan(&maxID2); err != nil {
		t.Fatalf("读事件表最大值失败: %v", err)
	}
	if int64(wm2) > maxID2 {
		t.Fatalf("水位线(%d) 不得高于事件表最大 ID(%d)", wm2, maxID2)
	}
	if int64(wm2) == maxID2 {
		t.Fatalf("越界行不得推进水位线: 水位线=%d 事件表最大=%d（两者仍相等说明水位线被越界行推进了）", wm2, maxID2)
	}
}

// TestEventConcurrentPublishContiguous 并发发布的铸造序判据：
// 水位行锁串行化下，并发发布拿到的 ID 恰为 1..N 连续无洞。
func TestEventConcurrentPublishContiguous(t *testing.T) {
	ctx := evtCtx(t)
	evs := openStore(t).Events()
	const workers = 8
	ids := make(chan uint64, workers)
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, err := evs.Insert(ctx, eventbus.Event{SessionID: "s", Type: "race"})
			if err != nil {
				errs <- err
				return
			}
			ids <- id
		}()
	}
	wg.Wait()
	close(ids)
	close(errs)
	for err := range errs {
		t.Fatalf("并发发布不得失败: %v", err)
	}
	seen := map[uint64]bool{}
	for id := range ids {
		if id < 1 || id > workers || seen[id] {
			t.Fatalf("并发铸造的 ID 应恰为 1..%d 且不重复, 出现 %d", workers, id)
		}
		seen[id] = true
	}
	if len(seen) != workers {
		t.Fatalf("应收齐 %d 个不同 ID, got %d", workers, len(seen))
	}
	w, err := evs.CommittedWatermark(ctx)
	if err != nil {
		t.Fatalf("读水位线失败: %v", err)
	}
	if w != workers {
		t.Fatalf("并发提交后水位线应为 %d, got %d", workers, w)
	}
	rows, err := evs.ListAfter(ctx, 0, workers)
	if err != nil || len(rows) != workers {
		t.Fatalf("水位线读法应一次取回全部 %d 帧（连续无洞）, got %d err=%v", workers, len(rows), err)
	}
	for i, ev := range rows {
		if ev.ID != uint64(i+1) {
			t.Fatalf("返回段必须连续无洞, got %+v", rows)
		}
	}
}
