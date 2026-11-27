---
title: 决策记录（ADR）
---

# 决策记录（ADR）

::: warning 本页由 `site/scripts/gen_reference_pages.mjs` 从仓内 `docs/adr/` 生成，勿手改
决策记录是**设计推导与取舍**的真相源；README 只留结论，推导一律在仓内 ADR。
:::

| 编号 | 决策 | 一句话结论 | 状态 |
|---|---|---|---|
| [ADR-0001](/goagent-runtime/adr/ADR-0001-独立成仓.html) | 独立成仓 | 装配层独立成仓——内核的门禁核心是「内核包不拖存储驱动」，把驱动装进内核就是给那条判据开口子。 | 已采纳（Accepted） |
| [ADR-0002](/goagent-runtime/adr/ADR-0002-判据不另写一套.html) | 判据不另写一套 | 判据归 kernel，本仓只接线——装配层最容易腐化成「顺手把判据也实现一遍」的第二个内核。 | 已采纳（Accepted） |
| [ADR-0003](/goagent-runtime/adr/ADR-0003-模型接缝只有-chat-Model.html) | 模型接缝只有 chat.Model | 模型接缝只有 `chat.Model` 一个窄接口——供应商中立是接缝纪律，不是偏好。 | 已采纳（Accepted） |
| [ADR-0004](/goagent-runtime/adr/ADR-0004-恢复不完整历史要报错不猜.html) | 恢复不完整历史要报错不猜 | 历史不完整是异常不是状态——恢复出来的历史要么完整要么没有这一段，中间态 fail-closed 报错，不猜。 | 已采纳（Accepted） |
| [ADR-0005](/goagent-runtime/adr/ADR-0005-cluster-不重复施加靠业务幂等键.html) | cluster 的不重复施加靠业务幂等键 | 「接管不重复施加副作用」的前提是宿主声明业务幂等键——这是结构性保证，不靠模型复述同一个 ID。 | 已采纳（Accepted） |
| [ADR-0006](/goagent-runtime/adr/ADR-0006-容器后端是参考实现.html) | 容器后端是参考实现 | 容器后端是参考实现——宿主抄它不 import 它，部署形态不焊进依赖面。 | 已采纳（Accepted） |
| [ADR-0007](/goagent-runtime/adr/ADR-0007-驱动绑定归绑定包.html) | 驱动绑定归绑定包 | 驱动绑定归绑定包——判据面（`storage`）零驱动边，绑定面（`storage/sqlitestore`）才带驱动，宿主按面 import。 | 已采纳（Accepted） |
| [ADR-0008](/goagent-runtime/adr/ADR-0008-事件与快照同生共死.html) | 事件与快照同生共死 | 事件与快照同生共死——两脸同库时单事务联合提交，孤儿事件与终态丢失两个窗口结构闭合。 | 已采纳（Accepted） |
| [ADR-0009](/goagent-runtime/adr/ADR-0009-流式接缝走-push-回调加框架归并器.html) | 流式接缝走 push 回调 + 框架归并器 | 流式接缝走 push 回调 + 框架归并器——宿主只产片段，归并规则归框架一份，增量是观测面不进状态面。 | 已采纳（Accepted） |
| [ADR-0010](/goagent-runtime/adr/ADR-0010-执行接口两模型分立共享语义不成模型.html) | 执行接口两模型分立，共享语义不成模型 | 执行接口两模型分立——Loop 与 Pipeline 不压成一个执行模型，共享的是 Run/Step/Checkpoint/Event/Cancellation 语义与读面；流程力学组合 `oekit/workflow`、拓扑守卫组合 `agent/dispatch`，本仓不自造引擎。 | 已采纳（Accepted） |
| [ADR-0011](/goagent-runtime/adr/ADR-0011-事件命名法与-trace-身份面裁决.html) | 事件命名法与 Trace 身份面裁决 | 事件命名法 snake_case 为法、`agent.*` 冻结不扩张、run 身份走载荷键不新造事件族、worker 身份留在任务板——trace 投影是词表的唯一合成点，不建第三份词表。 | 已采纳（Accepted） |

> 仓内索引：https://github.com/yi-nology/goagent-runtime/blob/main/docs/adr/README.md（新增 ADR 的流程写在那里）。
