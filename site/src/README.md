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
  - title: 多副本 cluster
    details: 多个副本共享一个 PostgreSQL：同一会话集群内只有一个副本执行，副本崩溃后别的副本接管重跑，已施加的副作用不会第二次施加。
---

<div class="home-punch"><strong>「装配层要做的事，宿主一件都不用做。」</strong>状态机 · 幂等 · 预算 · 恢复，全在 runtime 里接好。</div>

<div class="home-install"><span class="prompt">▸</span>go get github.com/yi-nology/goagent-runtime@latest</div>

<div class="home-links">
  <a class="chip" href="/goagent-runtime/guide/quickstart.html">五分钟上手</a>
  <a class="chip" href="/goagent-runtime/guide/architecture.html">架构总览</a>
  <a class="chip" href="/goagent-runtime/guide/concepts.html">核心概念</a>
  <a class="chip" href="/goagent-runtime/packages/">包导览</a>
  <a class="chip" href="/goagent-runtime/templates/">模板选型</a>
  <a class="chip" href="/goagent-runtime/advanced/recovery.html">崩溃恢复</a>
  <a class="chip chip-ai" href="https://yi-nology.github.io/goagent-kernel/llms.txt">AI 直读 llms.txt</a>
  <a class="chip" href="https://github.com/yi-nology/goagent-runtime">GitHub &#8599;</a>
</div>

## 它处在哪一层

<ArchDiagram />

一句话：**kernel 只管「怎么判定对错」，runtime 负责「把零件装成一辆车」**。你坐在驾驶座上，不用读几十个包。

## 三个模板，按场景选

| 模板 | 存储 | 适用 |
|---|---|---|
| `NewEmbedded` | 内存 | 单进程、测试、内嵌 |
| `NewStandalone` | SQLite 单文件（WAL） | 本机服务，进程重启可恢复 |
| `cluster` | PostgreSQL（多副本共享） | 多副本 / 高可用 worker 池 |

cluster 的能力、前提与证据见 [模板选型](/goagent-runtime/templates/)。
