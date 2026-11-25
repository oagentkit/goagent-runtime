// journal_test.go —— 账本面（invocation.Store）在 SQLite 上的共享契约
// 套件。
//
// 形态与其它面同款：环境入口 skip（无库时不伪造结果），套件场景**零 skip**。
package sqliteadapter

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/oagentkit/goagent-kernel/invocation"
	"github.com/oagentkit/goagent-kernel/invocation/invocationtest"
)

// freshJournalDB 造一份干净的账本库（每个子测试一份 ⇒ 互不干扰）。
func freshJournalDB(t *testing.T) *sql.DB {
	t.Helper()
	ctx := context.Background()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "state.db")+dsnParams)
	if err != nil {
		t.Fatalf("开句柄失败: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, schemaDDL); err != nil {
		t.Fatalf("建表失败: %v", err)
	}
	return db
}

// TestJournalContract 账本面的共享契约套件在 SQLite 上原样跑通。
func TestJournalContract(t *testing.T) {
	invocationtest.Run(t, func(t *testing.T) invocation.Store {
		return NewJournalStore(freshJournalDB(t))
	})
}
