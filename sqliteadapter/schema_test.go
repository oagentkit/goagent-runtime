// schema_test.go —— 建表的事务性判据（坏实现必红）。
//
// 这组用例判的是一条**写进注释的契约**：全部建表语句在**一个事务**里执行，
// 中途失败不留半截 schema。SQLite 在无显式事务时按 autocommit 逐条提交 ⇒
// 中途失败**确实会**留半截 schema ⇒ 这条契约需要一个能钉住它的判据。
//
// 两条用例各堵一个逃逸口：
//   - 注入 DDL（ensureSchema）：钉住「事务套」本身。
//   - 公开路径（EnsureSchema）：钉住「公开路径确实走在那条事务套上」。

package sqliteadapter

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// countTables 数一张表在 sqlite_master 里的条数（0 = 不存在）。
func countTables(t *testing.T, s *Store, name string) int {
	t.Helper()
	var n int
	if err := s.DB().QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&n); err != nil {
		t.Fatalf("读 sqlite_master 失败: %v", err)
	}
	return n
}

// TestEnsureSchemaRollsBackPartialDDL 建表中途失败 ⇒ 半截 schema 一张都不留。
//
// 判据形态：多语句 DDL（先建一张表，再执行一条语法错误语句）。
//   - 好实现（一个事务内跑）：中途失败 ⇒ 整条回滚 ⇒ 出错前建的那张表不留。
//   - 坏实现（裸跑，autocommit）：第一条已提交 ⇒ 半截 schema 留存 ⇒ 本用例红。
//
// 断言只查表名在不在 sqlite_master 里，不看行数——「留下半截」本身就是缺陷。
func TestEnsureSchemaRollsBackPartialDDL(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	ddl := `CREATE TABLE gak_ddl_partial (id TEXT PRIMARY KEY);
THIS IS NOT VALID SQL;
`
	if err := s.ensureSchema(ctx, ddl); err == nil {
		t.Fatal("多语句 DDL 中途语法错误必须报错")
	}
	if n := countTables(t, s, "gak_ddl_partial"); n != 0 {
		t.Fatalf("建表中途失败后不得留下半截 schema：表 gak_ddl_partial 仍在（%d 行）⇒ 建表不在一个事务里", n)
	}
}

// TestEnsureSchemaPublicPathIsAtomic 公开路径 EnsureSchema 也在一个事务里。
//
// 上一条用例走的是注入接缝 ensureSchema；本条走**公开路径** EnsureSchema ⇒
// 堵住「公开路径绕过事务套、直接裸跑真 DDL」这个逃逸口（那条路注入接缝测不到）。
//
// 注入失败点用触发器造：真 DDL 里那条 INSERT 命中触发器 ⇒ DDL 中途失败。
// 前置条件（触发器必须晚于至少一条 CREATE TABLE）写在用例里显式断言——
// 前提不成立时本用例当场红，不静默退化成「没有判别力的绿」。
func TestEnsureSchemaPublicPathIsAtomic(t *testing.T) {
	// 前提：注入失败点必须晚于至少一条 CREATE TABLE，否则本用例没有判别力。
	iCreate := strings.Index(schemaDDL, "CREATE TABLE IF NOT EXISTS gak_board_task")
	iInsert := strings.Index(schemaDDL, "INSERT INTO gak_control_version")
	if iCreate < 0 || iInsert < 0 || iCreate > iInsert {
		t.Fatalf("前提不成立：注入失败点须晚于第一条 CREATE TABLE（create=%d insert=%d）", iCreate, iInsert)
	}
	s, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("打开失败: %v", err)
	}
	defer func() { _ = s.Close() }()
	// 造一个必炸的插入点：真 DDL 里那条 INSERT 命中触发器 ⇒ DDL 中途失败。
	setup := `CREATE TABLE gak_control_version (id INTEGER PRIMARY KEY);
CREATE TRIGGER gak_trg_block_insert BEFORE INSERT ON gak_control_version
BEGIN SELECT RAISE(ABORT, 'gak injected ddl failure'); END;
`
	if _, err := s.DB().ExecContext(context.Background(), setup); err != nil {
		t.Fatalf("造注入失败点失败: %v", err)
	}
	if err := s.EnsureSchema(context.Background()); err == nil {
		t.Fatal("真 DDL 命中注入失败点必须报错")
	}
	if n := countTables(t, s, "gak_board_task"); n != 0 {
		t.Fatalf("公开路径建表中途失败后不得留下半截 schema：表 gak_board_task 仍在 ⇒ EnsureSchema 不在一个事务里")
	}
}
