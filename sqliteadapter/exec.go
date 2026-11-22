// exec.go —— 执行位置抽象：独立事务，或当前事务内。
//
// 判据单一来源的前提：两种执行位置共用同一套判据函数 ⇒ 事务内走的判据与
// 独立提交**逐字相同**，不会因为「换了个入口」而漂。这是跨面 Apply 的前提。

package sqliteadapter

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/oekit/pkg/errors"
)

// exec 一次操作的执行位置：独立事务，或当前事务内。
//
// 两种位置共用同一套判据函数 ⇒ 事务内走的判据与独立提交**逐字相同**，
// 不会因为「换了个入口」而漂。这是跨面 Apply 的前提。
type exec struct {
	// db 写句柄：MaxOpenConns(1)，_txlock=immediate（写事务先拿写锁）。
	db *sql.DB
	// read 只读句柄：独立连接池，_txlock=deferred，引擎层只读。
	//
	// 为什么 SQLite 侧要分句柄而 PG 侧不用：PG 有 MVCC，读不阻塞写是引擎
	// 内生的；SQLite 没有 MVCC，WAL 下的读并发靠「读者用自己的连接读已提交
	// 快照」实现 ⇒ 只读句柄要在适配器层显式提供。
	read *sql.DB
	// tx 当前事务（nil ⇒ 不在事务内）。
	tx *sql.Tx
	// clock 注入的时钟（契约套件硬要求）；nil ⇒ 用存储时钟。
	clock func() time.Time
	// failed 本批第一次失败（**仅**事务内形态使用；nil ⇒ 不在任何批次里）。
	//
	// 为什么要有它：事务内某个面判失败时，调用方**可能吞掉那个错误**继续
	// 写别的面 ⇒ 就会出现「一半已提交」。失败一旦发生就作废整批，是唯一能
	// 杜绝这种半截事务的形态（失败形态是变红，不是留半截状态）。
	//
	// ⚠ 它是**记录点**，不是判定点：判定只有一处（Apply 的提交闸）。两处
	// 都判 ⇒ 同一个判断有两个主人 ⇒ 改坏一处测试看不出区别。
	//
	// 指针而非值：各面共享**同一份**槽 ⇒ 任一面失败，其余面都看得见。
	failed *error
}

// queryer 读句柄（能发起查询的一侧：*sql.DB 与 *sql.Tx 都满足）。
type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// reader 取本次操作的读句柄。
//
// 路由判据：**tx != nil ⇒ 用 tx；否则用只读句柄。**
//   - 事务内的读一律走 tx（读-判-写原子性靠这条保证）；
//   - 独立读路径走只读句柄：写句柄只有一条连接，事务占用它期间从它取连接
//     会**永久等待**（实测：挂到 ctx 超时）。
func (e exec) reader() (queryer, error) {
	if e.tx != nil {
		return e.tx, nil
	}
	if e.read == nil {
		return nil, errors.New("sqliteadapter: 只读句柄未初始化（独立读路径无路可走）")
	}
	return e.read, nil
}

// fail 登记本批的一次失败（只记**第一次**）。
//
// 为什么是 exec 上的方法而不是 Apply 上的闭包：失败可能发生在**进 exec.run
// 之前**（租户解析拒绝、快照的落库前校验）⇒ 那些路径也得能登记，否则
// 「失败即作废整批」对它们是假的。
//
// 独立事务形态（failed == nil）⇒ 登记是空操作。
func (e exec) fail(err error) {
	if err == nil || e.failed == nil {
		return
	}
	if *e.failed == nil {
		*e.failed = err
	}
}

// run 在执行位置里跑一次操作。
func (e exec) run(ctx context.Context, fn func(tx *sql.Tx) error) error {
	if e.tx != nil {
		// 事务内：调用方拿到的是同一个 tx，不再开事务、不再提交。
		if err := fn(e.tx); err != nil {
			// 失败登记：调用方**可能吞掉**这个错误 ⇒ 本批必须作废。
			// 登记只发生在事务内形态（独立事务没有批次可作废）。
			e.fail(err)
			return err
		}
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	tx, err := e.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqliteadapter: 开事务失败: %w", err)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqliteadapter: 提交事务失败: %w", err)
	}
	return nil
}

// now 取本次操作的时刻。
//
// 注入时钟优先（套件硬要求）；否则读**存储时钟** unixepoch('subsec')。
//
// 两条纪律：
//   - 不用进程本地 time.Now——租约与执行意图是跨进程共享的事实，各进程用
//     自己的时钟写入/判定到期时刻时，±10s 的漂移对 15s 的 TTL 意味着
//     「A 写入的租约在 B 看来从一开始就是过期的」。
//   - 取时刻在**拿到写锁之后**——BEGIN IMMEDIATE 已持写锁，此刻取的才是
//     「真正开始干活」的时刻（unixepoch('subsec') 不是 BEGIN 冻结值，
//     实测同事务内两次取时刻相差 202ms）。
//
// 精度：SQLite 内建时钟是**毫秒级**（实测最小非零增量 0.9999ms），
// 比 PG 的 clock_timestamp（微秒）粗。租约 TTL 建议 ≥ 1s。
//
// tx 必须显式传：nil tx 且**无注入时钟** ⇒ 返回显式错误（fail loud），
// 不空指针 panic。
func (e exec) now(ctx context.Context, tx *sql.Tx) (time.Time, error) {
	if c := e.clockOverride(); c != nil {
		return c(), nil
	}
	// 为什么不让 now 自己去 e.db 取时钟：
	//   - exec.run 的回调拿到的 tx 与 exec 值里的 tx **不是同一个**（run 期间
	//     exec.tx 仍为 nil）⇒ 去 e.db 取走的不是那条事务连接。
	//   - 违反设计稿「取时刻在拿到写锁之后」；单连接池下更是直接与占着
	//     唯一连接的写事务抢连接 ⇒ 挂到 ctx 超时。
	// ⇒ nil tx 是**调用方漏传**的编程错误，显式报错让人一眼看出。
	if tx == nil {
		return time.Time{}, fmt.Errorf("sqliteadapter: 读存储时钟必须传事务句柄：tx 为 nil（调用方漏传 tx）")
	}
	var ms int64
	if err := tx.QueryRowContext(ctx, `SELECT CAST(unixepoch('subsec') * 1000 AS INTEGER)`).Scan(&ms); err != nil {
		return time.Time{}, fmt.Errorf("sqliteadapter: 读存储时钟失败: %w", err)
	}
	return time.UnixMilli(ms).UTC(), nil
}

// clockOverride 返回注入的时钟；nil ⇒ 读存储时钟。
func (e exec) clockOverride() func() time.Time { return e.clock }
