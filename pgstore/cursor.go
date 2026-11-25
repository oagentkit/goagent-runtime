// cursor.go —— eventbus.Tailer 游标持久化（eventbus.CursorStore）的 PostgreSQL
// 参考实现（issue #264 的「崩溃点重启续读」）。
//
// # 为什么需要它
//
// eventbus.Tailer 的游标默认是 Run 的局部状态，停机即失；只有装配
// CursorStore 才能从上次停点续随（eventbus.CursorStore 契约：Load 失败停摆、
// Save 失败不停摆、未命中回退显式起点 / MaxID）。仓内此前只有测试替身
// （eventbus 的 memCursorStore）满足这条接缝，生产宿主接 PostgreSQL 时没有
// 参考落点——本实现补上这一环。
//
// # 语义
//
//   - 一行一个消费者（consumer key）：同一键的游标只有一条，Load 未命中 =
//     found=false（首次部署）；读失败返回 error，Tailer 会停摆重试，绝不
//     静默回退 MaxID（回退即跳过停机窗口）。
//   - **游标单调不减**：SaveCursor 用 GREATEST 吸收回退写（陈旧副本的迟到
//     保存不会把游标拉回去）；非法回退不报错、不落库。
//   - 保存是独立 UPSERT，不与事件发布同事务：游标不是业务状态，Tailer 在
//     投递之后保存——崩溃窗口只会造成重启后重读（订阅侧游标幂等吸收），
//     不会跳读。
//   - 同一 consumer key 只应由一个尾随者写（多副本各用各的键）；同键并发写
//     的语义是「最后落库的最大值获胜」，不承诺无重复投递。
package pgstore

import (
	"context"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/oekit/pkg/errors"

	"github.com/oagentkit/goagent-kernel/eventbus"
)

// TailCursorStore eventbus.CursorStore 的 PostgreSQL 实现：绑定一个 consumer
// key（如「副本名 / 消费组名」），游标落在 pgref_tail_cursor。
type TailCursorStore struct {
	pool     *pgxpool.Pool
	consumer string
}

// NewTailCursorStore 构造游标存储。pool 不得为 nil，consumer 不得为空——
// 两者都是装配期编程错误，fail fast 在构造点（与 eventbus.NewTailer 同口径）。
func NewTailCursorStore(pool *pgxpool.Pool, consumer string) *TailCursorStore {
	if pool == nil {
		panic("pgstore: TailCursorStore 需要连接池（nil pool 的游标无从读写）")
	}
	if consumer == "" {
		panic("pgstore: TailCursorStore 需要 consumer key（空键会让所有尾随者共享一条游标）")
	}
	return &TailCursorStore{pool: pool, consumer: consumer}
}

// 编译期断言：本实现满足 eventbus 的游标持久化接缝。
var _ eventbus.CursorStore = (*TailCursorStore)(nil)

// LoadCursor 读上次保存的游标（0, false, nil = 从未存过；error = 读失败，
// Tailer 会停摆重试，不得静默回退 MaxID）。
func (s *TailCursorStore) LoadCursor(ctx context.Context) (uint64, bool, error) {
	return loadCursor(ctx, s.pool, s.consumer)
}

// SaveCursor 保存游标（UPSERT；单调不减：回退写被 GREATEST 吸收，不落库）。
//
// 失败返回 error，调用方（Tailer）只 warn 不停摆——游标落后只会导致重启后
// 重读（订阅侧幂等吸收），方向安全。
//
// ⚠ **批次进行中 ⇒ 显式拒绝**（见 rejectBatchCursorWrite）：游标走独立提交 ⇒
// 批内写游标会越过被回滚的事件 ⇒ 重启后跳读 ⇒ 静默丢帧。
func (s *TailCursorStore) SaveCursor(ctx context.Context, cursor uint64) error {
	if err := rejectBatchCursorWrite(ctx); err != nil {
		return err
	}
	return saveCursor(ctx, s.pool, s.consumer, cursor)
}

// TenantTailCursorStore **租户作用域**的 eventbus.CursorStore（issue #275）。
//
// 为什么需要它：游标是「某个租户事件流」的续读点。多租户部署若两个租户的
// 尾随者共用一条 consumer key，后跑的会把游标推过对方整段窗口——事件流本身
// 已按租户隔离（NewTenantEventStore），游标却串了，订阅侧静默漏帧。
//
// 装配期声明租户作用域：每次读写都从 ctx 取租户（tenant.Require，缺租户
// ⇒ tenant.ErrNoContext）。物理行仍是 pgref_tail_cursor，consumer 键由
// tenant + 长度前缀 + consumer 派生——长度前缀保证不同 (tenant, consumer)
// 组合拼不出同一个键（consumer 里出现任何分隔符都不影响单射）。
type TenantTailCursorStore struct {
	pool     *pgxpool.Pool
	consumer string
}

// NewTenantTailCursorStore 构造租户作用域游标存储。pool 不得为 nil，
// consumer 不得为空（装配期编程错误 fail fast，与 NewTailCursorStore 同口径）。
func NewTenantTailCursorStore(pool *pgxpool.Pool, consumer string) *TenantTailCursorStore {
	if pool == nil {
		panic("pgstore: TenantTailCursorStore 需要连接池（nil pool 的游标无从读写）")
	}
	if consumer == "" {
		panic("pgstore: TenantTailCursorStore 需要 consumer key（空键会让所有尾随者共享一条游标）")
	}
	return &TenantTailCursorStore{pool: pool, consumer: consumer}
}

var _ eventbus.CursorStore = (*TenantTailCursorStore)(nil)

// TenantScoped 报告本实例是否声明了租户作用域（装配期自检用）。
//
// 租户作用域型恒为 true（**类型本身就是声明**）——保留这个自检口是为了让宿主
// 对两种游标面用同一段装配期断言：把「忘记装配」变成**启动期失败**，而不是
// 运行期的跨租户可见（副作用面的 TenantScoped 同款用途）。
//
// ⚠ 默认型 TailCursorStore **没有**该方法（它不解析租户 ⇒ 无从声明）——与
// SQLite 侧同款（两侧现状一致，不是一边漏了）。
func (s *TenantTailCursorStore) TenantScoped() bool { return true }

// LoadCursor 读当前租户上次保存的游标（缺租户上下文 ⇒ tenant.ErrNoContext）。
func (s *TenantTailCursorStore) LoadCursor(ctx context.Context) (uint64, bool, error) {
	t, err := resolveTenant(ctx, true, "")
	if err != nil {
		return 0, false, err
	}
	return loadCursor(ctx, s.pool, tenantCursorKey(t, s.consumer))
}

// SaveCursor 保存当前租户的游标（缺租户上下文 ⇒ tenant.ErrNoContext）。
//
// ⚠ **批次进行中 ⇒ 显式拒绝**（见 rejectBatchCursorWrite）：游标走独立提交 ⇒
// 批内写游标会越过被回滚的事件 ⇒ 重启后跳读 ⇒ 静默丢帧。
//
// 拒绝**先于**租户解析：批次内的写游标一律按「批内写游标」拒（否则缺租户的
// 拒绝会顶掉它 ⇒ 拒绝进不了失败槽 ⇒ 吞掉它就静默半提交）。
func (s *TenantTailCursorStore) SaveCursor(ctx context.Context, cursor uint64) error {
	if err := rejectBatchCursorWrite(ctx); err != nil {
		return err
	}
	t, err := resolveTenant(ctx, true, "")
	if err != nil {
		return err
	}
	return saveCursor(ctx, s.pool, tenantCursorKey(t, s.consumer), cursor)
}

// rejectBatchCursorWrite 批次进行中时**拒绝**写游标。
//
// 为什么必须显式拒绝（而不是「让它独立提交」）：游标走 pool.Exec ⇒ 另有连接
// ⇒ **独立提交** ⇒ 批内写游标会在外层回滚时**已经落库** ⇒ 游标越过被回滚的
// 事件 ⇒ 重启后从更后面续读 ⇒ **静默丢帧**（全程没有任何报错）。
//
// ⚠ 与 SQLite 侧的失败形态**不同**（Task 9 的选型声明面要写清这条差异）：
//   - SQLite 侧批内写游标 ⇒ **挂到 ctx 超时**（响亮，但慢：写句柄只有一条
//     连接，内层等外层释放）；
//   - PG 侧批内写游标 ⇒ 本函数**显式拒绝**（不挂死、不静默半提交）。
//
// 拒绝要**登记进共享失败槽**：调用方吞掉这个错误时，本批仍必须作废。
//
// 读路径不管（游标读在批内无危害；「失败即作废整批」的承诺只覆盖写路径，与
// 既有口径一致）。
func rejectBatchCursorWrite(ctx context.Context) error {
	if batchFrom(ctx) == nil {
		return nil
	}
	err := errors.New("pgstore: 批次内禁止写游标（游标独立提交 ⇒ 会越过被回滚的事件 ⇒ 重启后跳读）")
	failBatch(ctx, err)
	return err
}

// tenantCursorKey 把 (tenant, consumer) 拼成 pgref_tail_cursor.consumer 的
// 单射键：长度前缀 + tenant + consumer。长度前缀是必要的——只加分隔符时，
// tenant 或 consumer 内含该分隔符就能拼出同一个键（跨租户串游标）。
func tenantCursorKey(tenantID, consumer string) string {
	return strconv.Itoa(len(tenantID)) + ":" + tenantID + consumer
}

func loadCursor(ctx context.Context, pool *pgxpool.Pool, consumer string) (uint64, bool, error) {
	var n int64
	err := pool.QueryRow(ctx,
		`SELECT cursor_pos FROM pgref_tail_cursor WHERE consumer = $1`,
		consumer).Scan(&n)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, errors.Wrap(err, "pgstore: 读尾随游标失败", "consumer", consumer)
	}
	if n < 0 {
		return 0, false, errors.New("pgstore: 尾随游标为负（数据损坏，consumer=" + consumer + "）")
	}
	return uint64(n), true, nil
}

func saveCursor(ctx context.Context, pool *pgxpool.Pool, consumer string, cursor uint64) error {
	if cursor > uint64(1<<63-1) {
		return errors.New("pgstore: 尾随游标超出 BIGINT 范围（consumer=" + consumer + "）")
	}
	_, err := pool.Exec(ctx, `
		INSERT INTO pgref_tail_cursor (consumer, cursor_pos) VALUES ($1, $2)
		ON CONFLICT (consumer) DO UPDATE
		  SET cursor_pos = GREATEST(pgref_tail_cursor.cursor_pos, EXCLUDED.cursor_pos),
		      updated_at = clock_timestamp()
		  WHERE pgref_tail_cursor.cursor_pos < EXCLUDED.cursor_pos`,
		consumer, int64(cursor))
	if err != nil {
		return errors.Wrap(err, "pgstore: 保存尾随游标失败", "consumer", consumer)
	}
	return nil
}
