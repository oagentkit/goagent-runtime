package adjudicate_test

// adjudicate_test.go —— 未知态裁决框架行为面（yi-nology/goagent-runtime#1）。
// 存储走 runtime/storage.NewMem()（kernel sidetest 同款契约实现），
// Source/Resolver 为合成假件。

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/yi-nology/goagent-kernel/sideeffect"

	"github.com/yi-nology/goagent-runtime/adjudicate"
	"github.com/yi-nology/goagent-runtime/storage"
)

type fakeSource struct {
	items      []adjudicate.Item
	cutoffSeen time.Time
	limitSeen  int
}

func (f *fakeSource) ListUncertain(_ context.Context, olderThan time.Time, limit int) ([]adjudicate.Item, error) {
	f.cutoffSeen = olderThan
	f.limitSeen = limit
	return f.items, nil
}

// fakeResolver 脚本化裁决器：按 Key 前缀匹配，行为三态（ok/err/不认识）。
type fakeResolver struct {
	fn func(item adjudicate.Item) (sideeffect.Resolution, bool, error)
}

func (f fakeResolver) Resolve(ctx context.Context, item adjudicate.Item) (sideeffect.Resolution, bool, error) {
	return f.fn(item)
}

func newRunner(t *testing.T) (*sideeffect.Runner, sideeffect.Store) {
	t.Helper()
	store := storage.NewMem().Effects()
	return sideeffect.NewRunner(store, time.Minute), store
}

func act(key string, safe bool) sideeffect.Action {
	rs := sideeffect.RetrySafetyUnsafe
	if safe {
		rs = sideeffect.RetrySafetySafe
	}
	return sideeffect.Action{Key: sideeffect.Key(key), Resource: "res:" + key, RetrySafety: rs}
}

// TestSettledApplied 裁决已生效 → 落库记完成；后续 Acquire 重放不再重做。
func TestSettledApplied(t *testing.T) {
	ctx := context.Background()
	runner, store := newRunner(t)
	a := act("k-applied", false)
	// 制造未知态：Acquire 获得执行权后显式 MarkUncertain（镜像生产「施加失败
	// 结果未知」形态——kernel 的未知态判据本体不走本测试覆盖，见 kernel sidetest）。
	outcome, lease, _, err := store.Acquire(ctx, a, time.Minute)
	if err != nil || outcome != sideeffect.OutcomeApplied {
		t.Fatalf("Acquire 应授予, outcome=%v err=%v", outcome, err)
	}
	if err := store.MarkUncertain(ctx, a, lease, "test"); err != nil {
		t.Fatal(err)
	}

	src := &fakeSource{items: []adjudicate.Item{{Action: a, Reason: "test", MarkedAt: time.Now().Add(-time.Hour)}}}
	res := sideeffect.Resolution{Applied: true, Result: sideeffect.Result{Payload: []byte(`{"id":42}`)}}
	loop, err := adjudicate.NewLoop(src, fakeResolver{fn: func(item adjudicate.Item) (sideeffect.Resolution, bool, error) {
		return res, true, nil
	}}, runner)
	if err != nil {
		t.Fatal(err)
	}
	st, err := loop.ReconcileOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Settled != 1 || st.Scanned != 1 {
		t.Fatalf("stats=%+v", st)
	}
	// 重放而非重做：Acquire 直接给既有结果。
	outcome, _, got, err := store.Acquire(ctx, a, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if outcome != sideeffect.OutcomeReplayed || string(got.Payload) != `{"id":42}` {
		t.Fatalf("应重放既有结果, outcome=%v payload=%s", outcome, got.Payload)
	}
}

// TestNotAppliedSafeReleased RetrySafetySafe 未生效 → 释放可重做。
func TestNotAppliedSafeReleased(t *testing.T) {
	ctx := context.Background()
	runner, store := newRunner(t)
	a := act("k-safe", true)
	// 制造未知态：Acquire 获得执行权后显式 MarkUncertain（镜像生产「施加失败
	// 结果未知」形态——kernel 的未知态判据本体不走本测试覆盖，见 kernel sidetest）。
	// 即时过期租约：释放后资源锁不挡重授予（kernel 栅栏语义——资源租约
	// 到 TTL 才放，与 Key 释放正交）。
	outcome, lease, _, err := store.Acquire(ctx, a, time.Nanosecond)
	if err != nil || outcome != sideeffect.OutcomeApplied {
		t.Fatalf("Acquire 应授予, outcome=%v err=%v", outcome, err)
	}
	if err := store.MarkUncertain(ctx, a, lease, "test"); err != nil {
		t.Fatal(err)
	}

	src := &fakeSource{items: []adjudicate.Item{{Action: a, MarkedAt: time.Now().Add(-time.Hour)}}}
	loop, err := adjudicate.NewLoop(src, fakeResolver{fn: func(item adjudicate.Item) (sideeffect.Resolution, bool, error) {
		return sideeffect.Resolution{Applied: false}, true, nil
	}}, runner)
	if err != nil {
		t.Fatal(err)
	}
	st, err := loop.ReconcileOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Settled != 1 {
		t.Fatalf("stats=%+v", st)
	}
	outcome2, _, _, err2 := store.Acquire(ctx, a, time.Minute)
	if err2 != nil {
		t.Fatal(err2)
	}
	if outcome2 != sideeffect.OutcomeApplied {
		t.Fatalf("safe 未生效应重新授予, outcome=%v", outcome2)
	}
}

// TestSkippedUnsupported 无人认识 → 跳过不改状态（绝不猜）。
func TestSkippedUnsupported(t *testing.T) {
	ctx := context.Background()
	runner, _ := newRunner(t)
	a := act("k-mystery", false)
	src := &fakeSource{items: []adjudicate.Item{{Action: a, MarkedAt: time.Now().Add(-time.Hour)}}}
	loop, err := adjudicate.NewLoop(src, adjudicate.NewRouter(), runner)
	if err != nil {
		t.Fatal(err)
	}
	st, err := loop.ReconcileOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Skipped != 1 || st.Settled != 0 {
		t.Fatalf("stats=%+v", st)
	}
}

// TestDeferredEvidenceError 判据不足（resolver 报错）→ 本轮不裁决，Key 原样保留。
func TestDeferredEvidenceError(t *testing.T) {
	ctx := context.Background()
	runner, _ := newRunner(t)
	a := act("k-flaky", false)
	src := &fakeSource{items: []adjudicate.Item{{Action: a, MarkedAt: time.Now().Add(-time.Hour)}}}
	loop, err := adjudicate.NewLoop(src, fakeResolver{fn: func(item adjudicate.Item) (sideeffect.Resolution, bool, error) {
		return sideeffect.Resolution{}, false, errors.New("下游查询超时")
	}}, runner)
	if err != nil {
		t.Fatal(err)
	}
	st, rerr := loop.ReconcileOnce(ctx)
	if rerr == nil {
		t.Fatal("判据不足应汇总返回 error")
	}
	if st.Deferred != 1 {
		t.Fatalf("stats=%+v", st)
	}
}

// TestGraceWindowAndBatch 静置窗与批量参数如实传给 Source。
func TestGraceWindowAndBatch(t *testing.T) {
	ctx := context.Background()
	runner, _ := newRunner(t)
	src := &fakeSource{}
	loop, err := adjudicate.NewLoop(src, adjudicate.NewRouter(), runner,
		adjudicate.WithGrace(2*time.Minute), adjudicate.WithBatch(7))
	if err != nil {
		t.Fatal(err)
	}
	before := time.Now()
	if _, err := loop.ReconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	wantCutoff := before.Add(-2 * time.Minute)
	if src.cutoffSeen.Before(wantCutoff.Add(-time.Second)) || src.cutoffSeen.After(wantCutoff.Add(time.Second)) {
		t.Fatalf("cutoff=%v, want≈%v", src.cutoffSeen, wantCutoff)
	}
	if src.limitSeen != 7 {
		t.Fatalf("limit=%d, want 7", src.limitSeen)
	}
}

// TestNewLoopFailFast 装配期缺件 fail-fast。
func TestNewLoopFailFast(t *testing.T) {
	runner, _ := newRunner(t)
	if _, err := adjudicate.NewLoop(nil, adjudicate.NewRouter(), runner); err == nil {
		t.Fatal("nil Source 应报错")
	}
	if _, err := adjudicate.NewLoop(&fakeSource{}, nil, runner); err == nil {
		t.Fatal("nil Resolver 应报错")
	}
	if _, err := adjudicate.NewLoop(&fakeSource{}, adjudicate.NewRouter(), nil); err == nil {
		t.Fatal("nil Runner 应报错")
	}
}

// TestRouterOrderAndErrorPassThrough 路由语义：先认识者生效；error 立即透传。
func TestRouterOrderAndErrorPassThrough(t *testing.T) {
	ctx := context.Background()
	item := adjudicate.Item{Action: act("k-r", false)}
	hit := sideeffect.Resolution{Applied: true}
	first := fakeResolver{fn: func(adjudicate.Item) (sideeffect.Resolution, bool, error) {
		return sideeffect.Resolution{}, false, errors.New("判据不足")
	}}
	second := fakeResolver{fn: func(adjudicate.Item) (sideeffect.Resolution, bool, error) {
		return hit, true, nil
	}}
	// error 透传：第一个认识但报错的裁决生效（短路，第二个不执行）。
	r := adjudicate.NewRouter(first, second)
	_, ok, err := r.Resolve(ctx, item)
	if err == nil || ok {
		t.Fatalf("error 应透传且 ok=false, got ok=%v err=%v", ok, err)
	}
	// ok=true 生效：第一个不认识，第二个裁决。
	r2 := adjudicate.NewRouter(fakeResolver{fn: func(adjudicate.Item) (sideeffect.Resolution, bool, error) {
		return sideeffect.Resolution{}, false, nil
	}}, second)
	got, ok, err := r2.Resolve(ctx, item)
	if err != nil || !ok || !got.Applied {
		t.Fatalf("第二个裁决应生效, got=%+v ok=%v err=%v", got, ok, err)
	}
}
