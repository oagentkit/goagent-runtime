// store.go —— 门面骨架：开库、装配期选项、生命周期。
//
// 门面只管三件事：开库、装选项、关库。门面暴露的各个面（任务板 / 副作用闸 /
// 进度快照 / 事件面 / 下游栅栏，其中副作用闸 / 事件面 / 下游栅栏另有租户作用域
// 变体；尾随游标，含租户作用域变体）与控制面、跨面 Apply **共用同一份** exec
// 执行位置抽象（exec.go），因此「独立提交」与「事务内」两条路径走的是同一套
// 判据函数。

package sqliteadapter

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/oekit/pkg/errors"

	_ "modernc.org/sqlite"
)

// dsnParams 打开参数：
//   - _txlock=immediate：写事务一律 BEGIN IMMEDIATE（先拿写锁再干活）。
//   - _busy_timeout=5000：跨进程写锁的等待上限（不静默丢写）。
//   - _journal_mode=WAL：读不阻塞写，崩溃后按 WAL 重放。
//   - _synchronous=FULL：synchronous=FULL（驱动**同时接受枚举词与数字**：
//     0 OFF / 1 NORMAL / 2 FULL / 3 EXTRA——写 FULL 与写 2 等价
//     （实测两者都是 PRAGMA synchronous=2）；只有**非法值**才报错，
//     如 _synchronous=bogus 会显式报 invalid _synchronous）。
const dsnParams = "?_txlock=immediate&_busy_timeout=5000&_journal_mode=WAL&_synchronous=FULL"

// dsnReadParams 只读句柄的打开参数：
//   - _txlock=deferred：只读句柄上的事务**不抢写锁**（复用写 DSN 的 immediate
//     会让读事务也去排队写锁）。
//   - _query_only=true：引擎层的只读闸（写语句直接被拒）——「只读句柄」不是
//     名字，是真的只读。驱动把它**最后**应用（写在可写阶段的 pragma 之后）。
//   - 其余与写句柄同档（WAL / busy_timeout / synchronous）。
//
// 为什么 SQLite 侧要分句柄而 PG 侧不用：PG 有 MVCC，读不阻塞写是引擎内生的；
// SQLite 没有 MVCC，WAL 下的读并发靠「读者用自己的连接读已提交快照」实现
// ⇒ 只读句柄要在适配器层显式提供。形态同构指的是**契约语义**同构，不是连接
// 管理同构。
const dsnReadParams = "?_txlock=deferred&_busy_timeout=5000&_journal_mode=WAL&_synchronous=FULL&_query_only=true"

// readMaxOpenConns 只读句柄的连接池上限（读并发才有意义；写句柄仍是一连接）。
const readMaxOpenConns = 4

// Option 装配期选项。
type Option func(*Store)

// WithClock 注入时钟（契约套件的硬要求；生产不装 ⇒ 读存储时钟）。
func WithClock(now func() time.Time) Option {
	return func(s *Store) { s.clock = now }
}

// Store 一个 SQLite 后端：各面 + 控制面 + 游标，共用一份事务域。
type Store struct {
	// db 写句柄：MaxOpenConns(1)，_txlock=immediate（写事务先拿写锁）。
	db *sql.DB
	// read 只读句柄：独立连接池，_txlock=deferred，引擎层只读——独立读路径
	// （List）走它，事务内的读一律走 tx（路由判据见 exec.reader）。
	read  *sql.DB
	path  string
	clock func() time.Time
}

// Open 打开（或建）一个 SQLite 后端。
//
// path 为空是装配期编程错误，fail fast（不得静默开一个匿名库——那会让
// 「重启后状态还在」这条判据无从验证）。
func Open(path string, opts ...Option) (*Store, error) {
	if path == "" {
		return nil, fmt.Errorf("sqliteadapter: 数据文件路径不能为空")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("sqliteadapter: 建数据目录失败: %w", err)
	}
	db, err := sql.Open("sqlite", "file:"+path+dsnParams)
	if err != nil {
		return nil, fmt.Errorf("sqliteadapter: 打开数据库失败: %w", err)
	}
	// 单写连接：进程内多个 goroutine 的写串行化。
	// ⚠️ 职责**只限进程内**——跨进程串行化靠 BEGIN IMMEDIATE + 文件锁。
	db.SetMaxOpenConns(1)

	// 独立只读句柄：独立读路径（List）走它。
	//
	// 为什么不能复用写句柄：写句柄只有一条连接，事务占用它期间从它取连接
	// 会**永久等待**（实测：挂到 ctx 超时）⇒ 独立读路径在写事务进行中直接
	// 挂起。另一个句柄读的是**已提交快照**（看不到未提交数据），与 PG 的
	// MVCC 快照语义一致——这才是真正要对齐的东西。
	ro, err := sql.Open("sqlite", "file:"+path+dsnReadParams)
	if err != nil {
		// 只读句柄开不出来时把已开的写句柄关掉——不泄漏连接。
		_ = db.Close()
		return nil, errors.Wrap(err, "sqliteadapter: 打开只读句柄失败")
	}
	ro.SetMaxOpenConns(readMaxOpenConns)

	s := &Store{db: db, read: ro, path: path}
	for _, o := range opts {
		o(s)
	}
	return s, nil
}

// Close 关闭数据库（不删除数据）。
//
// 两个句柄都关：只读句柄漏关 ⇒ 连接与文件句柄泄漏（且库文件删不掉）。
// ⚠ 判空要**两个都判**（只判 db 会让只读句柄漏关）。
func (s *Store) Close() error {
	if s.db == nil && s.read == nil {
		return nil
	}
	var err error
	if s.read != nil {
		err = s.read.Close()
	}
	if s.db != nil {
		if cerr := s.db.Close(); err == nil {
			err = cerr
		}
	}
	return err
}

// UseClock 注入时钟（boardtest.ClockedBoard / sidetest.ClockedStore 硬要求）。
func (s *Store) UseClock(now func() time.Time) {
	s.clock = now
}

// DB 暴露底层句柄（装配期与测试用；运行期不直接用）。
func (s *Store) DB() *sql.DB { return s.db }

// Path 数据文件路径（对账用）。
func (s *Store) Path() string { return s.path }

// sourceID 存储来源标识：同一数据文件的面（任务板 / 控制面）报同一值。
//
// 与 PG 侧 poolSourceID 同款用途：NewAuthoritativeStorePlane 据此在装配期
// 判定「同一张任务表」（不同数据文件 ⇒ 不同来源 ⇒ 装配期拒绝）。
func (s *Store) sourceID() string { return "sqliteadapter:" + s.path }

// exec 造一个**独立事务**的执行位置（判据单一来源的入口）。
//
// 事务内的执行位置走共享工厂 Store.execIn——两者共用同一套判据函数，只是
// 「谁负责提交」不同。
func (s *Store) exec() exec { return exec{db: s.db, read: s.read, clock: s.clock} }

// execBase 造一个**面级**执行位置的基座（**不带时钟**）。
//
// 为什么不带时钟：面级时钟在**调用时**解析（见 BoardStore.clockSource）——
// 构造时抓一份 ⇒ 「先 UseClock(A) 再 Board() 再 UseClock(B)」这一顺序会让面
// 继续用旧时钟 A ⇒ 时钟口径漂移且门禁不红。
func (s *Store) execBase() exec { return exec{db: s.db, read: s.read} }

// execOf 造一个**独立事务**的执行位置（写句柄与只读句柄同源）。
//
// 为什么单独一个工厂：exec 字面量的构造点必须集中在一处（见
// exec_factory_test.go 的 AST 判据）——面里手搓 exec{...} 迟早漏 clock ⇒
// 注入时钟在该面上静默失效 ⇒ 时钟口径漂移且门禁不红。
//
// 为什么只读句柄与写句柄同源：独立构造器只拿到 *sql.DB，拿不到数据文件路径
// ⇒ 开不出第二个只读句柄（只读句柄要另开一个连接，见 Store.Open）。
// 代价：写事务进行中时，独立读路径会与写句柄抢连接而排队；需要「读不阻塞
// 写」的宿主请用 Store.Effects()（门面自带独立只读句柄）。
func execOf(db *sql.DB) exec { return exec{db: db, read: db} }

// execIn 造一个**事务内**的执行位置（带 clock —— 漏了会让注入时钟静默失效）。
//
// 为什么放 Store 而不是每个面一份：面各抄一份 ⇒ 迟早有一份漏 clock ⇒
// 注入时钟在该面上静默失效 ⇒ 时钟口径漂移且门禁不红（Task 2 评审 M2、
// Task 3 评审同型）。构造点集中在一处 ⇒ 「漏 clock」没有发生的地方。
//
// clock 由**面**传入（面级注入优先 ⇒ Store 级 ⇒ nil，见 BoardStore.clockSource）：
// 契约套件注入的是**面级**时钟（boardtest.ClockedBoard.UseClock），若这里只取
// s.clock ⇒ 板面级注入失效 ⇒ 过期判定读不到注入时钟 ⇒ 套件立刻红。
func (s *Store) execIn(tx *sql.Tx, clock func() time.Time) exec {
	return exec{db: s.db, read: s.read, tx: tx, clock: clock}
}
