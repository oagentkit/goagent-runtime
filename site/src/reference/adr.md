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

> 仓内索引：https://github.com/yi-nology/goagent-runtime/blob/main/docs/adr/README.md（新增 ADR 的流程写在那里）。
