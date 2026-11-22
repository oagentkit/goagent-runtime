# storage：存储装配面

**一句话**：给上层一个统一的存储面——内存判据在此，SQLite/PG 参考实现通过门面转发。

## 三层的存储关系

```mermaid
flowchart TB
    subgraph 使用方
        LP["agentloop（checkpoint / 历史）"]
        RT["runtime（模板）"]
    end
    subgraph 装配面
        ST["storage：内存实现 + 门面转发"]
    end
    subgraph 参考实现
        SQ["sqliteadapter（SQLite WAL）"]
        PG["pgstore（PostgreSQL）"]
    end
    subgraph 判据来源
        K["kernel 契约：11 面门面 + sidetest 共享测试"]
    end
    LP --> ST
    RT --> ST
    ST --> SQ
    ST --> PG
    SQ --> K
    PG --> K
```

## 内存实现的三条判据

内存副作用闸（`memEffectStore`）的判据移植自 kernel 的参考运行时，钉住三条：

1. **三项联查**：令牌 + 持有者身份 + 资源一致才算数；
2. **执行意图**：Acquire 原子登记，过期未裁决转未知态；
3. **对账占用**：单对账者，过期可重入。

Acquire 判定顺序是契约里最要紧的一行：

```
已完成 > 结果未知 > 对账中 > 执行中 > 资源被占 > 授予
```

**顺序写反的代价是重复施加。** 原子性来自进程内互斥锁——只在单进程内成立，所以才有 SQLite/PG 两个跨进程实现。

## 门面转发的意义

```mermaid
flowchart LR
    U["上层代码"] -->|同一段代码| F["storage 门面"]
    F -->|embedded 模板| MEM["内存"]
    F -->|standalone 模板| SQ["SQLite"]
    F -->|cluster 模板| PG["PG"]
```

换模板不改业务代码——模板切换是存储面的配置，不是上层的改动。
