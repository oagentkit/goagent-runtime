# goagent-runtime

> 项目站点（GitHub Pages）：https://yi-nology.github.io/goagent-runtime/

面向业务开发者的 **Agent 运行时装配层**，把 [goagent-kernel](https://github.com/yi-nology/goagent-kernel) 的确定性机制（状态/幂等/栅栏/预算/恢复）装配成「一个普通 Go 团队不读几十个包也能用」的运行时。

```
宿主产品            只写领域：系统提示、工具实现、模型供应商适配
goagent-runtime     本层：Loop / Pipeline / 存储 / 事件 的接线与默认口径
goagent-kernel      判据：状态机 / 副作用闸 / 回合守卫 / 检查点
oagentkit / oekit   上游原语
```

**三个模板，按场景选**（能力、前提、证据都写在下文，不做没验证的承诺）：

- `NewEmbedded`：内存存储，单进程零依赖（测试 / 内嵌 / 一次性脚本）；
- `NewStandalone`：SQLite 单文件（WAL），进程重启可恢复（本机服务 / CLI / 边缘单点）；
- `cluster`：PostgreSQL + worker 池，多副本共享同一个 PG——集群内同一会话**有且只有一个副本在执行**，副本崩了由别的副本接管重跑，已施加的工具副作用**不会第二次施加**。

cluster 的前置是 kernel 侧两项 P0 已修复并发布（控制面权威介入原子审计 v0.64.3、租约持续故障失格 v0.64.1）。判据仍全部来自 kernel（`taskboard` 抢占/租约/接管代次、`sideeffect` 幂等闸、`roundguard` 预算、`eventbus` 事件面），本仓只做接线，不另写第二套。

## 为什么是独立仓

- kernel 的门禁核心是「内核包不拖存储驱动」；装配层必须直接装配 SQLite/PG 驱动与事件面，放进来会把那条判据开口子。
- 发版节奏不同：Runtime 的 Beta/RC/1.0 是产品层节奏，与 kernel 的机制版各发各的。

## 模板

| 模板 | 存储 | 适用 |
|---|---|---|
| `NewEmbedded` | 内存 | 单进程、测试、内嵌 |
| `NewStandalone` | SQLite 单文件（WAL） | 本机服务，进程重启可恢复 |
| `cluster` | PostgreSQL（多副本共享） | 多副本 / 高可用 worker 池 |

## cluster：多副本形态（PG + worker 池）

- **形态**：多个副本共享一个 PostgreSQL。任一副本可接单、可执行；同一会话的执行权在集群内**有且只有一个副本**（认领 + 租约 + 接管代次，走 kernel `taskboard` 判据）；副本崩了，任务由别的副本接手；结果经 PG 事件面回传给等待方（跨副本事件面，不另造结果表）。
- **任务模型**：一个任务 = 一次待执行的会话；任务 ID = 会话 ID；同一会话再次投递 = 下一轮（已终态任务重新打开；进行中投递返回 `taskboard.ErrTaken`）。
- **前提**：接管后要「不重复施加副作用」，靠宿主声明**业务幂等键**（`pipeline.Tool.IdempotencyKey`，如订单号这类稳定键）——声明后同一业务动作无论由哪个副本施加都算同一次，闸判 `replayed`。未声明时沿用编排侧铸造的 `toolCallID`，此时跨副本接管这一步依赖模型给出稳定的 `tool_call_id`。
- **证据**：集成用例跑**真 PostgreSQL**（`cluster/pg_cluster_test.go`）：投递 → worker 执行 → 结果经事件面回传；两个独立副本抢同一会话时工具只被施加一次；副本领了任务却不收场（模拟被 SIGKILL）后另一副本接管，已施加的副作用不再施加。CI 的 integration job 提供真 PostgreSQL 跑这批用例；没有库时用例显式 skip，不伪造结果。

## 最小接入

```go
rt, err := runtime.NewEmbedded(runtime.Config{   // standalone: NewStandalone(cfg, "/path/x.db")
    Model: myModel,          // 实现 chat.Model 接口（一个 Chat 方法）
    Tools: []pipeline.Tool{{
        Name: "查库存", Description: "...",
        Handler: func(ctx context.Context, args []byte) (string, error) { ... },
    }},
})
if err != nil { ... }

res, err := rt.Run(ctx, "session-1", "帮我查一下 A 仓库存")
// res.Status / res.Final / res.Rounds / res.ToolCalls / res.Uncertain
```

`chat.Model` 是供应商中立窄接口——任何模型 SDK 包一层适配即可接入，本仓不 import 任何具体供应商。

## 恢复语义（本层的要点）

- 会话历史整体落 checkpoint 快照；进程死在任意两个提交点之间，恢复出来的历史**要么完整要么没有这一段**（回填与提交在同一段顺序代码里）。
- 不完整历史（assistant 请求了工具但结果缺失）= 外部篡改/实现漂移，**fail-closed 报错，不猜**。
- 工具副作用的幂等键 = 编排侧铸造的 `toolCallID`：恢复后模型重放同一调用，副作用闸判 `replayed`——**执行体不会被再次调用**（`agentloop` 与 `runtime` 测试里都有「施加次数仍为 1」的断言）。
- 跨副本接管这类场景要更硬的保证时，宿主可声明业务幂等键 `pipeline.Tool.IdempotencyKey`（如订单号）——键由业务语义决定，不再指望模型复述同一个 ID。
- 结果未知（远端是否生效无法确认）显式上抛为 `res.Uncertain`，**禁止盲目重试**。

## 包

| 包 | 职责 |
|---|---|
| `chat` | 供应商中立的模型窄接口与消息类型 |
| `pipeline` | 工具注册与副作用施加（幂等键/资源/重试安全性映射） |
| `agentloop` | 可恢复回合循环（预算守卫 + 检查点 + 事件） |
| `storage` | 存储装配面（内存判据移植；SQLite/PG 门面转发本仓参考实现） |
| `sqliteadapter` | SQLite（WAL）参考实现：11 面统一门面（自 kernel 迁入，判据套件同源） |
| `pgstore` | PostgreSQL 参考实现：11 面统一门面 + SIGKILL 故障恢复 e2e（自 kernel 迁入） |
| `runtime` | 模板装配（embedded / standalone） |
| `cluster` | 多副本模板装配（PG + worker 池：认领/租约/接管代次、跨副本事件面） |
| `observe` | eventbus → 宿主回调 |

## 判据的归属（不发明第二套）

存储判据全部来自 kernel 契约，本仓**不另写一套**：

- `storage` 的内存、`sqliteadapter`、`pgstore` 三个副作用闸实现都跑 kernel 的 `sidetest` 共享契约套件（33 子测试，含带凭据裁决）；PG 上另有 SIGKILL 故障恢复 e2e（integration CI）；
- 内存快照存取的单调/拒零序号判据移植自 kernel `examples/reference-runtime`（MIT、同作者）；
- 恢复语义由 `agentloop` / `runtime` 的端到端用例钉住；
- cluster 模板的判据同样来自 kernel（`taskboard` 抢占/租约/接管代次、`sideeffect` 幂等闸、`roundguard` 预算、`eventbus` 事件面）——本仓只做接线，证据在真 PG 集成用例里。

## 运行

```bash
go test ./... -count=1     # 全量测试（含 kernel 契约套件在内存与 SQLite 上的两遍）
CGO_ENABLED=0 go run ./examples/embedded-demo    # 中断→恢复，副作用不重放
CGO_ENABLED=0 go run ./examples/standalone-demo  # SQLite 跨进程续跑
GAK_PGREF_DSN=postgres://postgres:postgres@localhost:5432/postgres go test ./cluster -count=1   # cluster 高可用用例（真 PG）
```

## 许可证

MIT（与 goagent-kernel 一致）。
