# goagent-runtime

Agent 运行时装配层 —— oagentkit 模块矩阵 **tier 5**（装配层，只接线不另写判据）。

本仓依赖的矩阵内仓：`agent` `goagent-kernel` `guardx`。

> 项目站点（GitHub Pages）：https://oagentkit.github.io/goagent-runtime/

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

## 版本与发布

- **发布说明**：GitHub Releases（每个版本的第一段是「下游要做什么」）与仓内 [CHANGELOG.md](CHANGELOG.md)——后者由家族工具 `matrix-tools/scripts/gen_changelogs.py` 从 git 历史生成，**勿手改**；站点的「版本记录」页是它的投影。
- **版本指针**：当前最新版本见 [Releases 页](https://github.com/yi-nology/goagent-runtime/releases)。本 README **不硬写版本号**——版本号的事实源是 tag 与 CHANGELOG，抄第二份只会漂。
- **一条纪律**：`go get` 拿到的模块里，CHANGELOG 必须含该 tag 自己那一段——tag 打在 CHANGELOG 生成之前就会缺段（`v0.6.3` 就是这么缺了一段，其修复随 `v0.6.4` 出货）。CI 的「CHANGELOG 同步校验」作业就是这条纪律的执行体。
## 为什么是独立仓

- kernel 的门禁核心是「内核包不拖存储驱动」；装配层必须直接装配 SQLite/PG 驱动与事件面，放进来会把那条判据开口子。
- 发版节奏不同：Runtime 的 Beta/RC/1.0 是产品层节奏，与 kernel 的机制版各发各的。

## 模板

| 模板 | 存储 | 适用 |
|---|---|---|
| `NewEmbedded` | 内存 | 单进程、测试、内嵌 |
| `NewStandalone` | SQLite 单文件（WAL） | 本机服务，进程重启可恢复 |
| `cluster` | PostgreSQL（多副本共享） | 多副本 / 高可用 worker 池 |

## SQLite 驱动名约束（选型红线）

`sqliteadapter` 用 `modernc.org/sqlite` 注册驱动名 **`"sqlite"`**。`database/sql` 的驱动注册表是**进程级全局态**：同名第二次 `sql.Register` 直接 `panic`——发生在 `init`，应用一行业务代码都跑不到，且报错不指明是哪个包引进来的第二个驱动。

- **全二进制只允许一个驱动注册 `"sqlite"`**。宿主若已链接别的同名驱动（如 `github.com/glebarez/go-sqlite`，modernc 的 fork），就**不得再拖入** `sqliteadapter` / `storage/sqlitestore`。
- 宿主自带 SQLite 驱动时的出路：① 只需要内存面 → 只 import `storage`（**零驱动边**，见「## 包」表）；② 要真 SQLite → 用宿主自己的驱动实现 kernel 的 `checkpoint.StoreCAS` 契约（判据归 kernel）。
- 排查线索：`panic: sql: Register called twice for driver sqlite` 即此冲突；`go list -deps <你的 main 包> | grep -E 'sqlite'` 看第二个驱动是谁引进来的。
- 本仓侧由 `make deps` 守着：纯判据包（`storage`）不得链数据库驱动，判据跑不起来即判红。
- 判据面与绑定面为什么这样分（以及拆分带来的破坏性变更），推导见 [ADR-0007](docs/adr/ADR-0007-驱动绑定归绑定包.md)。

## cluster：多副本形态（PG + worker 池）

- **形态**：多个副本共享一个 PostgreSQL。任一副本可接单、可执行；同一会话的执行权在集群内**有且只有一个副本**（认领 + 租约 + 接管代次，走 kernel `taskboard` 判据）；副本崩了，任务由别的副本接手；结果经 PG 事件面回传给等待方（跨副本事件面，不另造结果表）。
- **任务模型**：一个任务 = 一次待执行的会话；任务 ID = 会话 ID；同一会话再次投递 = 下一轮（已终态任务重新打开；进行中投递返回 `taskboard.ErrTaken`）。
- **前提**：接管后要「不重复施加副作用」，靠宿主声明**业务幂等键**（`pipeline.Tool.IdempotencyKey`，如订单号这类稳定键）——声明后同一业务动作无论由哪个副本施加都算同一次，闸判 `replayed`。未声明时沿用编排侧铸造的 `toolCallID`，此时跨副本接管这一步依赖模型给出稳定的 `tool_call_id`。
- **证据**：集成用例跑**真 PostgreSQL**（`cluster/pg_cluster_test.go`）：投递 → worker 执行 → 结果经事件面回传；两个独立副本抢同一会话时工具只被施加一次；副本领了任务却不收场（模拟被 SIGKILL）后另一副本接管，已施加的副作用不再施加。CI 的 integration job 提供真 PostgreSQL 跑这批用例；没有库时用例显式 skip，不伪造结果。
- **失败模式矩阵**（`cluster/pg_cluster_failure_test.go`，真 PG，runtime#40）：长任务租约续期（执行 > 2×TTL 期间竞争认领恒被拒）；双活窗口（旧持有者带过期令牌回来收场被拒、其结论不得对外可见）；工具结果未知（`Uncertain` 经事件面回传等待方）；取消 vs 在途（封口有界返回、任务不悬空、接管后成功施加恰一次）；优雅关闭排空与超时回落。
- **收尾语义**：`Close` = 立即取消在途（封口）→ 在途任务留待租约到期后被接管；`CloseGraceful(ctx)` = 停止认领新任务、在途**自然收场**后再关池，ctx 到期回落取消语义并**如实返回错误**（没排干净不许静默成成功）。两者幂等、可混用。已知边界：取消路径上 Safe 失败的副作用闸回收要等 `SideEffectTTL`（默认 30s，可配）才回到可认领——根治在 kernel 侧（清理 ctx 必须 cancel-proof），安全不变量（不重复施加）不破。

- **为何不整段换 kernel/worker.Loop（runtime#24 裁定，2026-11-26）**：Loop 同样走 List 全量（治不了列表量随任务数单调涨的 O(n)），且 `NewLoop` 强制要 Registry——cluster 架构里 worker 存活只靠租约过期推断，多副本 PG Registry 不存在，换 Loop 是语义扩容不是重构。家族先例（kernel#507）已裁「同形不抽、各自成文」，第三套池沿同一口径。已落地的改进：**单次 Claim 形态**（claimNext 筛出首个可领候选后只 Claim 一次，消掉竞争下的认领风暴，对齐 kernel/worker.Loop）；O(n) List 的根治是 pgstore 实现 `taskboard.ReadyBoard.ClaimReady`（带就绪索引的单查询），另行排期。

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
defer rt.Close()

session, err := rt.NewSession(ctx, runtime.SessionOptions{ID: "session-1"})
if err != nil { ... }

res, err := session.Run(ctx, "帮我查一下 A 仓库存")
// res.Status / res.Final / res.Rounds / res.ToolCalls / res.Uncertain
```

- `sessionID` 是恢复锚点：同一 ID 的多次 `Run` 共享历史，进程重启后同 ID 续跑即从快照恢复。
- `SessionOptions` 支持会话级覆盖（`System` / `Budget` / `Tools` / `Observer`，nil = 继承 Runtime 级，**只作用于本会话**）。
- 兼容入口 `rt.Run(ctx, sessionID, user)` 仍在（薄委托到 Session），老调用方零改动。

`chat.Model` 是供应商中立窄接口——任何模型 SDK 包一层适配即可接入，本仓不 import 任何具体供应商。

## 流式模型（保 SSE 逐片体验）

`chat.Model` 仍是**单方法非流式契约**（`Chat(ctx, ChatRequest) (ChatResponse, error)`）——老宿主零改动。但保流式的宿主不必再各写一座「Stream 聚合桥」：

- **承载面**：`chat.Delta` 带 kind + text，可选带 `ToolCall`（`Index` 分组键 + 参数片段）与 `Usage`（token 计量）。
- **归并器**：`chat.DeltaAggregator` 按 `Index` 归并工具调用、累计 usage——**归并规则在框架侧一份实现**（宿主不再各写各的）。宿主侧只剩「把 SDK 的 chunk 搬成片段」与 EOF/错误上抛。
- **旁路**：`ChatRequest.OnDelta` 是可选回调，模型不调用即回落非流式（nil 语义同缺省）。
- **事件面**：逐片出 `agent.content_delta`。联合提交形态（standalone / cluster）下**只即时广播、不落事件行**——增量是观测面不是状态面，落库会污染状态面并打破「一轮四事件」那条判据。
- **背压**：广播非阻塞（订阅方缓冲满则丢弃并计数）⇒ 宿主 SSE 写慢不反压模型流。

形态取舍与推导见 [ADR-0009](docs/adr/ADR-0009-流式接缝走-push-回调加框架归并器.md)。

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
| `adjudicate` | 副作用未知态的周期裁决（证据回查 → 三态判定 → 落裁决；#1） |
| `storage` | 存储判据面（Provider 契约 + 内存实现；**零驱动边**——纯判据包不带数据库驱动，见 #10） |
| `storage/sqlitestore` | standalone 形态的 SQLite Provider 绑定（**驱动边只在这一包**：modernc 驱动不进 `storage` 纯判据包） |
| `sqliteadapter` | SQLite（WAL）参考实现：11 面统一门面（自 kernel 迁入，判据套件同源） |
| `pgstore` | PostgreSQL 参考实现：11 面统一门面 + SIGKILL 故障恢复 e2e（自 kernel 迁入） |
| `runtime` | 模板装配（embedded / standalone） |
| `preset` | 五形态标准装配（ADR-004 (a) 路径：Simple/ReAct/Workflow 步骤/MultiAgent/Longrunning——真装配非转发，与 `examples/` 五形态样例互为锚） |
| `cluster` | 多副本模板装配（PG + worker 池：认领/租约/接管代次、跨副本事件面） |
| `observe` | eventbus → 宿主回调（薄转发，不 Interpret 语义） |
| `trace` | Trace 读模型：事件流 → Session/Run/Turn/ToolCall 层级投影 + 运营八问查询（#42） |
| `containerbackend` | 容器执行后端参考实现（Docker / K8s Job）——**宿主抄它，不 import 它** |
| `bridge/llmchat` | 家族钦定适配器：`oagentkit/llm`（eino 栈）→ `chat.Model`（**嵌套模块**——LLM SDK 边只在 bridge 内，主 module 零 LLM SDK import 的声明面不变） |
| `bridge/einotool` | 家族钦定适配器：eino `tool.InvokableTool` → `pipeline.Tool`（副作用闸字段不猜，宿主按业务补） |
| `agentiface` | 三入口 → `oagentkit/agent/iface` 窄接口适配器（agentloop / contract 边界两形态；agent#174——接口本体在 Agent 层，只有装配层能同时看见三套语义） |

## 判据的归属（不发明第二套）

存储判据全部来自 kernel 契约，本仓**不另写一套**：

- `storage` 的内存、`sqliteadapter`、`pgstore` 三个副作用闸实现都跑 kernel 的 `sidetest` 共享契约套件（33 子测试，含带凭据裁决）；PG 上另有 SIGKILL 故障恢复 e2e（integration CI）；
- 内存快照存取的单调/拒零序号判据移植自 kernel `examples/reference-runtime`（MIT、同作者）；
- 恢复语义由 `agentloop` / `runtime` 的端到端用例钉住；
- cluster 模板的判据同样来自 kernel（`taskboard` 抢占/租约/接管代次、`sideeffect` 幂等闸、`roundguard` 预算、`eventbus` 事件面）——本仓只做接线，证据在真 PG 集成用例里；
- `roundguard` 自 v0.77.0 起上抛 `github.com/oagentkit/agent/roundguard`（归属评测 2026-11-26：单 agent 循环判据，与编排无关）。

## 执行模型（Loop 与 Pipeline 分立）

两种执行模型**分立**，边界判据是**谁决定下一步**：Loop 的下一步由模型定（自主决策），Pipeline（若立项）的下一步由依赖图定（确定性流程）——不压成一个执行模型。共享的是 Run / Step / Checkpoint / Event / Cancellation 语义与读面，不是控制流抽象；判据仍全部来自 kernel。家族组合规则：定步流程力学组合 `oekit/workflow`、Multi-Agent 拓扑守卫组合 `oagentkit/agent/dispatch`，本仓不自造流程引擎与拓扑守卫；工具副作用换执行模型不换闸。Pipeline 执行器条件立项（有消费方才建）。推导与拒收面见 [ADR-0010](docs/adr/ADR-0010-执行接口两模型分立共享语义不成模型.md)。

## 运行时可观测（Trace）

事件是真相，`trace` 包只做投影（不落库、不改事件面）：`trace.NewCollector()` 挂到任意 Observer 注入面（`runtime.Config.Observer` / `SessionOptions.Observer` / `cluster.Config.Observer`），得到 Session → Run → Turn → ToolCall 层级，并回答运营八问——多少次 Run、多少 Turn、最慢工具调用、哪次模型调用失败（轮次+原因）、哪个 Worker 接管（走 `cluster.Board()`，任务板判据面）、为什么暂停、为什么 failed、Token 聚合。`run_id` 由 agentloop 每次执行铸造并注进每个事件 payload（`Result.RunID` 同源可对账）；计量走 `chat.ChatResponse.Usage` 随轮事件出面。包文档有八问 → 查询的逐条映射与用例（`trace/trace_test.go`）。事件命名法与词表单源的裁决（snake_case 为法 / `agent.*` 冻结 / run 身份走载荷键 / worker 身份留任务板）见 [ADR-0011](docs/adr/ADR-0011-事件命名法与-trace-身份面裁决.md)。

## 模型与工具的接缝（钦定适配器）

宿主有两条接入路径：**直接实现** `chat.Model` / `pipeline.Tool`（供应商中立，本仓不 import 任何 SDK——ADR-0001/0003），或**经 bridge 嵌套模块**复用 eino 生态（llm 降级链/预算、既有 eino 工具一族接入）。bridge 在独立 go.mod 里，不用它的宿主依赖图里没有 llm/eino。

## 运行

```bash
go test ./... -count=1     # 全量测试（含 kernel 契约套件在内存与 SQLite 上的两遍）
CGO_ENABLED=0 go run ./examples/embedded-demo    # 中断→恢复，副作用不重放
CGO_ENABLED=0 go run ./examples/standalone-demo  # SQLite 跨进程续跑
GAK_PGREF_DSN=postgres://postgres:postgres@localhost:5432/postgres go test ./cluster -count=1   # cluster 高可用用例（真 PG）
```

## 许可证

MIT（与 goagent-kernel 一致）。
