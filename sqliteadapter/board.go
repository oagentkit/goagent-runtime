// board.go —— taskboard.Board / taskboard.RenewingBoard 的 SQLite 参考实现。
//
// # 判据落库形态
//
//   - **认领是读-判-写不可分割**：一个事务里读任务行 → 判（不存在 / 词表外 /
//     终态 / 依赖 / 被活持有）→ 通过才写。写事务一律 BEGIN IMMEDIATE
//     （_txlock=immediate）：**先拿写锁再干活**，避免 defer 到 COMMIT 才升级
//     锁导致半途失败 ⇒ 并发认领恰好一个赢家。
//   - **令牌与接管代次由存储铸造**：令牌用随机 128 位（跨进程不复用不重合，
//     ABA 封死）；代次 = 该任务已见最大值 + 1，与令牌同一次写落库（同源）。
//   - **释放 / 完成 / 续期只认令牌**：条件更新 WHERE token = ?，影响行数为 0
//     即 ErrLeaseLost（迟到持有者放不掉别人的闸）。
//
// 到期时刻由**存储自己的时钟**算（可注入）：调用方的时钟与存储的时钟可能
// 对不上，跨进程时更不能假设一致。
//
// 本文件是**板面骨架**（类型 / 时钟 / 行投影 / 快照读 / 装载）；租约的写路径
// （认领与收场）在 board_lease.go。
package sqliteadapter

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/oekit/pkg/errors"

	"github.com/yi-nology/goagent-kernel/taskboard"
)

// boardCols 任务行的列（与 pgstore 同序）。
const boardCols = `id, title, deps, state, owner, token, fence, until, seq, run_id`

// BoardStore 任务面（taskboard.Board + taskboard.RenewingBoard）。
type BoardStore struct {
	// exec 执行位置：db 是写句柄，read 是只读句柄（独立读路径走它）。
	exec exec
	// store 归属的 Store：板面时钟的兜底来源（见 clockSource）。
	store *Store
}

// 编译期断言：本实现满足任务板的两套契约。
var (
	_ taskboard.Board         = (*BoardStore)(nil)
	_ taskboard.RenewingBoard = (*BoardStore)(nil)
)

// Board 任务面（与其它面共用同一份事务域）。
//
// 独立读路径（List）走**只读句柄**，事务内的读写走写句柄的 tx——路由判据见
// exec.reader。SQLite 没有 MVCC，读并发靠「读者用自己的连接读已提交快照」
// 实现 ⇒ 只读句柄必须在适配器层显式提供（PG 侧由引擎内生，故 PG 侧不分池）。
func (s *Store) Board() *BoardStore {
	return &BoardStore{exec: s.execBase(), store: s}
}

// UseClock 注入时钟（boardtest.ClockedBoard 硬要求）。
//
// 注入的是**板面自己的**时钟；板面没注入时回落到 Store 级时钟
// （Store.UseClock）——两个注入点合流见 clockSource。
func (b *BoardStore) UseClock(now func() time.Time) {
	b.exec.clock = now
}

// clockSource 取板面的时钟来源：板面注入优先，其次 Store 级注入，都没有 ⇒
// nil（nil ⇒ 读存储时钟）。
//
// 为什么两个注入点要合流而不是拷贝一份 func 值：契约套件注入的是**板面**
// 时钟（ClockedBoard.UseClock），Store.UseClock 是装配期注入；拷贝会让
// 「先 Board() 后 Store.UseClock()」这一顺序**静默**退回存储时钟 ⇒ 时钟口径
// 漂移且没有任何信号 ⇒ 这里按指针解析，两个注入点共用一份真相。
func (b *BoardStore) clockSource() func() time.Time {
	if b.exec.clock != nil {
		return b.exec.clock
	}
	if b.store != nil {
		return b.store.clock
	}
	return nil
}

// execIn 造一个事务内的执行位置（共享工厂在 Store 上，见 Store.execIn）。
//
// 这里只做一件事：把**面级**解析出的时钟交给共享工厂——构造点只有一处，
// 「漏 clock」没有发生的地方。
//
// ⚠ 时钟必须由**面**给：契约套件注入的是**面级**时钟（ClockedBoard.UseClock），
// 若工厂只取 Store 级时钟 ⇒ 板面级注入失效 ⇒ 过期判定读不到注入时钟 ⇒
// 套件立刻红。
func (b *BoardStore) execIn(tx *sql.Tx) exec {
	if b.store == nil {
		// 防御分支：BoardStore 只由 Store.Board() 造 ⇒ b.store 必非 nil。
		// 仍按同一口径补上 tx 与 clock（面里不另抄一份构造逻辑）。
		e := b.exec
		e.tx = tx
		e.clock = b.clockSource()
		return e
	}
	e := b.store.execIn(tx, b.clockSource())
	// 失败槽随执行位置走（批次级状态：各面共享同一份）。
	e.failed = b.exec.failed
	return e
}

// boardRow 任务行（内部表示）。
type boardRow struct {
	id    taskboard.TaskID
	title string
	deps  []taskboard.TaskID
	state taskboard.State
	owner string
	token string
	fence uint64
	until sql.NullInt64
	seq   uint64
	runID string
}

// held 判断该任务当前是否仍被**活持有**（now 为本次操作时刻）。
//
// ⚠ **不得自己写这份判据**：taskboard.LeaseHeld 是「活持有」的单点判据
// （issue #335）——各写一份必然漂（controlplane.MemStore 曾压根不判过期 ⇒
// 持有者崩掉后任务被永久占用，与 Claim 的「被别人**活着**持有」方向相反）。
// 零值到期 = 不限期，当成已过期会让「两个执行者同时认领」变成「两个赢家」。
func (r *boardRow) held(now time.Time) bool {
	return taskboard.LeaseHeld(r.task(), now)
}

// task 把行投影成契约值类型（拷贝语义：调用方改它不影响库）。
//
// Deps 每次投影都是**新切片**（扫描时新解析）⇒ 不与实现内部共享底层数组
// （Task 契约的拷贝义务，issue #379）。
func (r *boardRow) task() taskboard.Task {
	t := taskboard.Task{
		ID:    r.id,
		Title: r.title,
		State: r.state,
		Owner: taskboard.AgentID(r.owner),
		Seq:   r.seq,
	}
	if len(r.deps) > 0 {
		t.Deps = append(t.Deps, r.deps...)
	}
	if r.token != "" {
		t.Lease.Token = r.token
		t.Lease.Fence = taskboard.Fence(r.fence)
	}
	if r.until.Valid {
		t.Lease.Until = time.UnixMilli(r.until.Int64).UTC()
	}
	return t
}

// scanBoardRow 读一行任务（deps 以 JSON 文本存，取回时新解析 ⇒ 不共享底层数组）。
func scanBoardRow(row interface{ Scan(...any) error }) (*boardRow, error) {
	var (
		id    string
		title string
		deps  string
		state string
		owner sql.NullString
		token sql.NullString
		fence int64
		until sql.NullInt64
		seq   int64
		runID string
	)
	if err := row.Scan(&id, &title, &deps, &state, &owner, &token, &fence, &until, &seq, &runID); err != nil {
		return nil, err
	}
	var ids []taskboard.TaskID
	if deps != "" && deps != "[]" && deps != "null" {
		if err := json.Unmarshal([]byte(deps), &ids); err != nil {
			return nil, errors.Wrap(err, "sqliteadapter: 解析依赖失败", "deps", deps)
		}
	}
	return &boardRow{
		id:    taskboard.TaskID(id),
		title: title,
		deps:  ids,
		state: taskboard.State(state),
		owner: owner.String,
		token: token.String,
		fence: uint64(fence),
		until: until,
		seq:   uint64(seq),
		runID: runID,
	}, nil
}

// encodeDeps 把依赖列表压成 JSON 文本（deps 列是 TEXT）。
//
// 单一来源：装载入口（Seed）与控制面提交（CommitTask）都走它 ⇒ 依赖的存储
// 形态只有一份（两处各写一份必然漂）。
func encodeDeps(deps []taskboard.TaskID) (string, error) {
	if len(deps) == 0 {
		return "[]", nil
	}
	raw, err := json.Marshal(deps)
	if err != nil {
		return "", errors.Wrap(err, "sqliteadapter: 序列化依赖失败")
	}
	return string(raw), nil
}

// TaskSourceID 实现 controlplane.TaskSource（issue #268/#278）：同一数据
// 文件的任务板与控制面报同一值 ⇒ NewAuthoritativeStorePlane 据此在装配期
// 判定「同一张任务表」；不同数据文件 ⇒ 不同来源 ⇒ 装配期拒绝。
//
// ⚠ 本面与控制面**必须成对实现**：内核的同源校验是「双方都实现才比对」，
// 只实现一方 ⇒ 装配期直接 ErrAuthorityMismatch（控制面声明了来源而任务
// 权威面没有 ⇒ 无法判定同源）。
func (b *BoardStore) TaskSourceID() string {
	if b.store == nil {
		return ""
	}
	return b.store.sourceID()
}

// List 返回当前全部任务的快照（一致快照：单次查询读全量）。
//
// ⚠ **独立读路径必须走只读句柄**（不是写句柄）：写句柄只有一条连接
// （MaxOpenConns(1)），事务占用它期间从它取连接会**永久等待**（实测：挂到
// ctx 超时）⇒ 走 exec.reader 的路由（tx != nil ⇒ tx；否则 ⇒ 只读句柄）。
func (b *BoardStore) List(ctx context.Context) ([]taskboard.Task, error) {
	q, err := b.exec.reader()
	if err != nil {
		return nil, err
	}
	rows, err := q.QueryContext(ctx, `SELECT `+boardCols+` FROM gak_board_task`)
	if err != nil {
		return nil, errors.Wrap(err, "sqliteadapter: 读任务快照失败")
	}
	defer func() { _ = rows.Close() }()
	var out []taskboard.Task
	for rows.Next() {
		r, err := scanBoardRow(rows)
		if err != nil {
			return nil, errors.Wrap(err, "sqliteadapter: 解析任务行失败")
		}
		out = append(out, r.task())
	}
	if err := rows.Err(); err != nil {
		return nil, errors.Wrap(err, "sqliteadapter: 读任务快照失败")
	}
	// 一致快照按 id 排序输出（判据会排序，这里先排好便于人读）。
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Seed 摆初始任务（任务拓扑是宿主资产；契约里没有「创建」入口）。
//
// 与 pgstore.BoardStore.Seed 同形态：同 id 重摆覆盖内容并清租约，接管代次
// 水位保留；落库失败 panic（装载入口不容静默失败——静默失败会让套件在一张
// 空表上跑出误导性的结论）。
func (b *BoardStore) Seed(tasks ...taskboard.Task) {
	for _, task := range tasks {
		if task.ID == "" {
			continue
		}
		if err := b.seedOne(task); err != nil {
			panic(fmt.Sprintf("sqliteadapter: Seed 摆任务失败（装载入口不容静默失败）: %v", err))
		}
	}
}

// seedOne 摆一条任务。
//
// ⚠ **seq 递增不覆盖**（同 id 重摆时 seq = gak_board_task.seq + 1）：seq 是
// 「任务变更的序号」，单调递增是它的契约；覆盖会让同一个 seq 反复出现 ⇒
// 变更被吞掉且无人发现。
func (b *BoardStore) seedOne(task taskboard.Task) error {
	deps, err := encodeDeps(task.Deps)
	if err != nil {
		return err
	}
	ctx := context.Background()
	return b.exec.run(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO gak_board_task (id, title, deps, state, owner, token, fence, until, seq, run_id)
			 VALUES (?, ?, ?, ?, NULL, NULL, 0, NULL, 1, '')
			 ON CONFLICT(id) DO UPDATE SET
			   title = excluded.title, deps = excluded.deps, state = excluded.state,
			   owner = NULL, token = NULL, until = NULL, seq = gak_board_task.seq + 1`,
			string(task.ID), task.Title, deps, string(task.State))
		if err != nil {
			return errors.Wrap(err, "sqliteadapter: 摆任务失败", "task", string(task.ID))
		}
		return nil
	})
}
