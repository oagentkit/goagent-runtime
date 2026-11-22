package sqliteadapter

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// schemaTables 参考表清单（逐张表管一件事，见 doc.go）。
var schemaTables = []string{
	"gak_board_task",
	"gak_control_effect",
	"gak_control_version",
	"gak_downstream",
	"gak_effect_action",
	"gak_effect_resource",
	"gak_event",
	"gak_event_watermark",
	"gak_run",
	"gak_snapshot",
	"gak_tail_cursor",
}

// assertSchemaTables 逐张核对参考表都在。
//
// DDL 是一条多语句串：驱动只跑第一条的话，这里会当场露馅（表少一半）。
func assertSchemaTables(t *testing.T, s *Store) {
	t.Helper()
	rows, err := s.DB().QueryContext(context.Background(),
		`SELECT name FROM sqlite_master WHERE type = 'table' AND name LIKE 'gak_%'`)
	if err != nil {
		t.Fatalf("读 sqlite_master 失败: %v", err)
	}
	defer func() { _ = rows.Close() }()
	got := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("扫表名失败: %v", err)
		}
		got[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("读表名出错: %v", err)
	}
	for _, want := range schemaTables {
		if !got[want] {
			t.Errorf("参考表 %s 缺失（建表只跑了一部分？）", want)
		}
	}
}

// openStore 开一个建好表的临时库（用例共用）。
func openStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("打开失败: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.EnsureSchema(context.Background()); err != nil {
		t.Fatalf("建表失败: %v", err)
	}
	return s
}

// TestOpenCreatesSchemaAndIsReopenable 打开即建表；关掉重开仍在（持久化）。
func TestOpenCreatesSchemaAndIsReopenable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")

	s, err := Open(path)
	if err != nil {
		t.Fatalf("打开失败: %v", err)
	}
	if err := s.EnsureSchema(context.Background()); err != nil {
		t.Fatalf("建表失败: %v", err)
	}
	assertSchemaTables(t, s)
	if err := s.Close(); err != nil {
		t.Fatalf("关闭失败: %v", err)
	}

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("重开失败: %v", err)
	}
	defer func() { _ = s2.Close() }()
	if err := s2.EnsureSchema(context.Background()); err != nil {
		t.Fatalf("重开建表（幂等）失败: %v", err)
	}
	assertSchemaTables(t, s2)
}

// TestOpenRejectsEmptyPath 空路径是装配期编程错误，fail fast。
func TestOpenRejectsEmptyPath(t *testing.T) {
	if _, err := Open(""); err == nil {
		t.Fatal("空路径必须报错（不得静默开一个匿名库）")
	}
}

// TestOpenCreatesParentDirectory 数据目录不存在时自动建出来。
//
// 数据**文件**在首次真正落库时才出现：Open 不建连（sql.Open 是惰性的，
// 只解析 DSN 不连库）⇒ 省掉一次无谓的连接往返；但目录必须先备好，否则
// 首次落库会因「目录不存在」失败。
func TestOpenCreatesParentDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "deep")
	path := filepath.Join(dir, "state.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("打开失败: %v", err)
	}
	defer func() { _ = s.Close() }()
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		t.Fatalf("数据目录应已建出: %v", err)
	}
	if err := s.EnsureSchema(context.Background()); err != nil {
		t.Fatalf("建表失败: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("落库后数据文件应存在: %v", err)
	}
}

// TestOpenRejectsUnusableDirectory 数据目录建不出来时报错（不静默开一个坏库）。
func TestOpenRejectsUnusableDirectory(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("造阻挡文件失败: %v", err)
	}
	if _, err := Open(filepath.Join(blocker, "state.db")); err == nil {
		t.Fatal("数据目录不可用时必须报错")
	}
}

// TestStoreAccessors 门面访问器与数据文件路径对账。
func TestStoreAccessors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("打开失败: %v", err)
	}
	defer func() { _ = s.Close() }()
	if s.DB() == nil {
		t.Fatal("DB() 不得为 nil")
	}
	if s.Path() != path {
		t.Fatalf("Path() = %q，期望 %q", s.Path(), path)
	}
}

// TestCloseOnUnopenedStore 未开库的 Store 关闭不报错（幂等）。
func TestCloseOnUnopenedStore(t *testing.T) {
	s := &Store{}
	if err := s.Close(); err != nil {
		t.Fatalf("关闭未开库的 Store 不得报错: %v", err)
	}
}

// TestExecRunCommits 独立事务：回调成功即提交，写入对后续读可见。
func TestExecRunCommits(t *testing.T) {
	s := openStore(t)
	e := s.exec()
	if err := e.run(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(context.Background(),
			`INSERT INTO gak_run (id, status) VALUES ('r1', 'ok')`)
		return err
	}); err != nil {
		t.Fatalf("独立事务失败: %v", err)
	}
	var n int
	if err := s.DB().QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM gak_run WHERE id = 'r1'`).Scan(&n); err != nil {
		t.Fatalf("读回失败: %v", err)
	}
	if n != 1 {
		t.Fatalf("提交后应读到 1 行，实得 %d", n)
	}
}

// TestExecRunRollsBack 独立事务：回调报错即回滚，不留半截写。
func TestExecRunRollsBack(t *testing.T) {
	s := openStore(t)
	e := s.exec()
	err := e.run(context.Background(), func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(context.Background(),
			`INSERT INTO gak_run (id, status) VALUES ('r2', 'ok')`); err != nil {
			return err
		}
		return fmt.Errorf("注入失败")
	})
	if err == nil {
		t.Fatal("回调报错必须如实返回")
	}
	var n int
	if err := s.DB().QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM gak_run WHERE id = 'r2'`).Scan(&n); err != nil {
		t.Fatalf("读回失败: %v", err)
	}
	if n != 0 {
		t.Fatalf("回滚后不得留下半截写，实得 %d 行", n)
	}
}

// TestExecRunInsideTransaction 事务内执行位置：不再开事务，调用方提交才生效。
func TestExecRunInsideTransaction(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	tx, err := s.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("开事务失败: %v", err)
	}
	e := exec{db: s.DB(), tx: tx}
	same := false
	if err := e.run(ctx, func(got *sql.Tx) error {
		same = got == tx
		_, err := got.ExecContext(ctx,
			`INSERT INTO gak_run (id, status) VALUES ('r3', 'ok')`)
		return err
	}); err != nil {
		t.Fatalf("事务内执行失败: %v", err)
	}
	if !same {
		t.Fatal("事务内执行位置必须复用调用方的事务（不得另开一个）")
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("回滚失败: %v", err)
	}
	var n int
	if err := s.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM gak_run WHERE id = 'r3'`).Scan(&n); err != nil {
		t.Fatalf("读回失败: %v", err)
	}
	if n != 0 {
		t.Fatalf("外层事务回滚后不得留下写入，实得 %d 行", n)
	}
}

// TestExecRunHonorsCanceledContext 上下文已取消时不碰库。
func TestExecRunHonorsCanceledContext(t *testing.T) {
	s := openStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := s.exec().run(ctx, func(tx *sql.Tx) error {
		t.Fatal("上下文已取消时不得进入回调")
		return nil
	})
	if err == nil {
		t.Fatal("上下文已取消必须报错")
	}
}

// TestExecNowUsesInjectedClock 注入了时钟就用它（契约套件的硬要求）。
func TestExecNowUsesInjectedClock(t *testing.T) {
	s := openStore(t)
	fixed := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s.UseClock(func() time.Time { return fixed })
	e := s.exec()
	if e.clockOverride() == nil {
		t.Fatal("注入时钟后 clockOverride() 不得为 nil")
	}
	got, err := e.now(context.Background(), nil)
	if err != nil {
		t.Fatalf("取时刻失败: %v", err)
	}
	if !got.Equal(fixed) {
		t.Fatalf("注入时钟时应得 %v，实得 %v", fixed, got)
	}
}

// TestExecNowUsesStorageClock 未注入时钟时读存储时钟（毫秒级）。
func TestExecNowUsesStorageClock(t *testing.T) {
	s := openStore(t)
	e := s.exec()
	if e.clockOverride() != nil {
		t.Fatal("未注入时钟时 clockOverride() 应为 nil")
	}
	ctx := context.Background()
	tx, err := s.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("开事务失败: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	got, err := e.now(ctx, tx)
	if err != nil {
		t.Fatalf("取时刻失败: %v", err)
	}
	if got.IsZero() {
		t.Fatal("存储时钟不得为零时刻")
	}
	if got.Nanosecond()%int(time.Millisecond) != 0 {
		t.Fatalf("存储时钟是毫秒级，实得 %v", got)
	}
}

// TestExecNowNilTxWithoutClockFailsLoud nil tx 且无注入时钟 ⇒ 显式报错，不 panic。
//
// now 必须在**同一条事务连接**上取时刻（取时刻在拿到写锁之后）⇒ 调用方
// 漏传 tx 是编程错误。原先的实现直接 tx.QueryRowContext ⇒ nil 指针 panic；
// 这里要求 fail loud：报错且信息能让人一眼看出是「调用方没传 tx」。
func TestExecNowNilTxWithoutClockFailsLoud(t *testing.T) {
	s := openStore(t)
	e := s.exec()
	if e.clockOverride() != nil {
		t.Fatal("本用例要求未注入时钟")
	}
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("nil tx 不得 panic（应返回显式错误），实得 panic: %v", r)
		}
	}()
	got, err := e.now(context.Background(), nil)
	if err == nil {
		t.Fatalf("nil tx 且无注入时钟必须报错，实得时刻 %v", got)
	}
	if !got.IsZero() {
		t.Fatalf("出错时不得返回时刻，实得 %v", got)
	}
	if !strings.Contains(err.Error(), "tx") {
		t.Fatalf("错误信息须点明是调用方漏传 tx，实得: %v", err)
	}
}

// TestExecNowNilTxWithInjectedClock nil tx 但注入了时钟 ⇒ 用注入时刻（不碰库）。
//
// 注入时钟优先于存储时钟 ⇒ nil tx 在这条路径上无害（根本不走库）。
func TestExecNowNilTxWithInjectedClock(t *testing.T) {
	s := openStore(t)
	fixed := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	s.UseClock(func() time.Time { return fixed })
	e := s.exec()
	got, err := e.now(context.Background(), nil)
	if err != nil {
		t.Fatalf("注入时钟时 nil tx 不得报错: %v", err)
	}
	if !got.Equal(fixed) {
		t.Fatalf("应得注入时刻 %v，实得 %v", fixed, got)
	}
}

// TestExecNowWithTxReturnsStorageClock 有 tx ⇒ 读存储时钟，落在调用时刻附近。
//
// 断言只取「合理区间」：SQLite 内建时钟是毫秒级、且与进程时钟同源，
// 不写会抖的精确断言。
func TestExecNowWithTxReturnsStorageClock(t *testing.T) {
	s := openStore(t)
	e := s.exec()
	ctx := context.Background()
	tx, err := s.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("开事务失败: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	before := time.Now()
	got, err := e.now(ctx, tx)
	after := time.Now()
	if err != nil {
		t.Fatalf("取时刻失败: %v", err)
	}
	if got.IsZero() {
		t.Fatal("存储时钟不得为零时刻")
	}
	const slack = 2 * time.Second
	if got.Before(before.Add(-slack)) || got.After(after.Add(slack)) {
		t.Fatalf("存储时钟 %v 应落在调用区间 [%v, %v] 附近（容差 %v）", got, before, after, slack)
	}
	if got.Nanosecond()%int(time.Millisecond) != 0 {
		t.Fatalf("存储时钟是毫秒级，实得 %v", got)
	}
}
