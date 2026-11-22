# 架构总览

## 一张图看全家桶

<ArchDiagram />

读图要点：

- **箭头向下**：runtime 只依赖 kernel 的**契约**（接口与判据），不依赖任何宿主。
- **箭头向上**：宿主只接触 `runtime.Config`（模型+工具）、`Run` 的结果、和事件 Sink——三个面。
- **storage 的虚线感**：内存/SQLite/PG 三个实现都跑 kernel 的同一套共享测试，判据不分家。

## 一次请求的全链路

以「用户问一句、Agent 调一次工具」为例：

```mermaid
sequenceDiagram
    autonumber
    participant U as 用户代码
    participant LP as agentloop（runtime 装配）
    participant MD as chat.Model（你的适配）
    participant K as kernel 判据（预算守卫+副作用闸）
    participant ST as 存储（checkpoint）
    participant TO as pipeline → 工具 Handler

    U->>LP: Run(ctx, sessionID, "查A仓库存")
    LP->>ST: 加载 checkpoint 快照（会话历史）
    LP->>K: 判本回合预算
    K-->>LP: 额度够 / 按策略收尾
    LP->>MD: Chat(messages, tools)
    MD-->>LP: 请求工具调用（toolCallID 由 loop 铸造）
    LP->>K: 闸判：这个调用施没过？
    K-->>LP: 未施加过 → 放行
    LP->>ST: 提交 checkpoint（历史+调用记录）
    LP->>TO: 施加工具调用 Handler(ctx, args)
    TO-->>LP: 结果
    LP->>ST: 回填结果并提交 checkpoint
    LP->>MD: Chat（带工具结果，模型给出最终答复）
    MD-->>LP: Final 文本
    LP-->>U: Result{Status, Final, ...}
```

中间任何一步进程死掉，恢复时从 **checkpoint** 继续；已提交过的工具调用在副作用闸前被判 `replayed`，**Handler 不会第二次执行**（详见 [中断恢复](/goagent-runtime/advanced/recovery.html)）。

## 依赖关系（谁 import 谁）

```mermaid
flowchart LR
    subgraph 宿主
        host["你的产品"]
    end
    subgraph runtime仓
        chat["chat"]
        pipeline["pipeline"]
        agentloop["agentloop"]
        storage["storage"]
        sq["sqliteadapter"]
        pg["pgstore"]
        runtimep["runtime"]
        observe["observe"]
    end
    subgraph kernel仓
        kernel["goagent-kernel"]
    end
    subgraph 上游
        upstream["oagentkit / oekit"]
        drivers["pgx · modernc/sqlite"]
    end

    host --> runtimep
    runtimep --> agentloop
    runtimep --> storage
    runtimep --> observe
    agentloop --> chat
    agentloop --> pipeline
    agentloop --> kernel
    agentloop --> storage
    pipeline --> kernel
    storage --> kernel
    storage --> sq
    storage --> pg
    sq --> drivers
    pg --> drivers
    chat --> upstream
    observe --> kernel
    kernel --> upstream
```

两条纪律从图上读得出来：

1. **没有环形依赖**——kernel 不认识 runtime，上游不认识任何层。
2. **驱动只在叶子上**——pgx / modernc 只被 sqliteadapter / pgstore 拉进来，主装配面（runtime/agentloop）看不到数据库。

## 事件去哪了

```mermaid
flowchart LR
    LP["agentloop Emitter"] -->|embedded| BUS["eventbus 进程内广播"]
    LP -->|standalone| DB["事件落 SQLite 表"]
    BUS --> OB["observe.Attach"]
    DB --> OB2["轮询读取（standalone）"]
    OB --> SINK["宿主 Sink：日志 / 指标 / 审计"]
    OB2 --> SINK
```

`observe` 是薄装配：订阅并把事件转发给宿主 Sink。不过滤、不采样、不解释语义——那是宿主的事。
