---
home: true
title: 首页
heroText: goagent-runtime
tagline: goagent-kernel 的运行时装配层 —— 把确定性机制装配成普通 Go 团队直接能用的 Agent Runtime
actions:
  - text: 五分钟上手
    link: /guide/quickstart.html
    type: primary
  - text: 架构总览
    link: /guide/architecture.html
    type: secondary
  - text: GitHub
    link: https://github.com/yi-nology/goagent-runtime
    type: secondary
features:
  - title: 只写领域代码
    details: 系统提示、工具实现、模型供应商适配是你要写的全部；状态机、幂等闸、预算、恢复都由装配层接好。
  - title: 中断不丢、不重放
    details: 每个回合落检查点快照；进程死在任意时刻，恢复后要么完整要么没有这一段，工具副作用不会被第二次执行。
  - title: 供应商中立
    details: chat.Model 只有一个 Chat 方法，任何模型 SDK 包一层适配即可接入，本仓不 import 任何具体供应商。
  - title: 判据不另写一套
    details: 存储判据全部来自 kernel 契约：内存 / SQLite / PG 三个实现都跑同一套共享测试（33 个子测试）。
---

## 它处在哪一层

```mermaid
flowchart TB
    subgraph L1["① 宿主产品（你写的）"]
        A["系统提示词 · 工具实现 · 模型供应商适配"]
    end
    subgraph L2["② goagent-runtime（本仓）"]
        B["Loop / Pipeline / 存储 / 事件的接线与默认口径"]
    end
    subgraph L3["③ goagent-kernel（判据）"]
        C["状态机 · 副作用闸 · 回合守卫 · 检查点"]
    end
    subgraph L4["④ oagentkit / oekit（上游原语）"]
        D["工具治理 · 事件广播 · 并发与日志 · 基础库"]
    end
    L1 --> L2 --> L3 --> L4
```

一句话：**kernel 只管「怎么判定对错」，runtime 负责「把零件装成一辆车」**。你坐在驾驶座上，不用读几十个包。

## 三个模板，按场景选

| 模板 | 存储 | 适用 |
|---|---|---|
| `NewEmbedded` | 内存 | 单进程、测试、内嵌 |
| `NewStandalone` | SQLite 单文件（WAL） | 本机服务，进程重启可恢复 |
| cluster | — | 尚未提供（PG + worker 池，等 kernel 侧压测验证后上） |
