// pg_tenant_cursor_test.go —— 租户作用域尾随游标的隔离判据（issue #275）。
//
// 从 pg_tenant_test.go 拆出，只为守住「单文件 400 行」的结构阈值。
package pgstore

import (
	"context"
	"testing"

	"github.com/oekit/pkg/errors"

	"github.com/yi-nology/goagent-kernel/tenant"
)

// TestTenantTailCursorStoreIsolation 尾随游标按租户隔离：同一 consumer 名
// 在两个租户下各有一条游标，互不推进；缺租户上下文 fail-closed。
func TestTenantTailCursorStoreIsolation(t *testing.T) {
	_, _, _, pool := freshTenantStores(t)
	ctxA, ctxB := tenantCtx("t-a"), tenantCtx("t-b")
	cur := NewTenantTailCursorStore(pool, "replica-1")

	if _, found, err := cur.LoadCursor(ctxA); err != nil || found {
		t.Fatalf("A 首次读取应未命中（0,false,nil），got found=%v err=%v", found, err)
	}
	if err := cur.SaveCursor(ctxA, 5); err != nil {
		t.Fatalf("A SaveCursor: %v", err)
	}
	if got, found, err := cur.LoadCursor(ctxA); err != nil || !found || got != 5 {
		t.Fatalf("A 游标应为 5，got %d found=%v err=%v", got, found, err)
	}
	if _, found, err := cur.LoadCursor(ctxB); err != nil || found {
		t.Fatalf("B 不得看到 A 的游标（跨租户串游标），got found=%v err=%v", found, err)
	}
	if err := cur.SaveCursor(ctxB, 9); err != nil {
		t.Fatalf("B SaveCursor: %v", err)
	}
	if got, _, _ := cur.LoadCursor(ctxA); got != 5 {
		t.Fatalf("B 的保存不得推进 A 的游标，got %d", got)
	}
	if _, _, err := cur.LoadCursor(context.Background()); !errors.Is(err, tenant.ErrNoContext) {
		t.Fatalf("缺租户上下文的游标读取必须 tenant.ErrNoContext，实得 %v", err)
	}
	if err := cur.SaveCursor(context.Background(), 1); !errors.Is(err, tenant.ErrNoContext) {
		t.Fatalf("缺租户上下文的游标保存必须 tenant.ErrNoContext，实得 %v", err)
	}
}
