// board_reader_test.go —— 只读句柄路由规则（exec.reader）的判据。
//
// # 这条规则此前没有任何用例钉住
//
// reader() 的路由判据是「**tx != nil ⇒ 用 tx；否则用只读句柄**」。把它改成
// 「永远返回只读句柄」，全套用例仍然全绿（变异注入 MUT-B 实测 exit=0）。
//
// # 为什么必须钉住
//
// Task 4/5/6 会复用 reader()。一旦某新面在**事务内**经 reader() 读，而
// reader() 被改成永远返回只读句柄 ⇒ 读到的是**已提交快照**（看不到本事务的
// 未提交写）⇒ **读-判-写原子性静默失效** ⇒ 门禁不红 ⇒ 症状是偶发脏数据，
// 而不是报错。
//
// # 两条用例各管一半
//
//   - 单测：reader() 返回的**正是那个 tx**（逐字比对指针，不是比字段值——
//     比字段值的话，另开一条连接的坏实现也能蒙对）。
//   - 面级：事务内（**未提交**）经 reader() 读 ⇒ 读到**未提交值**（证明走
//     的是 tx，而不是只读句柄的已提交快照）。
//
// 两条都带 ctx 超时：坏实现的表现可能是「挂死」而不是干净的断言失败。
package sqliteadapter

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/oagentkit/goagent-kernel/taskboard"
)

// TestExecReaderPrefersTxInsideTransaction reader() 的路由判据：tx 优先。
//
// 逐字比对（同一个 *sql.Tx 指针）：比字段值的话，一个「另开一条连接、字段
// 看着一样」的坏实现也能蒙对。
//
// read 句柄也一并给上：否则「永远返回只读句柄」的坏实现会先撞上「只读句柄
// 未初始化」的报错——那测的就不是路由规则了。
func TestExecReaderPrefersTxInsideTransaction(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s := openStore(t)

	tx, err := s.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("开事务失败: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	e := exec{read: s.read, tx: tx}
	q, err := e.reader()
	if err != nil {
		t.Fatalf("事务内的 reader() 不该报错: %v", err)
	}
	got, ok := q.(*sql.Tx)
	if !ok {
		t.Fatalf("事务内的 reader() 必须返回 tx（实得 %T）——tx 被路由到别处 ⇒ 事务内读到的是别处的快照 ⇒ 读-判-写原子性静默失效", q)
	}
	if got != tx {
		t.Fatalf("reader() 返回的必须**就是**那个 tx（逐字比对），实得另一个事务句柄 %p", got)
	}

	// 规则的另一半：没有 tx ⇒ 走只读句柄（否则「永远返回 tx」的坏实现也能过）。
	q2, err := exec{read: s.read}.reader()
	if err != nil {
		t.Fatalf("非事务内的 reader() 不该报错: %v", err)
	}
	db, ok := q2.(*sql.DB)
	if !ok {
		t.Fatalf("非事务内的 reader() 必须返回只读句柄（实得 %T）", q2)
	}
	if db != s.read {
		t.Fatalf("非事务内的 reader() 必须返回只读句柄本身（逐字比对），实得 %p", db)
	}
}

// TestExecReaderSeesUncommittedWriteInsideTx 事务内经 reader() 读 ⇒ 读到未提交值。
//
// 判据形态：开一个写事务，在里面更新一行**不提交**，再在**同一个事务内**经
// reader() 读它 ⇒ 必须读到未提交值。
//
// 坏实现（reader() 永远返回只读句柄）读到的是**已提交快照**（旧值）⇒ 断言
// 必红：症状是「事务内读-判-写读到了已提交数据」，而门禁不红。
func TestExecReaderSeesUncommittedWriteInsideTx(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s := openStore(t)
	b := s.Board()
	b.Seed(taskboard.Task{ID: "r1", Title: "已提交", State: taskboard.StateOpen})

	// 写事务（BEGIN IMMEDIATE，写锁已拿）：改标题，**不提交**。
	tx, err := s.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("开写事务失败: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx,
		`UPDATE gak_board_task SET title = ? WHERE id = ?`, "未提交", "r1"); err != nil {
		t.Fatalf("写事务内更新失败: %v", err)
	}

	// 事务内经 reader() 读 ⇒ 必须读到**未提交值**（走的是 tx）。
	q, err := exec{read: s.read, tx: tx}.reader()
	if err != nil {
		t.Fatalf("事务内的 reader() 不该报错: %v", err)
	}
	var got string
	if err := q.QueryRowContext(ctx,
		`SELECT title FROM gak_board_task WHERE id = ?`, "r1").Scan(&got); err != nil {
		t.Fatalf("事务内读失败: %v", err)
	}
	if got != "未提交" {
		t.Fatalf("事务内经 reader() 读必须读到未提交值（实得 %q）——读到已提交快照说明 reader() 走了只读句柄 ⇒ 读-判-写原子性静默失效且门禁不红", got)
	}

	// 对照一：同一时刻，只读句柄读到的仍是**已提交快照**（看不到未提交值）。
	// 这半句把两个句柄的语义差异钉住：只读句柄 = 已提交快照，tx = 本事务视图。
	var viaRead string
	if err := s.read.QueryRowContext(ctx,
		`SELECT title FROM gak_board_task WHERE id = ?`, "r1").Scan(&viaRead); err != nil {
		t.Fatalf("只读句柄读失败: %v", err)
	}
	if viaRead != "已提交" {
		t.Fatalf("只读句柄必须读已提交快照（看不到未提交值），实得 %q", viaRead)
	}

	// 对照二：提交之后，只读句柄才看得到该值（提交没落地 ⇒ 提交本身失效）。
	if err := tx.Commit(); err != nil {
		t.Fatalf("提交失败: %v", err)
	}
	var after string
	if err := s.read.QueryRowContext(ctx,
		`SELECT title FROM gak_board_task WHERE id = ?`, "r1").Scan(&after); err != nil {
		t.Fatalf("提交后只读句柄读失败: %v", err)
	}
	if after != "未提交" {
		t.Fatalf("提交后只读句柄必须看得到已提交值（实得 %q）", after)
	}
}
