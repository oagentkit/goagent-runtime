// effect_test.go —— 副作用闸与下游栅栏的**契约接线** + 本包落库形态判据。
//
// # 为什么主判据是共享套件
//
// sideeffect.Store 的正确性无法靠「看起来对」保证：它要跨进程、靠一次原子
// 操作维持三个判据的联合。本包与 pgstore 跑的是**同一段断言**
// （sidetest 全套 33 条 + RunSinkContract 6 条）⇒ 两侧语义漂移当场可见。
//
// # 本文件自己的用例只判「落库形态」
//
// 套件判的是**契约语义**（跨实现必须一致的部分）；本文件的用例判的是
// **SQLite 侧的落库形态**（两级状态落在哪张表、哪几列）——那部分不进共享
// 套件（PG 与 SQLite 的列名不同），但它同样是判据：判据缺失 ⇒ 坏实现也能过。
package sqliteadapter

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/yi-nology/goagent-kernel/sideeffect"
	"github.com/yi-nology/goagent-kernel/sidetest"
)

// openEffectStore 开一份全新的后端并建好表（契约套件的工厂约定：每个子测试一份）。
func openEffectStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("打开失败: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	// ctx 带超时：回归要**快速红**，而不是挂在 60s 超时上。
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.EnsureSchema(ctx); err != nil {
		t.Fatalf("建表失败: %v", err)
	}
	return s
}

// effectFactory 契约套件的工厂（每个子测试调一次，返回全新实现）。
func effectFactory(t *testing.T, _ *sidetest.FakeClock) sidetest.ClockedStore {
	t.Helper()
	return openEffectStore(t).Effects()
}

// TestEffectContract 跑副作用闸的共享契约套件（sidetest 全套 33 条）。
func TestEffectContract(t *testing.T) {
	sidetest.Run(t, effectFactory)
}

// TestSinkContract 跑下游栅栏的共享契约套件（RunSinkContract 6 条）。
func TestSinkContract(t *testing.T) {
	sidetest.RunSinkContract(t, func(t *testing.T) sidetest.SinkProbe {
		t.Helper()
		return openEffectStore(t).Downstream()
	})
}

// TestEffectTwoLevelRows 钉住**两级状态的落库形态**。
//
// 契约套件判的是语义（「意图挡住重试」「闸被放掉」），判不出**落在哪张表**：
// 一个把两级状态合成一张表的实现，套件照样全绿。本用例把落库形态钉住：
//
//   - Acquire 后：Key 级四列意图齐全 + 资源级 holder/fence/until 三列齐全；
//   - Complete 后：意图四列清空、资源级 holder 清空、结果留存、done 置位。
//
// 失效形态：意图只写三列（半写行）⇒ 存储侧数据损坏，且「半写行」这条
// fail-closed 判据（ErrIntentIncomplete）永远走不到。
func TestEffectTwoLevelRows(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s := openEffectStore(t)
	e := s.Effects()

	act := sideeffect.Action{Key: "k-rows", Resource: "res-rows", Payload: []byte("p")}
	outcome, lease, _, err := e.Acquire(ctx, act, time.Minute)
	if err != nil {
		t.Fatalf("Acquire 失败: %v", err)
	}
	if outcome != sideeffect.OutcomeApplied {
		t.Fatalf("首次 Acquire 应为 applied，实得 %v", outcome)
	}

	// Key 级：意图四列齐全（缺一列即半写行）。
	var (
		intentResource sql.NullString
		intentFence    sql.NullInt64
		intentHolder   sql.NullString
		intentExpires  sql.NullInt64
		done           int64
	)
	if err := s.DB().QueryRowContext(ctx,
		`SELECT done, intent_resource, intent_fence, intent_holder, intent_expires
		   FROM gak_effect_action WHERE tenant = '' AND key = ?`, string(act.Key)).
		Scan(&done, &intentResource, &intentFence, &intentHolder, &intentExpires); err != nil {
		t.Fatalf("读动作行失败: %v", err)
	}
	if done != 0 {
		t.Fatalf("刚授予的动作不该是已完成，实得 done=%d", done)
	}
	if !intentResource.Valid || !intentFence.Valid || !intentHolder.Valid || !intentExpires.Valid {
		t.Fatalf("执行意图四列应同存（半写行是存储侧损坏）：resource=%v fence=%v holder=%v expires=%v",
			intentResource.Valid, intentFence.Valid, intentHolder.Valid, intentExpires.Valid)
	}
	if intentHolder.String != lease.Holder {
		t.Fatalf("意图里的持有者身份应与授予的那份一致，实得 %q want %q", intentHolder.String, lease.Holder)
	}
	if intentFence.Int64 != int64(lease.Fence) {
		t.Fatalf("意图里的令牌应与授予的那枚一致，实得 %d want %d", intentFence.Int64, lease.Fence)
	}

	// 资源级：holder/fence/until 三列齐全。
	var (
		fence  int64
		holder sql.NullString
		until  sql.NullInt64
	)
	if err := s.DB().QueryRowContext(ctx,
		`SELECT fence, holder, until FROM gak_effect_resource WHERE tenant = '' AND resource = ?`, act.Resource).
		Scan(&fence, &holder, &until); err != nil {
		t.Fatalf("读资源行失败: %v", err)
	}
	if !holder.Valid || !until.Valid {
		t.Fatalf("资源行应记着持有者与到期时刻：holder=%v until=%v", holder.Valid, until.Valid)
	}
	if fence != int64(lease.Fence) {
		t.Fatalf("资源行的令牌应与授予的那枚一致，实得 %d want %d", fence, lease.Fence)
	}

	// 收场：意图清空、闸放掉、结果留存。
	if _, err := e.Complete(ctx, act, lease, sideeffect.Result{Payload: []byte("v-rows")}); err != nil {
		t.Fatalf("Complete 失败: %v", err)
	}
	var (
		gotDone    int64
		gotResult  []byte
		gotIntentR sql.NullString
		gotIntentF sql.NullInt64
		gotIntentH sql.NullString
		gotIntentE sql.NullInt64
	)
	if err := s.DB().QueryRowContext(ctx,
		`SELECT done, result, intent_resource, intent_fence, intent_holder, intent_expires
		   FROM gak_effect_action WHERE tenant = '' AND key = ?`, string(act.Key)).
		Scan(&gotDone, &gotResult, &gotIntentR, &gotIntentF, &gotIntentH, &gotIntentE); err != nil {
		t.Fatalf("读动作行失败: %v", err)
	}
	if gotDone != 1 {
		t.Fatalf("完成后 done 应为 1，实得 %d", gotDone)
	}
	if string(gotResult) != "v-rows" {
		t.Fatalf("结果应留存，实得 %q", gotResult)
	}
	if gotIntentR.Valid || gotIntentF.Valid || gotIntentH.Valid || gotIntentE.Valid {
		t.Fatalf("完成后执行意图四列应同空，实得 resource=%v fence=%v holder=%v expires=%v",
			gotIntentR.Valid, gotIntentF.Valid, gotIntentH.Valid, gotIntentE.Valid)
	}
	var gotHolder sql.NullString
	if err := s.DB().QueryRowContext(ctx,
		`SELECT holder FROM gak_effect_resource WHERE tenant = '' AND resource = ?`, act.Resource).
		Scan(&gotHolder); err != nil {
		t.Fatalf("读资源行失败: %v", err)
	}
	if gotHolder.Valid {
		t.Fatalf("完成后资源闸应放掉（holder 清空），实得 %q", gotHolder.String)
	}
}

// TestEffectActionTenantCarriesColumn 钉住「租户列取动作自带值」（旧布局）。
//
// 判据：同一 Key 在两个租户下互不命中 ⇒ 租户列真的进了联合键，而不是
// 一个摆设。失效形态：租户列被写死成空串 ⇒ 跨租户的动作互相覆盖
// （一个租户的完成结果被另一个租户取走）。
func TestEffectActionTenantCarriesColumn(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s := openEffectStore(t)
	e := s.Effects()

	a1 := sideeffect.Action{Key: "k-shared", Resource: "res-a", Tenant: "t-1", Payload: []byte("p1")}
	a2 := sideeffect.Action{Key: "k-shared", Resource: "res-b", Tenant: "t-2", Payload: []byte("p2")}

	_, lease1, _, err := e.Acquire(ctx, a1, time.Minute)
	if err != nil {
		t.Fatalf("租户 t-1 的 Acquire 失败: %v", err)
	}
	if _, err := e.Complete(ctx, a1, lease1, sideeffect.Result{Payload: []byte("first")}); err != nil {
		t.Fatalf("Complete 失败: %v", err)
	}

	// 另一个租户的同 Key：不得命中前者的完成结果（那会把 A 的答案给 B）。
	outcome, _, got, err := e.Acquire(ctx, a2, time.Minute)
	if err != nil {
		t.Fatalf("租户 t-2 的 Acquire 失败: %v", err)
	}
	if outcome != sideeffect.OutcomeApplied {
		t.Fatalf("另一租户的同 Key 应独立授予执行权，实得 %v", outcome)
	}
	if len(got.Payload) != 0 {
		t.Fatalf("另一租户不得取回前者的结果，实得 %q", got.Payload)
	}
}

// TestStandaloneEffectStore 钉住独立构造器（宿主自带句柄的形态）。
//
// 判据：独立构造器造出的面同样满足契约（不是「只在门面上能用」）——
// 这里跑一条最简的幂等判据（同一 Key 第二次 Acquire → replayed + 同一结果）。
func TestStandaloneEffectStore(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "state.db")+dsnParams)
	if err != nil {
		t.Fatalf("开句柄失败: %v", err)
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, schemaDDL); err != nil {
		t.Fatalf("建表失败: %v", err)
	}

	e := NewEffectStore(db)
	act := sideeffect.Action{Key: "k-standalone", Resource: "res-standalone", Payload: []byte("p")}
	outcome, lease, _, err := e.Acquire(ctx, act, time.Minute)
	if err != nil {
		t.Fatalf("Acquire 失败: %v", err)
	}
	if outcome != sideeffect.OutcomeApplied {
		t.Fatalf("首次 Acquire 应为 applied，实得 %v", outcome)
	}
	if _, err := e.Complete(ctx, act, lease, sideeffect.Result{Payload: []byte("v")}); err != nil {
		t.Fatalf("Complete 失败: %v", err)
	}
	outcome2, _, got, err := e.Acquire(ctx, act, time.Minute)
	if err != nil {
		t.Fatalf("二次 Acquire 失败: %v", err)
	}
	if outcome2 != sideeffect.OutcomeReplayed || string(got.Payload) != "v" {
		t.Fatalf("同一 Key 二次 Acquire 应重放同一结果，实得 outcome=%v payload=%q", outcome2, got.Payload)
	}
}
