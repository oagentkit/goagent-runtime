# goagent-runtime

> 项目站点（GitHub Pages）：https://yi-nology.github.io/goagent-runtime/

面向业务开发者的 **Agent 运行时装配层**，把 [goagent-kernel](https://github.com/yi-nology/goagent-kernel) 的确定性机制（状态/幂等/栅栏/预算/恢复）装配成「一个普通 Go 团队不读几十个包也能用」的运行时。

```
宿主产品            只写领域：系统提示、工具实现、模型供应商适配
goagent-runtime     本层：Loop / Pipeline / 存储 / 事件 的接线与默认口径
goagent-kernel      判据：状态机 / 副作用闸 / 回合守卫 / 检查点
oagentkit / oekit   上游原语
```

**定位边界（如实声明）**：本仓提供装配层与参考实现，不保证所有机制已组成可直接部署的高可用 Agent Runtime；实际能力以各模板的集成测试证据为准。cluster（PG + worker 池）模板**尚未提供**——等 kernel 侧控制面原子审计与租约失格两项 P0 修复、PG 事件面压测过后再上。

## 为什么是独立仓

- kernel 的门禁核心是「内核包不拖存储驱动」；装配层必须直接装配 SQLite/PG 驱动与事件面，放进来会把那条判据开口子。
- 发版节奏不同：Runtime 的 Beta/RC/1.0 是产品层节奏，与 kernel 的机制版各发各的。

## 模板

| 模板 | 存储 | 适用 |
|---|---|---|
| `NewEmbedded` | 内存 | 单进程、测试、内嵌 |
| `NewStandalone` | SQLite 单文件（WAL） | 本机服务，进程重启可恢复 |
| cluster | — | 未提供（见上方边界声明） |

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
| `observe` | eventbus → 宿主回调 |

## 判据的归属（不发明第二套）

存储判据全部来自 kernel 契约，本仓**不另写一套**：

- `storage` 的内存、`sqliteadapter`、`pgstore` 三个副作用闸实现都跑 kernel 的 `sidetest` 共享契约套件（33 子测试，含带凭据裁决）；PG 上另有 SIGKILL 故障恢复 e2e（integration CI）；
- 内存快照存取的单调/拒零序号判据移植自 kernel `examples/reference-runtime`（MIT、同作者）；
- 恢复语义由 `agentloop` / `runtime` 的端到端用例钉住。

## 运行

```bash
go test ./... -count=1     # 全量测试（含 kernel 契约套件在内存与 SQLite 上的两遍）
CGO_ENABLED=0 go run ./examples/embedded-demo    # 中断→恢复，副作用不重放
CGO_ENABLED=0 go run ./examples/standalone-demo  # SQLite 跨进程续跑
```

## 许可证

MIT（与 goagent-kernel 一致）。
