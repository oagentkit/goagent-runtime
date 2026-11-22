# runtime：模板装配

**一句话**：把上面所有零件装进两个工厂函数，宿主三行代码开跑；多副本形态在 `cluster` 包（PG + worker 池）。

## 装配图

```mermaid
flowchart TB
    CFG["runtime.Config：Model + Tools + 可选口径"] --> RT["runtime 模板"]
    RT -->|NewEmbedded| E["内存存储 + 进程内 eventbus"]
    RT -->|NewStandalone| S["SQLite 存储 + 事件落表"]
    RT -->|cluster.New| CL["PG 存储 + worker 池 + 跨副本事件面"]
    E --> LP["agentloop"]
    S --> LP
    LP --> OB["observe → 宿主 Sink"]
```

## 最小代码

```go
rt, err := runtime.NewEmbedded(runtime.Config{
    Model: myModel,
    Tools: []pipeline.Tool{{Name: "查库存", Description: "...", Handler: ...}},
})
res, err := rt.Run(ctx, "session-1", "帮我查一下 A 仓库存")
```

`Run` 返回的 `Result`：

| 字段 | 含义 |
|---|---|
| `Status` | 成功 / 预算收尾 / 拒绝 / 出错 |
| `Final` | 模型的最终答复文本 |
| `Rounds` | 跑了几个回合 |
| `ToolCalls` | 发生了哪些工具调用 |
| `Uncertain` | 是否有副作用结果无法确认（见 [中断恢复](/advanced/recovery.html)） |

## 边界（如实声明）

- `NewEmbedded`：内存存储，单进程，零外部依赖——测试/内嵌场景。**进程退出即丢失**。
- `NewStandalone`：SQLite 单文件持久化，本机服务场景。
- **cluster（PG + worker 池）**：多副本形态——多个副本共享一个 PostgreSQL；同一会话集群内只有一个副本执行（认领 + 租约 + 接管代次）；副本崩了别的副本接管重跑，已施加的副作用不重复施加。前提是宿主声明业务幂等键（未声明时依赖模型给出稳定的 `tool_call_id`），证据是真 PostgreSQL 集成用例（多副本抢同一会话、接管不重放，见 [模板选型](/templates/)）。
