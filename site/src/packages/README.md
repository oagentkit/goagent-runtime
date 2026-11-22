# 包导览

八个包一条主线：**runtime 装配 → agentloop 循环 → chat 问模型 / pipeline 调工具 / storage 落存储 / observe 发事件**。

```mermaid
flowchart LR
    subgraph 装配面
        RT["runtime：模板 NewEmbedded / NewStandalone"]
    end
    subgraph 循环面
        LP["agentloop：可恢复回合循环"]
    end
    subgraph 接缝面
        CH["chat：模型窄接口"]
        PL["pipeline：工具施加管线"]
        OB["observe：事件 → 宿主 Sink"]
    end
    subgraph 存储面
        ST["storage：装配面 + 内存闸"]
        SQ["sqliteadapter：SQLite 参考实现"]
        PG["pgstore：PG 参考实现"]
    end
    RT --> LP
    LP <--> CH
    LP --> PL
    LP --> ST
    LP --> OB
    ST --> SQ
    ST --> PG
```

| 包 | 一句话职责 | 详细 |
|---|---|---|
| `chat` | 供应商中立的模型窄接口与消息类型 | [展开](/packages/chat.html) |
| `pipeline` | 工具注册与副作用施加（幂等键/资源/重试安全性映射） | [展开](/packages/pipeline.html) |
| `agentloop` | 可恢复回合循环（预算守卫 + 检查点 + 事件） | [展开](/packages/agentloop.html) |
| `storage` | 存储装配面（内存判据 + SQLite/PG 门面转发） | [展开](/packages/storage.html) |
| `sqliteadapter` / `pgstore` | 两个参考实现，11 面统一门面，判据套件同源 | [展开](/packages/sqlite-pg.html) |
| `runtime` | 模板装配（embedded / standalone） | [展开](/packages/runtime.html) |
| `observe` | eventbus → 宿主回调 | [展开](/packages/observe.html) |

## 依赖速查

```mermaid
flowchart TB
    K["goagent-kernel（状态机/幂等闸/预算/事件判据）"]
    RT["runtime"] --> AL["agentloop"]
    RT --> ST["storage"]
    RT --> OB["observe"]
    AL --> CH["chat"]
    AL --> PL["pipeline"]
    AL --> ST
    AL --> K
    PL --> K
    ST --> K
    ST --> SQ["sqliteadapter"] --> D1["modernc.org/sqlite"]
    ST --> PG["pgstore"] --> D2["pgx/v5"]
    OB --> K
```

- kernel 不认识 runtime（单向）；驱动只在叶子（pgx / modernc 不进入装配面）。
