---
title: 坑速查
---

# 坑速查：症状 → 真因 → 处置

::: warning 本页由 `site/scripts/gen_reference_pages.mjs` 从 `site/data/pitfalls.json` 生成，勿手改
坑表的唯一真相是 `site/data/pitfalls.json`；数据文件里的 kernel 地板版本（`v0.66.1`）与 go.mod 对账，对不上即判红。
:::

本仓踩过的坑与它的前提都在这儿。接完线拿它自查一遍。

## 使用者：跑起来才会遇到

| 症状 | 真因 | 处置 |
|---|---|---|
| 副本接管后，同一个工具被执行了两次 | 未声明业务幂等键。跨副本接管重跑时，幂等闸只能沿用编排侧铸造的 toolCallID —— 这一步要靠模型给出稳定的 tool_call_id | 给工具声明 pipeline.Tool.IdempotencyKey（订单号这类业务稳定键）；声明后同一业务动作无论由哪个副本施加都算同一次，闸判 replayed |
| 投递任务返回「已被认领」类错误，任务没跑 | 同一会话在集群内有且只有一个副本执行；重复投递命中 taskboard.ErrTaken | 把它当「已在执行」分支处理（等待事件面回传结果），不要重投；重投只会继续命中同一个租约 |
| 进程重启恢复后直接报错，说历史不完整 | 检查点缺失（外部篡改或实现漂移），不是可猜的状态 | fail-closed 是设计，别绕：修数据源或实现，不要手改快照 |
| 结果未知时重试，副作用被重复施加 | 忽略了 res.Uncertain —— 远端是否生效无法确认 | 显式处理 Uncertain，禁止盲目重试；业务侧已施加成功与否要由宿主确认 |
| 以为 SQLite 需要 gcc，交叉编译报 CGO 错误 | 以为 SQLite 走的是需要 CGO 的驱动 | 本仓用 modernc.org/sqlite（纯 Go），CGO_ENABLED=0 即可编；依赖事实见「参考 › 机器事实」 |
| 本地跑 PG / cluster 用例全部 skip | 没有 GAK_PGREF_DSN 环境变量 | 起一个本地 PostgreSQL 并设置该环境变量；没有库时用例显式 skip，不伪造结果 |
| 换一个模型供应商，要改 runtime 源码 | 以为模型调用是绑定在某家 SDK 上的 | chat.Model 是供应商中立窄接口（只有一个 Chat 方法），在宿主包一层适配即可；本仓不 import 任何具体供应商 |
| 在本仓找不到状态机 / 幂等 / 预算的实现 | 判据不在本仓——状态机、幂等闸、预算守卫、恢复、抢占接管全在 goagent-kernel | 判据的归属是本仓的第一条纪律：要改判据去 kernel，要接线在本仓 |

## 贡献者：改本仓时才会遇到

| 症状 | 真因 | 处置 |
|---|---|---|
| 站点部署后页面里的链接全 404 | 站点部署在子路径（base=/goagent-runtime/），而 markdown 正文链接是原生 &lt;a href&gt;，框架不加工 base | 正文链接必须自带 base 前缀；导航 / 侧栏 / hero 按钮走路由链接（纯路由路径、不带 base）。判据是 site/scripts/check-links.mjs，红了就是有链接 404 |
| 想在本仓实现一套新的存储判据 | 判据的本体归 kernel，本仓只做接线 | 新存储实现必须跑 kernel 的 sidetest 共享契约套件；不要在本仓重写一套「类似」的判据 |
| 改了仓内事实，站点页面却还是旧的 | 站点页面是生成物（由 site/scripts/gen_reference_pages.mjs 从仓库事实生成） | 跑 npm run gen:reference 重新生成；CI 的 --check 会判红，直接手改生成物也会被红 |
| 本地跑着好好的，CI 红了但看不出哪条规则拦的 | 门禁有多条（单测 / 集成 / 站点链接 / 生成物同步），红的是其中一条 | 看 CI 作业名与步骤名定位；集成作业需要真 PostgreSQL（services 里已声明） |
