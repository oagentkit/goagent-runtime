// pg_event_test.go —— eventbus 可靠事件面（Outbox 形态）的真 PG 判据（issue #264）。
//
// 跳过与硬失败的边界与 pg_test.go 同口径：场景硬失败；唯一的 skip 在环境
// 入口（GAK_PGREF_DSN 未设置，Skipf 写明原因与起法——不会在无库时伪造结果）。
// 机制逻辑另有进程内同构判据（eventbus/tail_cursor_test.go 的形态互证 +
// tail_outbox_test.go 的水位线口径），两处同源互证。
//
// 判据对应 issue #264 验收标准：
//   - TestEventStorePublishInTxSameTransaction  事件面与业务写入同事务（判据四）
//   - 回滚不烧号（判据二的真 PG 形态）；
//   - TestEventStoreWatermarkBoundedRead        水位线读法 fail-closed 口径；
//   - TestEventStoreConcurrentPublishContiguous 并发下铸造序 == 提交可见序；
//   - TestEventStoreRoundTripsEventFields       行→Event 转换不改写字段；
//   - TestEventStoreTailerEndToEnd              Tailer 挂真实现端到端（判据一 + 二）。
package pgstore

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/oekit/pkg/errors"

	"github.com/yi-nology/goagent-kernel/eventbus"
)

// freshEventStore 一个子测试一份全新事件面：独立 schema → 建表（含水位行
// 种子）→ 独立连接池（search_path 已指到该 schema）。t.Cleanup 整块删除。
func freshEventStore(t *testing.T) (*EventStore, *pgxpool.Pool) {
	t.Helper()
	dsn := requireDSN(t)
	admin := basePool(t, dsn)
	ctx := context.Background()
	schema, err := NewSchema(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = DropSchema(ctx, admin, schema) })
	if err := EnsureSchema(ctx, admin, schema); err != nil {
		t.Fatal(err)
	}
	pool := schemaPool(t, dsn, schema)
	return NewEventStore(pool), pool
}

// TestEventStorePublishInTxSameTransaction 事件面与业务写入**同事务**（#264
// 判据四）：提交则事件行与业务状态同现；回滚则两者同灭，且水位线不动、
// ID 不烧号（下一次发布复用同一 ID——判据二的真 PG 形态）。
func TestEventStorePublishInTxSameTransaction(t *testing.T) {
	evs, pool := freshEventStore(t)
	ctx := context.Background()

	// 提交路径：业务写入与事件发布在同一事务——同现。
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	id, err := evs.PublishInTx(ctx, tx, eventbus.Event{SessionID: "s", Type: "committed"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO pgref_snapshot (run_id, seq, status) VALUES ('biz-tx', 1, 'running')`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if id != 1 {
		t.Fatalf("首铸应为 1, got %d", id)
	}
	var seq int64
	if err := pool.QueryRow(ctx,
		`SELECT seq FROM pgref_snapshot WHERE run_id = 'biz-tx'`).Scan(&seq); err != nil {
		t.Fatalf("提交后业务行必须可见: %v", err)
	}
	got, err := evs.ListAfter(ctx, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != 1 || got[0].Type != "committed" {
		t.Fatalf("提交后事件必须可见（水位线读法）, got %+v", got)
	}

	// 在途不可见：水位线与在途行对外停留在上一笔已提交态（提交可见序）。
	tx, err = pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	rolledID, err := evs.PublishInTx(ctx, tx, eventbus.Event{SessionID: "s", Type: "rolled"})
	if err != nil {
		t.Fatal(err)
	}
	if rolledID != 2 {
		t.Fatalf("在途铸造应拿 2, got %d", rolledID)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO pgref_snapshot (run_id, seq, status) VALUES ('biz-rolled', 1, 'running')`); err != nil {
		t.Fatal(err)
	}
	if w, err := evs.CommittedWatermark(ctx); err != nil || w != 1 {
		t.Fatalf("在途事务不得推进可见水位线（got %d, err=%v）", w, err)
	}
	if evs2, err := evs.ListAfter(ctx, 1, 10); err != nil || len(evs2) != 0 {
		t.Fatalf("在途行不得可见（got %d 帧, err=%v）", len(evs2), err)
	}

	// 回滚路径：事件行与业务写入同灭，水位线不动，ID 不烧号。
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT seq FROM pgref_snapshot WHERE run_id = 'biz-rolled'`).Scan(&seq); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("回滚后业务行必须同灭（ErrNoRows）, err=%v", err)
	}
	if w, err := evs.CommittedWatermark(ctx); err != nil || w != 1 {
		t.Fatalf("回滚后水位线必须回到 1（同灭）, got %d err=%v", w, err)
	}
	nextID, err := evs.Insert(ctx, eventbus.Event{SessionID: "s", Type: "reused"})
	if err != nil {
		t.Fatal(err)
	}
	if nextID != 2 {
		t.Fatalf("回滚不烧号：下一次发布必须复用 ID=2（实得 %d——烧号即制造永久空洞）", nextID)
	}
}

// TestEventStoreWatermarkBoundedRead 水位线读法的 fail-closed 口径（#264 可
// 机判口径第二条）：宿主绕过写入协议落的行（id 越过水位线）被扣住不返；
// MaxID 停在无空洞边界——初始化不越过它（返回裸 max(id) = 初始化即跳帧）。
func TestEventStoreWatermarkBoundedRead(t *testing.T) {
	evs, pool := freshEventStore(t)
	ctx := context.Background()
	for _, typ := range []string{"e1", "e2", "e3"} {
		if _, err := evs.Insert(ctx, eventbus.Event{SessionID: "s", Type: typ}); err != nil {
			t.Fatal(err)
		}
	}
	// 绕过协议直插一行（宿主误用形态）：id 越过水位线。
	if _, err := pool.Exec(ctx,
		`INSERT INTO pgref_event (id, session_id, type) VALUES (99, 's', 'out-of-band')`); err != nil {
		t.Fatal(err)
	}
	w, err := evs.CommittedWatermark(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if w != 3 {
		t.Fatalf("水位线应停在 3（绕过协议的行不得推进它）, got %d", w)
	}
	max, err := evs.MaxID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if max != 3 {
		t.Fatalf("MaxID 必须是无空洞边界 3（返回 99 = 初始化即跳帧）, got %d", max)
	}
	got, err := evs.ListAfter(ctx, 0, 100)
	if err != nil {
		t.Fatal(err)
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

// TestEventStoreConcurrentPublishContiguous 并发发布的铸造序判据（#264）：
// 水位行锁串行化下，并发发布拿到的 ID 恰为 1..N 连续无洞，水位线 == 最大
// ID——「铸造序 == 提交可见序」在真 PG 的并发下成立（乱序提交结构性地
// 不存在，这是 Outbox 形态对判据一的机制性回答）。
func TestEventStoreConcurrentPublishContiguous(t *testing.T) {
	evs, _ := freshEventStore(t)
	ctx := context.Background()
	const workers = 8
	ids := make(chan uint64, workers)
	errs := make(chan error, workers)
	for range workers {
		go func() {
			id, err := evs.Insert(ctx, eventbus.Event{SessionID: "s", Type: "race"})
			if err != nil {
				errs <- err
				return
			}
			ids <- id
		}()
	}
	got := make([]uint64, 0, workers)
	for range workers {
		select {
		case id := <-ids:
			got = append(got, id)
		case err := <-errs:
			t.Fatalf("并发发布不得失败: %v", err)
		case <-time.After(10 * time.Second):
			t.Fatal("10s 内未收齐并发发布结果")
		}
	}
	seen := map[uint64]bool{}
	for _, id := range got {
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
		t.Fatal(err)
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

// TestEventStoreRoundTripsEventFields 行→Event 转换不改写字段（TailStore
// 契约第三条）：全字段（SessionID / CaseID / StepID / Type / Level /
// Payload / OccurredAt）往返一致——权威行不是读侧的产物，改写即篡改。
func TestEventStoreRoundTripsEventFields(t *testing.T) {
	evs, _ := freshEventStore(t)
	ctx := context.Background()
	// TIMESTAMPTZ 精度为微秒：纳秒位对齐，往返比较才有确定结论。
	at := time.Date(2026, 11, 17, 9, 30, 0, 123456000, time.UTC)
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
		t.Fatal(err)
	}
	if id != 1 {
		t.Fatalf("首铸应为 1, got %d", id)
	}
	rows, err := evs.ListAfter(ctx, 0, 10)
	if err != nil {
		t.Fatal(err)
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

// TestEventStoreTailerEndToEnd 承诺形态端到端（#264 判据一 + 判据二在真
// PG 上）：eventbus.Tailer 挂在本实现上——中途回滚一铸（不烧号），随后
// 全部提交的帧按铸造序送达，空洞哨兵全程静默（不触发永久丢失判据）。
func TestEventStoreTailerEndToEnd(t *testing.T) {
	evs, pool := freshEventStore(t)
	ctx := context.Background()

	publish := func(typ string) {
		t.Helper()
		if _, err := evs.Insert(ctx, eventbus.Event{SessionID: "s", Type: typ}); err != nil {
			t.Fatalf("发布 %s 失败: %v", typ, err)
		}
	}
	rollbackOne := func(typ string) {
		t.Helper()
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := evs.PublishInTx(ctx, tx, eventbus.Event{SessionID: "s", Type: typ}); err != nil {
			t.Fatal(err)
		}
		if err := tx.Rollback(ctx); err != nil {
			t.Fatal(err)
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
	tctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		eventbus.NewTailer(evs, hub,
			eventbus.WithTailInterval(10*time.Millisecond),
			eventbus.WithTailStartCursor(0),
			eventbus.WithTailOnGap(func(from, to uint64) { gaps <- [2]uint64{from, to} }),
		).Run(tctx)
	}()

	for _, want := range []struct {
		id  uint64
		typ string
	}{{1, "e1"}, {2, "e2"}, {3, "e3"}} {
		select {
		case ev := <-ch:
			if ev.ID != want.id || ev.Type != want.typ {
				t.Fatalf("应按铸造序送达 %d(%s), got %d(%s)——回滚空洞被跳过或读法违约",
					want.id, want.typ, ev.ID, ev.Type)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("2s 内未收到帧 %d(%s)——回滚空洞被跳过或读法违约", want.id, want.typ)
		}
	}
	publish("e4") // 4：尾随进行中继续发布
	select {
	case ev := <-ch:
		if ev.ID != 4 || ev.Type != "e4" {
			t.Fatalf("后续发布应照常补达, got %d(%s)", ev.ID, ev.Type)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("2s 内未收到 e4（尾随停摆）")
	}
	select {
	case g := <-gaps:
		t.Fatalf("回滚空洞不得触发永久丢失判据（哨兵静默）: [%d,%d]", g[0], g[1])
	case <-time.After(100 * time.Millisecond):
	}
	defer stopTailer(t, cancel, done, "端到端尾随")
}
