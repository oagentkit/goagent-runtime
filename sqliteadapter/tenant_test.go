// tenant_test.go —— 租户作用域的**判据入口**（resolveTenant 三段 + 自检口 + 派生键）。
//
// # 对位关系
//
// 逐条对位 pgstore（v0.2.0 自 goagent-kernel 迁入）/tenant.go 与 pgstore 的 effect/event/sink/cursor：
// 形态对位（同名构造器、同名自检口、同款三段判定），**判据逻辑各写一份**
// （共用会把两个引擎耦在一起）。
//
// # 三段判定的三条
//
//  1. scoped=false ⇒ 返回「声明的租户」（空 = 旧布局 / 单租户命名空间）；
//  2. scoped=true ⇒ ctx 必须带租户（缺 ⇒ tenant.ErrNoContext，fail-closed）；
//  3. scoped=true 且「声明的租户」非空且与 ctx 不一致 ⇒ ErrTenantMismatch
//     （拒绝猜测哪一份是对的——猜错的方向是跨租户串数据）。
//
// # 自检口
//
// TenantScoped() 的**两种取值**都要有用例：恒 false 的自检口等于零判据
// （Task 5 评审遗留的交接，本任务必须补上）。
//
// 各面的**租户隔离**判据在 tenant_faces_test.go（一个面一个面地核）。
package sqliteadapter

import (
	"context"
	"testing"
	"time"

	"github.com/oekit/pkg/errors"

	"github.com/yi-nology/goagent-kernel/tenant"
)

// tenCtx 注入租户上下文的 ctx（带超时：回归要**快速红**，而不是挂在 60s 超时上）。
//
// tenantID 为空 ⇒ 不注入租户（模拟「忘了注入上下文」的 fail-closed 场景）。
func tenCtx(t *testing.T, tenantID string) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	if tenantID == "" {
		return ctx
	}
	return tenant.WithContext(ctx, tenant.Context{Tenant: tenantID})
}

// TestResolveTenantUnscopedReturnsDeclared 未声明作用域 ⇒ 原样返回声明的租户。
//
// 旧布局逐字不变：动作/事件自带的 Tenant 就是分区键，不强制上下文。
func TestResolveTenantUnscopedReturnsDeclared(t *testing.T) {
	got, err := resolveTenant(context.Background(), false, "t-declared")
	if err != nil {
		t.Fatalf("未声明作用域时不得报错: %v", err)
	}
	if got != "t-declared" {
		t.Fatalf("未声明作用域时应原样返回声明的租户，实得 %q", got)
	}
	// 空声明 = 单租户命名空间（不是缺租户）。
	got, err = resolveTenant(context.Background(), false, "")
	if err != nil || got != "" {
		t.Fatalf("空声明应得空租户且不报错：got=%q err=%v", got, err)
	}
}

// TestResolveTenantScopedMissingContextFailsClosed 声明作用域但 ctx 没租户 ⇒ 拒绝。
//
// fail-closed：缺租户是**拒绝**，不是回落到默认命名空间（回落 = 跨租户串数据）。
func TestResolveTenantScopedMissingContextFailsClosed(t *testing.T) {
	_, err := resolveTenant(context.Background(), true, "")
	if !errors.Is(err, tenant.ErrNoContext) {
		t.Fatalf("缺租户必须被拒（tenant.ErrNoContext），实得 %v", err)
	}
}

// TestResolveTenantScopedUsesContextTenant 声明作用域 ⇒ 取 ctx 的租户。
func TestResolveTenantScopedUsesContextTenant(t *testing.T) {
	ctx := tenCtx(t, "t-a")
	got, err := resolveTenant(ctx, true, "")
	if err != nil {
		t.Fatalf("解析租户失败: %v", err)
	}
	if got != "t-a" {
		t.Fatalf("应取 ctx 的租户，实得 %q", got)
	}
}

// TestResolveTenantScopedRejectsDeclaredMismatch 声明的租户与 ctx 不一致 ⇒ 拒绝。
//
// 第三段：同一份写入不能有两个租户结论。
func TestResolveTenantScopedRejectsDeclaredMismatch(t *testing.T) {
	ctx := tenCtx(t, "t-a")
	_, err := resolveTenant(ctx, true, "t-b")
	if !errors.Is(err, ErrTenantMismatch) {
		t.Fatalf("声明与 ctx 不一致必须被拒（ErrTenantMismatch），实得 %v", err)
	}
}

// TestResolveTenantScopedAcceptsMatchingDeclared 声明与 ctx 一致 ⇒ 放行。
func TestResolveTenantScopedAcceptsMatchingDeclared(t *testing.T) {
	ctx := tenCtx(t, "t-a")
	got, err := resolveTenant(ctx, true, "t-a")
	if err != nil {
		t.Fatalf("一致时不得报错: %v", err)
	}
	if got != "t-a" {
		t.Fatalf("一致时应取 ctx 的租户，实得 %q", got)
	}
}

// TestTenantScopedSelfCheckPorts 自检口的两种取值都要有判据。
//
// 恒 false 的开关等于零判据（Task 5 评审遗留）：四种面各判一次「默认形态
// 报 false」与「租户形态报 true」。
func TestTenantScopedSelfCheckPorts(t *testing.T) {
	s := openStore(t)

	if s.Effects().TenantScoped() {
		t.Fatal("默认形态的副作用闸不得自称租户作用域")
	}
	if s.Events().TenantScoped() {
		t.Fatal("默认形态的事件面不得自称租户作用域")
	}
	if s.Downstream().TenantScoped() {
		t.Fatal("默认形态的下游栅栏不得自称租户作用域")
	}
	if s.TenantTailCursors("replica-a").TenantScoped() != true {
		t.Fatal("租户作用域的游标面必须自称租户作用域")
	}

	if !s.TenantEffects().TenantScoped() {
		t.Fatal("租户作用域的副作用闸必须自称租户作用域")
	}
	if !s.TenantEvents().TenantScoped() {
		t.Fatal("租户作用域的事件面必须自称租户作用域")
	}
	if !s.TenantDownstream().TenantScoped() {
		t.Fatal("租户作用域的下游栅栏必须自称租户作用域")
	}
}

// TestTenantTailCursorStoreRejectsEmptyConsumer 空 consumer key 是装配期编程错误。
func TestTenantTailCursorStoreRejectsEmptyConsumer(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("空 consumer key 必须 panic（装配期编程错误不容静默通过）")
		}
	}()
	openStore(t).TenantTailCursors("")
}

// TestTenantCursorKeyIsInjective 派生键必须单射。
//
// 长度前缀是必要的：只加分隔符时，tenant 或 consumer 内含该分隔符就能拼出
// 同一个键 ⇒ 跨租户串游标。
func TestTenantCursorKeyIsInjective(t *testing.T) {
	// ⚠ 用**切片**装组合，不用 map 字面量装「键 ⇒ 标签」：map 字面量的键若是
	// 非常量表达式（这里是函数调用），重复键**不报编译错**，运行期被后者覆盖
	// ⇒ 撞键会被这条用例**静默吞掉**（变异验证实测：去掉长度前缀后本用例仍绿）。
	// ⇒ 组合装切片，撞键由下面的循环当场判出来。
	pairs := [][2]string{
		{"t-a", "c"},
		{"t-a", "bc"},
		{"t-a:b", "c"},
		{"t-a", "b:c"},
		{"", "c"},
		{"t-a", ""},
	}
	seen := map[string][2]string{}
	for _, p := range pairs {
		key := tenantCursorKey(p[0], p[1])
		if prev, dup := seen[key]; dup {
			t.Fatalf("派生键不单射：(tenant=%q,consumer=%q) 与 (tenant=%q,consumer=%q) 撞成 %q",
				prev[0], prev[1], p[0], p[1], key)
		}
		seen[key] = p
	}
	// 长度前缀必须**真的**在键里（不是靠运气不撞）：同长度不同租户也得各自成键。
	if tenantCursorKey("ab", "c") == tenantCursorKey("ac", "b") {
		t.Fatal("同长度的不同 (tenant, consumer) 不得撞成同一个键")
	}
}
