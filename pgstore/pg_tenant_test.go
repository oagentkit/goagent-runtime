// pg_tenant_test.go —— 租户隔离落库判据（issue #275）。
//
// 判据对应验收标准：
//   - 跨租户同 TaskID + 同动作参数 ⇒ 互不命中（不重放、不冲突、不串资源）；
//   - 同租户不同 Run 同 TaskID ⇒ 互不命中；
//   - 事件面跨租户不串流（每个租户一条独立 ID 空间 + 水位线）；
//   - 缺租户上下文的写入被拒（tenant.ErrNoContext，fail-closed）；
//   - 反向验证：去掉租户维度 ⇒ 跨租户静默重放（用例显式钉住该形态）。
//
// 跳过与硬失败的边界与 pg_test.go 同口径：场景硬失败；唯一的 skip 在环境
// 入口（GAK_PGREF_DSN 未设置，Skipf 写明原因与起法——不会在无库时伪造结果）。
package pgstore

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/oekit/pkg/errors"

	"github.com/yi-nology/goagent-kernel/eventbus"
	"github.com/yi-nology/goagent-kernel/sideeffect"
	"github.com/yi-nology/goagent-kernel/tenant"
)

// tenantCtx 注入租户上下文（写路径的 fail-closed 判据读它）。
func tenantCtx(tenantID string) context.Context {
	return tenant.WithContext(context.Background(), tenant.Context{Tenant: tenantID})
}

// freshTenantStores 一份全新 schema + 三份**租户作用域**存储。
func freshTenantStores(t *testing.T) (*EffectStore, *EventStore, *Downstream, *pgxpool.Pool) {
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
	return NewTenantEffectStore(pool), NewTenantEventStore(pool), NewTenantDownstream(pool), pool
}

// identity 用统一入口（NewActionKeyBuilderFor）拼一组动作身份。
func identity(tenantID, run, task string) sideeffect.Action {
	b := sideeffect.NewActionKeyBuilderFor(tenantID, run)
	return sideeffect.Action{
		Tenant:   tenantID,
		Scope:    []string{run},
		Key:      b.Key("task:" + task),
		Resource: b.Resource(task),
	}
}

// TestEffectStoreCrossTenantSameKeyIsolated 跨租户同 TaskID + 同动作参数
// 互不命中：各自真正执行（Applied），重放只拿回**自己**租户的结果。
func TestEffectStoreCrossTenantSameKeyIsolated(t *testing.T) {
	eff, _, _, _ := freshTenantStores(t)
	ctxA, ctxB := tenantCtx("t-a"), tenantCtx("t-b")

	actA := identity("t-a", "run-1", "deploy-01")
	actB := identity("t-b", "run-1", "deploy-01")
	if actA.Key == actB.Key {
		t.Fatalf("不同租户的默认键不得相同（键构造未纳入租户）：%q", actA.Key)
	}

	outA, leaseA, _, err := eff.Acquire(ctxA, actA, time.Minute)
	if err != nil {
		t.Fatalf("A Acquire: %v", err)
	}
	if outA != sideeffect.OutcomeApplied {
		t.Fatalf("A 首次必须真正执行（Applied），实得 %v——跨租户被重放了？", outA)
	}
	if _, err := eff.Complete(ctxA, actA, leaseA, sideeffect.Result{Payload: []byte("A-result")}); err != nil {
		t.Fatalf("A Complete: %v", err)
	}

	outB, leaseB, _, err := eff.Acquire(ctxB, actB, time.Minute)
	if err != nil {
		t.Fatalf("B Acquire: %v", err)
	}
	if outB != sideeffect.OutcomeApplied {
		t.Fatalf("B 首次必须真正执行（Applied），实得 %v——跨租户静默重放（#275 要封的形态）", outB)
	}
	if leaseB.Resource == leaseA.Resource {
		t.Fatalf("跨租户资源必须隔离：A=%q B=%q", leaseA.Resource, leaseB.Resource)
	}
	if _, err := eff.Complete(ctxB, actB, leaseB, sideeffect.Result{Payload: []byte("B-result")}); err != nil {
		t.Fatalf("B Complete: %v", err)
	}

	// 重放各回各家：A 不得拿到 B 的结果（反之亦然）。
	_, _, resA, err := eff.Acquire(ctxA, actA, time.Minute)
	if err != nil {
		t.Fatalf("A 重放: %v", err)
	}
	if string(resA.Payload) != "A-result" {
		t.Fatalf("A 重放必须拿回 A 的结果，实得 %q（跨租户串结果）", resA.Payload)
	}
	_, _, resB, err := eff.Acquire(ctxB, actB, time.Minute)
	if err != nil {
		t.Fatalf("B 重放: %v", err)
	}
	if string(resB.Payload) != "B-result" {
		t.Fatalf("B 重放必须拿回 B 的结果，实得 %q（跨租户串结果）", resB.Payload)
	}
}

// TestEffectStoreSameTenantDifferentRunIsolated 同租户不同 Run 同 TaskID
// 互不命中（运行身份同样进键与存储）。
func TestEffectStoreSameTenantDifferentRunIsolated(t *testing.T) {
	eff, _, _, _ := freshTenantStores(t)
	ctx := tenantCtx("t-a")

	run1 := identity("t-a", "run-1", "deploy-01")
	run2 := identity("t-a", "run-2", "deploy-01")
	if run1.Key == run2.Key || run1.Resource == run2.Resource {
		t.Fatalf("不同 Run 的身份不得相同：key %q/%q resource %q/%q",
			run1.Key, run2.Key, run1.Resource, run2.Resource)
	}

	out1, lease1, _, err := eff.Acquire(ctx, run1, time.Minute)
	if err != nil || out1 != sideeffect.OutcomeApplied {
		t.Fatalf("run-1 首次应 Applied，got %v err=%v", out1, err)
	}
	if _, err := eff.Complete(ctx, run1, lease1, sideeffect.Result{Payload: []byte("run-1")}); err != nil {
		t.Fatalf("run-1 Complete: %v", err)
	}
	out2, lease2, _, err := eff.Acquire(ctx, run2, time.Minute)
	if err != nil {
		t.Fatalf("run-2 Acquire: %v", err)
	}
	if out2 != sideeffect.OutcomeApplied {
		t.Fatalf("同租户不同 Run 不得互相重放，实得 %v", out2)
	}
	if lease2.Resource == lease1.Resource {
		t.Fatalf("不同 Run 的资源必须隔离：%q", lease1.Resource)
	}
}

// TestEffectStoreTenantScopeFailClosed 缺租户 / 租户不一致的写入被拒。
func TestEffectStoreTenantScopeFailClosed(t *testing.T) {
	eff, _, _, _ := freshTenantStores(t)

	_, _, _, err := eff.Acquire(context.Background(), identity("", "run-1", "t"), time.Minute)
	if !errors.Is(err, tenant.ErrNoContext) {
		t.Fatalf("缺租户上下文的写入必须 tenant.ErrNoContext（fail-closed），实得 %v", err)
	}

	// 同一份写入不能有两个租户结论：ctx=t-a 而动作声明 t-b ⇒ ErrTenantMismatch。
	_, _, _, err = eff.Acquire(tenantCtx("t-a"), identity("t-b", "run-1", "t"), time.Minute)
	if !errors.Is(err, ErrTenantMismatch) {
		t.Fatalf("声明租户与上下文不一致必须 ErrTenantMismatch，实得 %v", err)
	}

	// Complete 同样是写路径：缺租户上下文同样拒绝。
	act := identity("t-a", "run-1", "t")
	_, lease, _, err := eff.Acquire(tenantCtx("t-a"), act, time.Minute)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if _, err := eff.Complete(context.Background(), act, lease, sideeffect.Result{}); !errors.Is(err, tenant.ErrNoContext) {
		t.Fatalf("Complete 缺租户必须 tenant.ErrNoContext，实得 %v", err)
	}
}

// TestEffectStoreLegacyKeyReplaysAcrossTenants 反向验证（issue #275 验收
// 标准）：**去掉租户维度**——不把租户拼进键、存储也不分区——同一 TaskID
// 的第二个租户会拿到 Replayed，业务动作从未执行。这正是本单要封的形态；
// 用例钉住它，证明上面的修复不是「测了个本来就绿的东西」。
func TestEffectStoreLegacyKeyReplaysAcrossTenants(t *testing.T) {
	eff, _, _, _ := freshTenantStores(t)
	// 旧口径：键只用 TaskID，动作不带租户；存储走非租户作用域实例。
	legacy := sideeffect.Action{Key: "task:deploy-01", Resource: "deploy-01"}
	legacyStore := NewEffectStore(eff.pool)
	ctxA, ctxB := tenantCtx("t-a"), tenantCtx("t-b")

	outA, leaseA, _, err := legacyStore.Acquire(ctxA, legacy, time.Minute)
	if err != nil || outA != sideeffect.OutcomeApplied {
		t.Fatalf("旧口径 A 首次应 Applied，got %v err=%v", outA, err)
	}
	if _, err := legacyStore.Complete(ctxA, legacy, leaseA, sideeffect.Result{Payload: []byte("A")}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	outB, _, resB, err := legacyStore.Acquire(ctxB, legacy, time.Minute)
	if err != nil {
		t.Fatalf("旧口径 B Acquire: %v", err)
	}
	if outB != sideeffect.OutcomeReplayed || string(resB.Payload) != "A" {
		t.Fatalf("反向验证失败：去掉租户维度后 B 必须被静默重放（Replayed + A 的结果），实得 %v %q", outB, resB.Payload)
	}
}

// TestEventStoreCrossTenantIsolation 事件面跨租户不串流：每个租户一条独立
// ID 空间与水位线，订阅侧按租户取流（存储层过滤，不靠载荷自筛）。
func TestEventStoreCrossTenantIsolation(t *testing.T) {
	_, evs, _, _ := freshTenantStores(t)
	ctxA, ctxB := tenantCtx("t-a"), tenantCtx("t-b")

	idA1, err := evs.Insert(ctxA, eventbus.Event{Tenant: "t-a", SessionID: "sa", Type: "a1"})
	if err != nil {
		t.Fatalf("A 发布: %v", err)
	}
	idB1, err := evs.Insert(ctxB, eventbus.Event{Tenant: "t-b", SessionID: "sb", Type: "b1"})
	if err != nil {
		t.Fatalf("B 发布: %v", err)
	}
	idA2, err := evs.Insert(ctxA, eventbus.Event{Tenant: "t-a", SessionID: "sa", Type: "a2"})
	if err != nil {
		t.Fatalf("A 第二次发布: %v", err)
	}
	if idA1 != 1 || idA2 != 2 || idB1 != 1 {
		t.Fatalf("每租户应各自从 1 起编号（A: %d,%d B: %d）", idA1, idA2, idB1)
	}

	listA, err := evs.ListAfter(ctxA, 0, 10)
	if err != nil {
		t.Fatalf("ListAfter A: %v", err)
	}
	if len(listA) != 2 || listA[0].Type != "a1" || listA[1].Type != "a2" {
		t.Fatalf("A 流只能看到 A 的事件，实得 %+v", listA)
	}
	for _, ev := range listA {
		if ev.Tenant != "t-a" {
			t.Fatalf("A 流混入他租户事件：%+v", ev)
		}
	}
	listB, err := evs.ListAfter(ctxB, 0, 10)
	if err != nil {
		t.Fatalf("ListAfter B: %v", err)
	}
	if len(listB) != 1 || listB[0].Type != "b1" || listB[0].Tenant != "t-b" {
		t.Fatalf("B 流只能看到 B 的事件，实得 %+v", listB)
	}

	wA, err := evs.CommittedWatermark(ctxA)
	if err != nil || wA != 2 {
		t.Fatalf("A 水位线应为 2，got %d err=%v", wA, err)
	}
	wB, err := evs.CommittedWatermark(ctxB)
	if err != nil || wB != 1 {
		t.Fatalf("B 水位线应为 1（不被 A 的发布推进），got %d err=%v", wB, err)
	}
}

// TestEventStoreTenantScopeFailClosed 事件写入缺租户 / 租户不一致被拒。
func TestEventStoreTenantScopeFailClosed(t *testing.T) {
	_, evs, _, _ := freshTenantStores(t)

	if _, err := evs.Insert(context.Background(), eventbus.Event{Type: "x"}); !errors.Is(err, tenant.ErrNoContext) {
		t.Fatalf("缺租户事件写入必须 tenant.ErrNoContext，实得 %v", err)
	}
	if _, err := evs.Insert(tenantCtx("t-a"), eventbus.Event{Tenant: "t-b", Type: "x"}); !errors.Is(err, ErrTenantMismatch) {
		t.Fatalf("事件声明租户与 ctx 不一致必须 ErrTenantMismatch，实得 %v", err)
	}
	if _, err := evs.ListAfter(context.Background(), 0, 10); !errors.Is(err, tenant.ErrNoContext) {
		t.Fatalf("缺租户读取必须 tenant.ErrNoContext，实得 %v", err)
	}
}

// TestDownstreamCrossTenantIsolation 下游栅栏按 (tenant, resource) 隔离：
// 他租户的更高令牌不得抬升本租户的已见最大。
func TestDownstreamCrossTenantIsolation(t *testing.T) {
	_, _, ds, _ := freshTenantStores(t)
	ctxA, ctxB := tenantCtx("t-a"), tenantCtx("t-b")

	if err := ds.WriteFenced(ctxA, "shared-res", 5, func(ctx context.Context) error {
		return ds.WriteMarker(ctx, "shared-res")
	}); err != nil {
		t.Fatalf("A WriteFenced: %v", err)
	}
	if err := ds.WriteFenced(ctxB, "shared-res", 9, func(ctx context.Context) error {
		return ds.WriteMarker(ctx, "shared-res")
	}); err != nil {
		t.Fatalf("B WriteFenced: %v", err)
	}
	if got, err := ds.SeenFence(ctxA, "shared-res"); err != nil || got != 5 {
		t.Fatalf("A 已见令牌必须是自己的 5（不得被 B 的 9 抬升），got %d err=%v", got, err)
	}
	if got, err := ds.SeenFence(ctxB, "shared-res"); err != nil || got != 9 {
		t.Fatalf("B 已见令牌应为 9，got %d err=%v", got, err)
	}
	// A 的 7 仍是最新代（B 的 9 与 A 无关）。
	if err := ds.WriteFenced(ctxA, "shared-res", 7, nil); err != nil {
		t.Fatalf("A 的 7 应被接受（跨租户不得互相拒绝），实得 %v", err)
	}
	if err := ds.WriteFenced(ctxB, "shared-res", 8, nil); !errors.Is(err, sideeffect.ErrFenceStale) {
		t.Fatalf("B 的 8 应被自己的 9 拒收，实得 %v", err)
	}
	if err := ds.WriteFenced(context.Background(), "shared-res", 1, nil); !errors.Is(err, tenant.ErrNoContext) {
		t.Fatalf("缺租户的栅栏写入必须 tenant.ErrNoContext，实得 %v", err)
	}
}

// TestEnsureSchemaMigratesLegacyTenantSchema vN+1 迁移：旧库（无租户列、
// 单列主键、单行水位）升级后，旧数据归入 tenant ” 且不丢；同 Key 的新
// 租户行可与旧行共存；重复执行 EnsureSchema 幂等。
func TestEnsureSchemaMigratesLegacyTenantSchema(t *testing.T) {
	dsn := requireDSN(t)
	admin := basePool(t, dsn)
	ctx := context.Background()
	schema, err := NewSchema(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = DropSchema(ctx, admin, schema) })

	// 旧形态的 schema（与 vN 的 schema.go 一致的最小集）。
	pool := schemaPool(t, dsn, schema)
	const legacyDDL = `
CREATE TABLE pgref_effect_action (
    key TEXT PRIMARY KEY, done BOOLEAN NOT NULL DEFAULT FALSE, result BYTEA,
    uncertain BOOLEAN NOT NULL DEFAULT FALSE, reason TEXT NOT NULL DEFAULT '',
    intent_resource TEXT, intent_fence BIGINT, intent_holder TEXT, intent_expires TIMESTAMPTZ,
    recon_holder TEXT, recon_expires TIMESTAMPTZ);
CREATE TABLE pgref_effect_resource (
    resource TEXT PRIMARY KEY, fence BIGINT NOT NULL DEFAULT 0, holder TEXT, until TIMESTAMPTZ);
CREATE TABLE pgref_downstream (
    resource TEXT PRIMARY KEY, seen_fence BIGINT NOT NULL, value TEXT NOT NULL DEFAULT '');
CREATE TABLE pgref_event_watermark (
    id SMALLINT PRIMARY KEY DEFAULT 1 CHECK (id = 1), committed BIGINT NOT NULL DEFAULT 0);
INSERT INTO pgref_event_watermark (id, committed) VALUES (1, 1);
CREATE TABLE pgref_event (
    id BIGINT PRIMARY KEY, session_id TEXT NOT NULL DEFAULT '', case_id TEXT NOT NULL DEFAULT '',
    step_id BIGINT, type TEXT NOT NULL DEFAULT '', level TEXT NOT NULL DEFAULT 'info',
    payload JSONB NOT NULL DEFAULT '{}'::jsonb, occurred_at TIMESTAMPTZ NOT NULL DEFAULT now());
INSERT INTO pgref_effect_action (key) VALUES ('legacy-key');
INSERT INTO pgref_effect_resource (resource) VALUES ('legacy-res');
INSERT INTO pgref_downstream (resource, seen_fence) VALUES ('legacy-ds', 3);
INSERT INTO pgref_event (id, session_id, type) VALUES (1, 's', 'legacy');`
	if _, err := pool.Exec(ctx, legacyDDL); err != nil {
		t.Fatalf("建旧 schema 失败: %v", err)
	}

	for i := 0; i < 2; i++ { // 第二次 = 幂等性判据
		if err := EnsureSchema(ctx, admin, schema); err != nil {
			t.Fatalf("第 %d 次 EnsureSchema 迁移失败（必须幂等）: %v", i+1, err)
		}
	}

	// 旧行保留并归入 '' 租户；同 Key 的新租户行可共存（复合主键生效）。
	var tenantID string
	if err := pool.QueryRow(ctx,
		`SELECT tenant FROM pgref_effect_action WHERE key = 'legacy-key'`).Scan(&tenantID); err != nil {
		t.Fatalf("旧动作行丢失: %v", err)
	}
	if tenantID != "" {
		t.Fatalf("旧动作行必须归入 tenant ''，实得 %q", tenantID)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO pgref_effect_action (tenant, key) VALUES ('t-a', 'legacy-key')`); err != nil {
		t.Fatalf("同 Key 的新租户行必须可与旧行共存（复合主键未生效）: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO pgref_effect_resource (tenant, resource) VALUES ('t-a', 'legacy-res')`); err != nil {
		t.Fatalf("资源行复合主键未生效: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO pgref_downstream (tenant, resource, seen_fence) VALUES ('t-a', 'legacy-ds', 1)`); err != nil {
		t.Fatalf("下游行复合主键未生效: %v", err)
	}

	// 旧全局事件与旧水位原样保留：默认（'' 流）仍能读到。
	evs := NewEventStore(pool)
	if w, err := evs.CommittedWatermark(ctx); err != nil || w != 1 {
		t.Fatalf("旧全局水位必须保留为 1，got %d err=%v", w, err)
	}
	evsList, err := evs.ListAfter(ctx, 0, 10)
	if err != nil {
		t.Fatalf("旧事件读取失败: %v", err)
	}
	if len(evsList) != 1 || evsList[0].Type != "legacy" {
		t.Fatalf("旧事件必须仍在 '' 流中，实得 %+v", evsList)
	}

	// 新租户事件可写，且默认流不会被它串入（各自一条 ID 空间）。
	evTenant := NewTenantEventStore(pool)
	if id, err := evTenant.Insert(tenantCtx("t-a"), eventbus.Event{Tenant: "t-a", Type: "new"}); err != nil || id != 1 {
		t.Fatalf("新租户首条事件应为 ID 1（独立 ID 空间），got %d err=%v", id, err)
	}
	if got, err := evs.ListAfter(ctx, 0, 10); err != nil || len(got) != 1 {
		t.Fatalf("默认流不得串入新租户事件，got %+v err=%v", got, err)
	}
}
