// Package sqliteadapter 单机持久化的**真 SQLite 参考实现**。
//
// 生命周期: Experimental（v0.50.0 定级）——参考实现，随契约套件与参考部署演进，
// 不承诺任何兼容，升级风险自负；定级理由见 api-lifecycle.yaml，分级规则见
// docs/api-compat.md。
//
// # 它是什么
//
// 把内核的四套存储中立契约（taskboard.Board / sideeffect.Store /
// checkpoint.StoreCAS / eventbus.Store）+ 控制面（controlplane.Store）+
// 下游栅栏（sideeffect.FencedSink）+ 尾随游标（eventbus.CursorStore）
// 落到一个 SQLite(WAL) 单文件库里，全部面**共用一份事务域**。
//
// # 与 pgstore（v0.2.0 自 goagent-kernel 迁入） 的关系
//
// **形态同构、代码不共用**：表结构、事务边界、时钟口径、租户作用域、跨面
// Apply 逐项对位；但判据逻辑两包各写一份——共用会把两个引擎耦在一起。
// 正确性由**同一套契约套件**（boardtest / sidetest / RunSinkContract）对齐。
//
// # 跨面 Apply 与租户作用域
//
//   - **跨面 Apply**（Store.Apply）：各面的变更收进**同一次提交**，失败即
//     作废整批——即使调用方吞掉了内层的错误（内层失败会被记进共享失败槽）
//     ⇒ 跨面不会出现「一半已提交」，也不会出现「什么都没提交却报成功」。
//   - **租户作用域**（NewTenantEffectStore / NewTenantEventStore /
//     NewTenantDownstream / NewTenantTailCursorStore 与门面的 Store.Tenant*
//     入口）：每次读写都从 ctx 取租户（缺 ⇒ tenant.ErrNoContext，fail-closed
//     ——不是回落到默认命名空间）；声明的租户与 ctx 不一致 ⇒
//     ErrTenantMismatch（拒绝猜测哪一份是对的）。
//
// 默认形态（NewEffectStore / NewEventStore / NewDownstream / TailCursors 与
// 门面的 Store.* 入口）**逐字不变**：单租户命名空间，租户取动作/事件自带的值。
//
// # 选型口径
//
// 本包只适合**单机单副本**。多副本 / 跨机一律用 PostgreSQL：SQLite 的单文件
// 锁跨机不可靠。判据由 cmd/storagecheck 机判。
//
// # 驱动名约束（选型红线，issue #12）
//
// 本包 `import _ "modernc.org/sqlite"` 注册的驱动名是 **"sqlite"**。
// `database/sql` 的驱动注册表是**进程级全局态**：同名第二次 `sql.Register`
// 直接 `panic`——发生在 `init`，应用一行业务代码都跑不到，且报错不指明是
// 哪个包引进来的第二个驱动。典型现场：宿主已链接 `github.com/glebarez/go-sqlite`
// 这类同名注册的 fork，再拖入本包 ⇒ `panic: sql: Register called twice for
// driver sqlite`。
//
//   - **全二进制只允许一个驱动注册 "sqlite"**；宿主自带同名驱动时**不得**
//     拖入本包或 `storage/sqlitestore`（后者是本包的绑定面，驱动边在它那里）。
//   - 宿主只要内存面 ⇒ 只 import `storage`（纯判据包，零驱动边）。
//   - 排查：`go list -deps <你的 main 包> | grep -E 'sqlite'` 看第二个驱动是谁引进来的。
//
// # 驱动与版本
//
// 驱动是 modernc.org/sqlite **v1.61.0**（纯 Go，无 cgo ⇒ CGO_ENABLED=0 的
// 交叉构建矩阵不受影响）。锁 v1.61.0 的原因：v1.62.0 需要 libc v1.80.1，
// 而本地 module cache 里没有它（且 goproxy.cn 解析超时）——驱动版本是依赖
// 地板的一部分，抬版必须连带把传递依赖一起对账，不能只看驱动自己。
//
// # 表结构（参考表，gak_ 前缀）
//
//   - gak_board_task      任务认领面（taskboard.Board）
//   - gak_effect_action   Key 级状态（副作用闸 sideeffect.Store）
//   - gak_effect_resource 资源级状态（这个资源谁占着 / 发第几代令牌）
//   - gak_downstream      下游资源（sideeffect.FencedSink 的落点）
//   - gak_snapshot        进度快照（checkpoint.StoreCAS）
//   - gak_invocation      工具调用账本（invocation.Store：调用身份 + 进度状态）
//   - gak_event_watermark 已提交水位线（每租户一行；事件 ID 的铸造点）
//   - gak_event           事件行（与业务状态同事务提交）
//   - gak_tail_cursor     尾随游标（eventbus.CursorStore）
//   - gak_run             运行状态（控制面 controlplane.Store 的落点）
//   - gak_control_version 控制面版本锚（单行）
//   - gak_control_effect  控制面效果裁决账
//
// gak_ 前缀是「参考表」的意思：与 PG 侧的 pgref_* 同款——标清楚它是参考实现，
// 不是生产协议。
//
// # 已知边界
//
//   - 存储时钟是毫秒级（实测）：租约 TTL 建议 ≥ 1s。
//   - DDL 的写锁升级在 SQLITE_BUSY 上不等 busy_timeout：建表只在装配期。
//   - 单写者模型：多写者下退化为串行，busy_timeout 到点即失败（可判、
//     不静默丢写，但降低可用性）。
package sqliteadapter
