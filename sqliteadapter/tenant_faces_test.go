// tenant_faces_test.go —— 租户作用域的**逐面隔离**判据。
//
// 一个面一个面地核：租户作用域打开后，各面的存储键是否真的带上了租户
// （带上了 ⇒ 跨租户同 Key / 同 resource / 同 consumer 互不命中）。
//
// 判据落点：
//   - 副作用面：键是 (tenant, key)；
//   - 事件面：水位行按租户分行 ⇒ 每个租户一条独立 ID 空间与流；
//   - 栅栏面：键是 (tenant, resource)；
//   - 游标面：物理行无 tenant 列 ⇒ 隔离靠**派生键**（tenantCursorKey）。
//
// 缺租户一律 fail-closed（tenant.ErrNoContext）：缺租户是**拒绝**，不是回落到
// 默认命名空间（回落 = 跨租户串数据）。
package sqliteadapter

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/oekit/pkg/errors"

	"github.com/yi-nology/goagent-kernel/eventbus"
	"github.com/yi-nology/goagent-kernel/sideeffect"
	"github.com/yi-nology/goagent-kernel/tenant"
)

// TestTenantEffectStoreIsolatesSameKeyAcrossTenants 跨租户同 Key 互不命中。
//
// 判据：存储键是 (tenant, key) ⇒ A 租户的动作不得让 B 租户拿到同一份结果。
func TestTenantEffectStoreIsolatesSameKeyAcrossTenants(t *testing.T) {
	ctxA := tenCtx(t, "t-a")
	ctxB := tenCtx(t, "t-b")
	s := openStore(t)
	e := s.TenantEffects()

	actA := sideeffect.Action{Key: "k-shared", Resource: "res-a", Payload: []byte("p-a")}
	outcome, lease, _, err := e.Acquire(ctxA, actA, time.Minute)
	if err != nil {
		t.Fatalf("t-a 的 Acquire 失败: %v", err)
	}
	if outcome != sideeffect.OutcomeApplied {
		t.Fatalf("t-a 首次 Acquire 应为 applied，实得 %v", outcome)
	}
	if _, err := e.Complete(ctxA, actA, lease, sideeffect.Result{Payload: []byte("done-a")}); err != nil {
		t.Fatalf("t-a 的 Complete 失败: %v", err)
	}

	// 另一个租户的同 Key：不得重放前者的结果（那会把 A 的答案给 B）。
	actB := sideeffect.Action{Key: "k-shared", Resource: "res-b", Payload: []byte("p-b")}
	outcome, _, got, err := e.Acquire(ctxB, actB, time.Minute)
	if err != nil {
		t.Fatalf("t-b 的 Acquire 失败: %v", err)
	}
	if outcome != sideeffect.OutcomeApplied {
		t.Fatalf("t-b 的同 Key 应独立授予执行权，实得 %v", outcome)
	}
	if len(got.Payload) != 0 {
		t.Fatalf("t-b 不得取回 t-a 的结果，实得 %q", got.Payload)
	}
}

// TestTenantEffectStoreRejectsMissingContext 租户作用域下缺租户 ⇒ 写入被拒。
func TestTenantEffectStoreRejectsMissingContext(t *testing.T) {
	ctx := tenCtx(t, "")
	s := openStore(t)
	e := s.TenantEffects()

	_, _, _, err := e.Acquire(ctx, sideeffect.Action{Key: "k-nocontext", Resource: "res"}, time.Minute)
	if !errors.Is(err, tenant.ErrNoContext) {
		t.Fatalf("缺租户必须被拒（tenant.ErrNoContext），实得 %v", err)
	}
	// 落库不得有任何一行（拒绝发生在写之前）。
	var n int
	if err := s.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM gak_effect_action WHERE key = 'k-nocontext'`).Scan(&n); err != nil {
		t.Fatalf("读动作行数失败: %v", err)
	}
	if n != 0 {
		t.Fatalf("被拒的写入不得留下行，实得 %d 行", n)
	}
}

// TestTenantEffectStoreRejectsDeclaredMismatch 声明的租户与 ctx 不一致 ⇒ 拒绝。
func TestTenantEffectStoreRejectsDeclaredMismatch(t *testing.T) {
	ctx := tenCtx(t, "t-a")
	s := openStore(t)
	e := s.TenantEffects()

	act := sideeffect.Action{Key: "k-mismatch", Resource: "res", Tenant: "t-b"}
	_, _, _, err := e.Acquire(ctx, act, time.Minute)
	if !errors.Is(err, ErrTenantMismatch) {
		t.Fatalf("声明与 ctx 不一致必须被拒（ErrTenantMismatch），实得 %v", err)
	}
}

// TestTenantEventStoreIsolatesStreams 每个租户一条独立的事件流与 ID 空间。
//
// 判据：水位线按租户分行 ⇒ 共享 ID 空间做过滤会产生永久空洞（违反 TailStore
// 的无空洞契约）⇒ 隔离必须落在存储流上。
func TestTenantEventStoreIsolatesStreams(t *testing.T) {
	ctxA := tenCtx(t, "t-a")
	ctxB := tenCtx(t, "t-b")
	s := openStore(t)
	ev := s.TenantEvents()

	idA, err := ev.Insert(ctxA, eventbus.Event{SessionID: "s-a", Type: "t-a"})
	if err != nil {
		t.Fatalf("t-a 的 Insert 失败: %v", err)
	}
	idB, err := ev.Insert(ctxB, eventbus.Event{SessionID: "s-b", Type: "t-b"})
	if err != nil {
		t.Fatalf("t-b 的 Insert 失败: %v", err)
	}
	if idA != 1 || idB != 1 {
		t.Fatalf("每个租户应有独立的 ID 空间（各自从 1 起），实得 a=%d b=%d", idA, idB)
	}

	// 读面按 ctx 读各自的流。
	gotA, err := ev.ListAfter(ctxA, 0, 10)
	if err != nil {
		t.Fatalf("t-a 的 ListAfter 失败: %v", err)
	}
	if len(gotA) != 1 || gotA[0].Tenant != "t-a" || gotA[0].SessionID != "s-a" {
		t.Fatalf("t-a 的流里只应有 t-a 的事件，实得 %+v", gotA)
	}
	gotB, err := ev.ListAfter(ctxB, 0, 10)
	if err != nil {
		t.Fatalf("t-b 的 ListAfter 失败: %v", err)
	}
	if len(gotB) != 1 || gotB[0].Tenant != "t-b" {
		t.Fatalf("t-b 的流里只应有 t-b 的事件，实得 %+v", gotB)
	}

	// 水位线各是各的。
	wmA, err := ev.CommittedWatermark(ctxA)
	if err != nil {
		t.Fatalf("t-a 的水位线读取失败: %v", err)
	}
	wmB, err := ev.CommittedWatermark(ctxB)
	if err != nil {
		t.Fatalf("t-b 的水位线读取失败: %v", err)
	}
	if wmA != 1 || wmB != 1 {
		t.Fatalf("两个租户的水位线应各为 1，实得 a=%d b=%d", wmA, wmB)
	}
}

// TestTenantEventStoreRejectsMissingContext 缺租户 ⇒ 读写都被拒。
func TestTenantEventStoreRejectsMissingContext(t *testing.T) {
	ctx := tenCtx(t, "")
	s := openStore(t)
	ev := s.TenantEvents()

	if _, err := ev.Insert(ctx, eventbus.Event{SessionID: "s", Type: "t"}); !errors.Is(err, tenant.ErrNoContext) {
		t.Fatalf("缺租户的写入必须被拒，实得 %v", err)
	}
	if _, err := ev.MaxID(ctx); !errors.Is(err, tenant.ErrNoContext) {
		t.Fatalf("缺租户的读取必须被拒，实得 %v", err)
	}
}

// TestTenantDownstreamIsolatesFences 同一 resource 在两个租户下是两条独立记录。
//
// 判据：A 租户把栅栏抬到 5，不得让 B 租户的 1 被判陈旧（跨租户抬栅栏 = 静默
// 拒写）。
func TestTenantDownstreamIsolatesFences(t *testing.T) {
	ctxA := tenCtx(t, "t-a")
	ctxB := tenCtx(t, "t-b")
	s := openStore(t)
	d := s.TenantDownstream()

	if err := d.WriteFenced(ctxA, "res-shared", 5, nil); err != nil {
		t.Fatalf("t-a 的 WriteFenced 失败: %v", err)
	}
	// B 租户用更小的令牌：各租户各的栅栏 ⇒ 不得被判陈旧。
	if err := d.WriteFenced(ctxB, "res-shared", 1, nil); err != nil {
		t.Fatalf("t-b 的 WriteFenced 失败（跨租户抬栅栏）: %v", err)
	}

	seenA, err := d.SeenFence(ctxA, "res-shared")
	if err != nil {
		t.Fatalf("t-a 的 SeenFence 失败: %v", err)
	}
	seenB, err := d.SeenFence(ctxB, "res-shared")
	if err != nil {
		t.Fatalf("t-b 的 SeenFence 失败: %v", err)
	}
	if seenA != 5 || seenB != 1 {
		t.Fatalf("两个租户的已见令牌应各自独立，实得 a=%d b=%d", seenA, seenB)
	}
}

// TestTenantDownstreamRejectsMissingContext 缺租户 ⇒ 栅栏读写都被拒。
func TestTenantDownstreamRejectsMissingContext(t *testing.T) {
	ctx := tenCtx(t, "")
	s := openStore(t)
	d := s.TenantDownstream()

	if err := d.WriteFenced(ctx, "res", 1, nil); !errors.Is(err, tenant.ErrNoContext) {
		t.Fatalf("缺租户的栅栏写入必须被拒，实得 %v", err)
	}
	if _, err := d.SeenFence(ctx, "res"); !errors.Is(err, tenant.ErrNoContext) {
		t.Fatalf("缺租户的栅栏读取必须被拒，实得 %v", err)
	}
}

// TestTenantTailCursorStoreIsolatesConsumers 同 consumer 在两个租户下互不干扰。
//
// 判据：游标键由 (tenant, consumer) 派生 ⇒ 跨租户串游标会让后跑的推过对方
// 整段窗口（订阅侧静默漏帧）。
func TestTenantTailCursorStoreIsolatesConsumers(t *testing.T) {
	ctxA := tenCtx(t, "t-a")
	ctxB := tenCtx(t, "t-b")
	s := openStore(t)

	csA := s.TenantTailCursors("replica-a")
	csB := s.TenantTailCursors("replica-a")

	if err := csA.SaveCursor(ctxA, 7); err != nil {
		t.Fatalf("t-a 的 SaveCursor 失败: %v", err)
	}
	// B 租户的同 consumer：独立的一条（未命中）。
	if _, found, err := csB.LoadCursor(ctxB); err != nil || found {
		t.Fatalf("另一租户的游标必须独立（应 found=false）：found=%v err=%v", found, err)
	}
	if err := csB.SaveCursor(ctxB, 3); err != nil {
		t.Fatalf("t-b 的 SaveCursor 失败: %v", err)
	}

	gotA, found, err := csA.LoadCursor(ctxA)
	if err != nil || !found || gotA != 7 {
		t.Fatalf("t-a 的游标应仍为 7：found=%v got=%d err=%v", found, gotA, err)
	}
	gotB, found, err := csB.LoadCursor(ctxB)
	if err != nil || !found || gotB != 3 {
		t.Fatalf("t-b 的游标应仍为 3：found=%v got=%d err=%v", found, gotB, err)
	}
}

// TestTenantTailCursorStoreRejectsMissingContext 缺租户 ⇒ 游标读写都被拒。
func TestTenantTailCursorStoreRejectsMissingContext(t *testing.T) {
	ctx := tenCtx(t, "")
	s := openStore(t)
	cs := s.TenantTailCursors("replica-a")

	if err := cs.SaveCursor(ctx, 1); !errors.Is(err, tenant.ErrNoContext) {
		t.Fatalf("缺租户的游标写入必须被拒，实得 %v", err)
	}
	if _, _, err := cs.LoadCursor(ctx); !errors.Is(err, tenant.ErrNoContext) {
		t.Fatalf("缺租户的游标读取必须被拒，实得 %v", err)
	}
}

// TestStandaloneTenantEffectStore 独立构造器（宿主自带句柄）同样具备租户作用域。
//
// 对位 PG 侧 NewTenantEffectStore：名字与形态对齐。
func TestStandaloneTenantEffectStore(t *testing.T) {
	ctxA := tenCtx(t, "t-a")
	ctxB := tenCtx(t, "t-b")
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "state.db")+dsnParams)
	if err != nil {
		t.Fatalf("开句柄失败: %v", err)
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctxA, schemaDDL); err != nil {
		t.Fatalf("建表失败: %v", err)
	}

	e := NewTenantEffectStore(db)
	if !e.TenantScoped() {
		t.Fatal("独立构造器造出的租户作用域面必须自称租户作用域")
	}
	act := sideeffect.Action{Key: "k-standalone", Resource: "res-standalone"}
	outcome, _, _, err := e.Acquire(ctxA, act, time.Minute)
	if err != nil {
		t.Fatalf("Acquire 失败: %v", err)
	}
	if outcome != sideeffect.OutcomeApplied {
		t.Fatalf("首次 Acquire 应为 applied，实得 %v", outcome)
	}
	// 缺租户 ⇒ 拒绝（fail-closed 在独立构造器上同样成立）。
	if _, _, _, err := e.Acquire(tenCtx(t, ""), act, time.Minute); !errors.Is(err, tenant.ErrNoContext) {
		t.Fatalf("缺租户必须被拒，实得 %v", err)
	}
	// 另一个租户的同 Key：独立。
	act2 := sideeffect.Action{Key: "k-standalone", Resource: "res-standalone-2"}
	outcome2, _, _, err := e.Acquire(ctxB, act2, time.Minute)
	if err != nil {
		t.Fatalf("另一租户的 Acquire 失败: %v", err)
	}
	if outcome2 != sideeffect.OutcomeApplied {
		t.Fatalf("另一租户的同 Key 应独立授予执行权，实得 %v", outcome2)
	}
}
