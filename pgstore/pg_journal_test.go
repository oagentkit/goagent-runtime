// pg_journal_test.go —— 账本面（invocation.Store）在真实 PostgreSQL 上的
// 共享契约套件。
//
// 形态与其它面同款：环境入口 skip（无 DSN 时不伪造结果），套件场景**零 skip**
// ——显式接入套件 = 声明要满足契约，跳过等于把「没测过」伪装成「通过了」。
package pgstore

import (
	"context"
	"testing"

	"github.com/oagentkit/goagent-kernel/invocation"
	"github.com/oagentkit/goagent-kernel/invocation/invocationtest"
)

// freshJournal 造一份干净的账本存储（独立 schema ⇒ 各子测试互不干扰）。
func freshJournal(t *testing.T, dsn string) *JournalStore {
	t.Helper()
	ctx := context.Background()
	admin := basePool(t, dsn)
	schema, err := NewSchema(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = DropSchema(ctx, admin, schema) })
	if err := EnsureSchema(ctx, admin, schema); err != nil {
		t.Fatal(err)
	}
	return NewJournalStore(schemaPool(t, dsn, schema))
}

// TestJournalContract 账本面的共享契约套件在真实 PostgreSQL 上原样跑通。
//
// 为什么值得跑：账本面的三条硬要求（身份不可变 / 转移原子 / 幂等）在
// 「读-判-写」拆三步的实现里都会悄悄失效，而单元测试看不出来——只有套件
// 那条「并发推进恰好一个赢家」能撞出来。
func TestJournalContract(t *testing.T) {
	dsn := requireDSN(t)
	invocationtest.Run(t, func(t *testing.T) invocation.Store {
		return freshJournal(t, dsn)
	})
}
