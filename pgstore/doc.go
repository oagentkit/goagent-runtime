// Package pgstore 副作用闸 / 任务认领面 / 进度快照 / 可靠事件面（Outbox 形态）/
// 控制面状态存储的 **PostgreSQL 参考实现**。
//
// 生命周期: Experimental（v0.33.0 定级）——参考实现，随契约套件与参考部署
// 演进，不承诺任何兼容，升级风险自负；定级理由见 api-lifecycle.yaml，分级
// 规则见 docs/api-compat.md。
//
// # 它是什么、不是什么
//
// 这是 examples 里的**参考实现**，不是内核依赖：内核（sideeffect / taskboard /
// checkpoint）依旧只给契约与判据，不给存储。本包演示的是「把同样的判据落到
// 一个真实数据库的原子操作上」长什么样——宿主照抄它，比照着契约散文重写，
// 漂移的概率小一个量级。
//
// 它与 examples/reference-runtime（内存版参考实现）是一对：
//
//   - 内存版证明「判据本身是对的」（进程内互斥锁即可）；
//   - 本包证明「判据在**跨进程**下依然成立」（判据归存储，不归进程）——
//     用内核的共享契约套件（sidetest / boardtest / RunSinkContract）在
//     真实 PostgreSQL 上原样跑通，不为任何实现开后门。
//
// # 三套契约各自的落库形态
//
//   - sideeffect.Store：Acquire 的「三判合一」（已完成 > 结果未知 > 执行中 >
//     资源被占 > 授予）在一个事务里完成——SELECT … FOR UPDATE 行锁把
//     「读状态 → 判 → 写」钉成跨进程原子；执行意图与授予（令牌 + 持有者身份，
//     sideeffect.MintHolder 在同一事务里铸造）写在同一事务里。Complete /
//     Renew / MarkUncertain / Abandon / Release 全部三项联查（令牌 + 持有者 +
//     资源一致），缺一即拒（ErrFenceStale / ErrNotHolder / ErrResourceMismatch）。
//   - sideeffect.FencedSink：一条带条件的 INSERT … ON CONFLICT DO UPDATE
//     （WHERE seen_fence <= presented）先原子预占，写入体在同一事务里执行；
//     写入体报错 ⇒ 整个事务回滚 ⇒ 不得部分写。
//   - taskboard.Board：认领是一个事务里的读-判-写（行锁），令牌与接管代次
//     在同一次操作里铸造；释放 / 完成 / 续期只认令牌。
//   - checkpoint.StoreCAS：一条带条件的 upsert（WHERE 持久化序号 < 本次序号），
//     影响行数为 0 即判陈旧。
//   - controlplane.Store（issue #278）：三个 Snapshot* 在 REPEATABLE READ
//     只读事务里取一致快照；三个 Commit* 在 pgref_control_version 单行版本锚
//     上「校验 → 留痕 → 写状态 → 推进版本」同事务提交（留痕失败 ⇒ 状态一字
//     不动）。任务状态与 taskboard.Board 共用 pgref_board_task 这**一张可写
//     表**，运行成员由该表的 run_id 单源表达；ControlStore 与 BoardStore 的
//     TaskSourceID 同 schema 一致，NewAuthoritativeStorePlane 据此装配期判
//     同源（issue #268）。权威接线仍走 NewAuthoritativeStorePlane：强释经
//     任务板令牌/代次判据，裁决经副作用闸同一份状态。
//   - eventbus 可靠事件面（Outbox 形态，issue #264）：事件行与业务状态同
//     事务提交（PublishInTx——事件与业务写入同生共死），ID 由每租户一行
//     水位表串行化铸造（铸造序 == 提交序，回滚不烧号 ⇒ 序列空洞结构性地不存在），
//     读侧以已提交水位线为上界（TailStore / WatermarkTailStore）。这是
//     eventbus.TailStore 契约点名的唯一被承诺支持的生产形态；自增直读
//     （BIGSERIAL + WHERE id > cursor）被该契约显式声明不支持。
//     尾随游标经 TailCursorStore（pgref_tail_cursor）持久化——实现
//     eventbus.CursorStore，崩溃/停机后从上次停点续读（#264）。
//
// # 租户作用域（issue #275）
//
// effect / 资源 / 下游 / 事件四张表都带 tenant 列，键按 (tenant, 业务键)
// 联合定位：跨租户同名任务与同 Key 动作互不命中。装配期两种形态：
//
//   - 默认（NewEffectStore / NewEventStore / NewDownstream）：旧布局，
//     tenant 取动作 / 事件自带值（空 = ”）；单租户部署逐字不变。
//   - 租户作用域（NewTenantEffectStore / NewTenantEventStore /
//     NewTenantDownstream）：每次调用从 ctx 取租户（tenant.Require），
//     缺租户 ⇒ tenant.ErrNoContext（写入被拒，fail-closed）；声明与 ctx
//     不一致 ⇒ ErrTenantMismatch。
//
// 事件面每个租户一条独立 ID 空间与水位行（pgref_event_watermark 主键
// tenant）：订阅侧用租户作用域实例取流即按租户过滤，单租户流仍满足
// TailStore 的「无空洞推进」——过滤共享 ID 空间会产生永久空洞。
// 尾随游标经 NewTenantTailCursorStore 按 (tenant, consumer) 隔离（同一
// consumer 名在两个租户下是两条游标，跨租户不会互相推进）。
// 旧库由 EnsureSchema 的 vN+1 迁移补齐租户列 / 复合主键（旧行归入 ”）。
//
// # 时钟
//
// 过期判定的时间源（v0.33.0 后修正）：**缺省读数据库的 clock_timestamp**
// （事务内、取得行锁之后），经 UseClock 可注入（sidetest / boardtest 的
// 硬要求：注入值是系统时间的测试替身）。
//
// 为什么不再是进程本地的 time.Now：租约与执行意图是**跨节点共享的事实**，
// 各 worker 用自己的时钟写入/判定到期时刻时，±10s 的时钟漂移对 15s 的
// TTL 意味着「A 写入的租约在 B 看来从一开始就是过期的」⇒ B 提前接管 A
// 仍在执行的资源。数据库只有一个时钟，判定读它，漂移就没有入口。
// 取时刻必须在**取得行锁之后**：CURRENT_TIMESTAMP 固定在事务开始时刻，
// 事务在锁上等了几秒的话，用它判过期会把等锁时长算进租约寿命。
//
// # 数据库连接
//
// 构造函数直接收 *pgxpool.Pool 与 schema 名：本包不管理连接串与凭据，
// 那是宿主的接线责任。隔离方案是**每个实例一个 schema**（EnsureSchema 建
// CREATE TABLE IF NOT EXISTS 一套表），契约套件每个子测试建一份全新实现
// ——与「factory 每次构造全新实现」的套件约定对齐。
//
// # 部署与验证入口
//
//   - 契约 + 故障恢复用例：GAK_PGREF_DSN=… go test ./pgstore（v0.2.0 自 goagent-kernel 迁入）/…
//     （见 examples/docker-compose.yml 与 Makefile 的 integration 目标）；
//   - 冒烟部署：docker compose -f examples/docker-compose.yml up（pgsmoke 服务）。
package pgstore

import (
	"github.com/yi-nology/goagent-kernel/checkpoint"
	"github.com/yi-nology/goagent-kernel/eventbus"
	"github.com/yi-nology/goagent-kernel/sideeffect"
	"github.com/yi-nology/goagent-kernel/taskboard"
)

// 编译期断言：本包的实现确实满足三套存储中立契约（+ 下游栅栏与续期扩展
// + 事件面的四张接缝，#264）。
var (
	_ sideeffect.Store            = (*EffectStore)(nil)
	_ sideeffect.FencedSink       = (*Downstream)(nil)
	_ taskboard.Board             = (*BoardStore)(nil)
	_ taskboard.RenewingBoard     = (*BoardStore)(nil)
	_ checkpoint.StoreCAS         = (*SnapStore)(nil)
	_ eventbus.Store              = (*EventStore)(nil)
	_ eventbus.TailStore          = (*EventStore)(nil)
	_ eventbus.WatermarkTailStore = (*EventStore)(nil)
	_ eventbus.CursorStore        = (*TailCursorStore)(nil)
)
