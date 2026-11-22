# examples/pgstore —— PostgreSQL 参考存储 + 部署案例

`sideeffect.Store` / `sideeffect.FencedSink` / `taskboard.Board`（含
`RenewingBoard`）/ `checkpoint.StoreCAS` / `controlplane.Store` 的
**PostgreSQL 参考实现**，连同把它们的判据在真实数据库上跑通的集成环境。

它是**参考实现，不是内核依赖**（「内核不实现存储」的边界不动）：内核包的
依赖图里没有 pgx；本子包只属于 `examples/`，宿主照抄它、不 import 它。
与 `examples/reference-runtime`（内存版）的分工：内存版证明**判据本身是对的**，
这一份证明**判据在跨进程下依然成立**——用内核的共享契约套件
（`sidetest` / `boardtest` / `sidetest.RunSinkContract`）在真实 PostgreSQL 上
原样跑通，不为任何实现开后门（场景级硬失败）。

## 判据落库形态（宿主抄它的那部分）

| 契约 | 落库形态 |
|---|---|
| `sideeffect.Store.Acquire` | **一个事务**里的三判合一：`SELECT … FOR UPDATE` 行锁（锁序恒为资源行 → 动作行）+ 按契约顺序判定（已完成 > 结果未知 > 执行中 > 资源被占 > 授予）；授予时令牌 = 已见最大值 + 1、持有者身份 `sideeffect.MintHolder()` 同事务铸造、执行意图同事务落库 |
| `Complete` / `Release` / `Abandon` / `Renew` / `MarkUncertain` | 同一事务内**三项联查**（令牌 + 持有者 + 资源一致），校验失败即整体回滚——「标记写失败必须保住执行意图」由此成立 |
| `ResolveUncertain` / `BeginReconcile` | 裁决不校验栅栏（依据是下游事实）；对账占用同刻至多一人，过期可重入 |
| `sideeffect.FencedSink.WriteFenced` | 一条带条件的 `INSERT … ON CONFLICT DO UPDATE … WHERE seen_fence <= presented` 原子预占；写入体经 `ExecWithin` 走**同一事务**，报错即整体回滚（**不得部分写**） |
| `taskboard.Board` | 认领是事务内的读-判-写；令牌随机铸造、接管代次同一次落库；释放/完成/续期条件更新只认令牌 |
| `checkpoint.StoreCAS` | 条件 upsert（`WHERE 已持久化序号 < 本次序号`），`Seq == 0` 先拒 |
| 租户隔离（issue #275） | effect / 资源 / 下游 / 事件表的键均为 `(tenant, 业务键)` 复合主键；租户作用域实例（`NewTenant*`）从 ctx 取租户，缺租户 ⇒ `tenant.ErrNoContext`（写入被拒），声明与 ctx 不一致 ⇒ `ErrTenantMismatch`；事件每个租户一条独立 ID 空间与水位行，订阅侧按流过滤；尾随游标用 `NewTenantTailCursorStore` 按 `(tenant, consumer)` 隔离 |
| `controlplane.Store`（issue #278） | 三个 `Snapshot*` 在 REPEATABLE READ 只读事务里取一致快照；三个 `Commit*` 在 `pgref_control_version` 单行版本锚上「校验 → 留痕 → 写状态 → 推进版本」同事务；任务状态落在 `pgref_board_task`（**与 taskboard 同一张可写表**），运行成员由该表 `run_id` 单源表达。`ControlStore`/`BoardStore` 的 `TaskSourceID` 同 schema 一致，`NewAuthoritativeStorePlane` 装配期判同源 |

## 跑起来（三选一，按场景）

### 1) 部署冒烟：一条命令起环境（默认 profile）

```bash
docker compose -f examples/docker-compose.yml up --abort-on-container-exit --exit-code-from smoke
```

起 `postgres:16` + 参考宿主冒烟（`cmd/pgsmoke`：幂等键重放 / 栅栏拒陈旧 /
双宿主竞争恰好一个 applied / 双 agent 认领恰好一个赢家），退出码 0 即通过。

### 2) 契约验证：同一套共享套件 + SIGKILL e2e（verify profile）

```bash
docker compose -f examples/docker-compose.yml --profile verify up \
  --abort-on-container-exit --exit-code-from contract
```

跑 `go test ./examples/pgstore/` 的全部内容（见下）。

### 3) 本地开发（复用宿主 Go 工具链，最快）

```bash
make integration        # compose 起 postgres → go test → down；compose 起不来会明确报错
# 或指向已有实例：
GAK_PGREF_DSN=postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable \
  go test ./examples/pgstore/ -count=1
```

无 PostgreSQL 时用例在**环境入口**显式跳过（`t.Skipf` 写明原因与起法，
`go test -v` 可见）；套件场景本身不跳——跑不过就是跑不过。

## 测试都判了什么

| 层 | 内容 |
|---|---|
| `TestSideEffectStoreContract` | `sidetest` 33 条：三判合一 / 三项联查 / 执行意图 / 结果未知 / 对账占用（含带凭据裁决与 TTL 校验）/ 动作身份不可变 / 过期租约拒续期 / Unsafe 裁决 / 标记失败……与内存版参考实现跑**同一段套件**（issue #94 的核心验收） |
| `TestTaskBoardContract` | `boardtest` 全家族（含并发认领 10 与 100 两档） |
| `TestDownstreamSinkContract` | `sidetest.RunSinkContract` 6 条：栅栏一族（issue #84 的核心验收，判据收编自 sideeffect 包内用例） |
| `TestSnapshot…` | 快照单调闸（拒陈旧 / 拒零 / 无记录 NotFound / 并发同序号恰好一个赢家） |
| `TestPGE2E*` | **真实进程故障恢复**（issue #95）：子进程执行动作 → `SIGKILL` → 等 TTL 过期 → 断言同一 Key 转需对账**不被重放**；对账「已生效」后只重放结果；双进程竞争恰好一个 `OutcomeApplied`。子进程是 re-exec 本测试二进制（unix build tag），跨进程共享状态只有 PostgreSQL |
| `TestPGE2ETwoProcessSnapshot*` | **跨进程快照单调**（评审第三节场景「两个进程同时提交快照」）：两个真实子进程经同一绝对时刻屏障同时提交——同序号恰好一个写入一个被拒，不同序号落库的必然是较大那个（只进不退），且竞争结束后闸依然立着 |
| `TestEffectStore*Tenant*` / `TestEventStore*Tenant*` / `TestDownstream*Tenant*` | 租户隔离（issue #275）：跨租户同 TaskID 同参数互不重放、同租户不同 Run 互不命中、事件每租户独立 ID 空间与水位线、下游栅栏按 `(tenant, resource)` 隔离、缺租户 fail-closed、旧库 vN+1 迁移幂等 |
| `TestEffectStoreLegacyKeyReplaysAcrossTenants` | **反向验证**（issue #275）：去掉租户维度（不拼键、不分区）后第二个租户必然拿到 `Replayed`——证明修复不是「测了个本来就绿的东西」 |
| `TestControlStore*` | 控制面持久化（issue #278）：快照一致 + 版本锚（旧快照提交 `ErrStoreConflict`，并发只有一个赢家）、缺留痕面/留痕失败状态一字不动、`SnapshotRun`/`CommitRun`/效果账、`CommitTask` 与 `Board.List` **同表**可见、同 schema 同源装配、不同 schema `ErrAuthorityMismatch`；权威 `ForceRelease` 后旧令牌完成被同一张任务板拒收 |

CI 的 `integration-pg` job（postgres service container）每次推送都真跑同一批用例。

## compose 服务清单

| 服务 | profile | 职责 |
|---|---|---|
| `postgres` | 默认 | PostgreSQL 16（healthcheck 门控依赖方） |
| `smoke` | 默认 | 参考宿主冒烟（`cmd/pgsmoke`）——「起环境即验证部署可用」 |
| `contract` | `verify` | 共享契约套件 + SIGKILL e2e 在容器里真跑 |

### Kubernetes（可选 profile，非默认路径）

compose 里的三个角色对应 K8s 对象：`postgres` → StatefulSet（持久卷 +
readinessProbe 对应 healthcheck）、`smoke`/参考宿主 → Deployment（环境变量
`GAK_PGREF_DSN` 指 Service DNS）、`contract` → Job（跑同一批契约用例，作为
发布前的验收闸）。本仓不附带 manifest：无云环境时 compose 已覆盖全部可验证
面，K8s 形态留给宿主按自己的底座装配。

## 新依赖

`github.com/jackc/pgx/v5`（v5.13.0，纯 Go，`CGO_ENABLED=0` 交叉构建不受影响）——
**只进本子包的依赖图**，内核各包 `requires` 均不含它（policy 的
`dependency_direction.ext_prefixes` 登记了 `github.com/jackc`，内核包若 import
驱动会被依赖方向门禁当场拦下）。
