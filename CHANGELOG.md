# goagent-runtime CHANGELOG

> 本文件由 `matrix-tools/scripts/gen_changelogs.py` 从 git 历史生成，按 tag 切段、取每段内的 conventional commit 渲染。

## [v0.4.2] — 2026-11-23

- 格式(site)：架构图按 goagent-kernel 站视觉口径重绘（分层卡片 + 单向箭头 + 图注）

原图（手绘 SVG 组件）与 kernel 站不是同一套视觉语言，且窄视口会被裁：
- 半透明彩色层带 + 辉光滤镜 + 绿/蓝/紫/灰四色，与 kernel 站的「深色 GitHub 风」
  （扁平方卡 / 1.5px 细箭头 / #0d1117 三层底）分属两套语言——同一矩阵的两站图长得不一样。
- 层带用 fill-opacity 0.055，文字压在半透明色块上，暗色下对比度不稳。
- 连线是长贝塞尔曲线（observe → 事件 Sink 横穿整张图），读图时抓不住走向。
- 右侧内容在窄视口被裁，且没有图注说明怎么读。
改为 kernel 站同款语法：
- 层带改「左标签列 + 卡片网格」：宿主 / runtime / kernel / 上游四层，层名带色
  （runtime 蓝 #58a6ff、kernel 紫 #a371f7、宿主灰、上游更淡）。
- 卡片扁平化（rx=10、1.2px 描边），agentloop 用蓝描边点出核心；宿主侧四张卡
  用更外一层的底色（#1c2330），与 kernel 站「外框层级」一致。
- 箭头只留三条层间流向 + 一条「模板装配 → agentloop」，全部 1.5px 细线 + 三角箭头；
  每条层间流向带一行说明（三个面 / 只依赖契约面 / 上游只给原语）。
- 补图注（居中 12.5px）。暗色逐值对齐 kernel 取色板，亮色给同构浅色一套——
  runtime 站跑的是 VuePress 默认主题，它的 --vp-c-* 是默认调色板（绿品牌色），
  与 kernel 那套深色不是同一份真相，直接引用会把图染成绿色。
同步改 architecture.md 的「读图要点」：原文描述的是旧图「箭头向上/向下」两种语义，
新图只有向下一种方向 ⇒ 文字必须跟着改，否则文档与图对不上。
零行为变化：仅站点组件与站点文案，go 代码未动。


## [v0.4.1] — 2026-11-23

### 修复(site)：正文链接补 base 前缀（子路径部署下的 404）+ 内部链接门禁

实录：站点首页「接下来」三条链接（五分钟跑起来 / 架构总览 / 核心概念）点开全是 404。
根因：站点部署在**子路径**（base = '/goagent-runtime/'），而 markdown 里的链接被
markdown-it 输出成**原生 <a href>** —— 框架不加工它 ⇒ 写 '/guide/quickstart.html'
时浏览器解析成 https://yi-nology.github.io/guide/quickstart.html（少一层前缀）⇒ 404。
导航 / 侧栏 / hero 按钮却正常：它们是**路由链接**，框架运行时会给它们加 base。
⇒ 两类链接语义不同，不能同判。
修法：
- 17 处正文链接补 base 前缀（guide 5 处 / packages 10 处 / templates 1 处 / …）。
- 新增 site/scripts/check-links.mjs（失败形态是变红）：
  · 正文链接必须自带 base，且目标页面必须存在；
  · 路由链接（frontmatter 的 link: 与 config.ts 的 nav/sidebar）必须是纯路由路径
    （以 / 开头、不含 base），否则框架会加两次 base；
  · base 解析不到即判红（判据跑不起来不许报绿）。
- 接线：site build 前置该门禁（Pages 部署因此不可能再发出 404 链接）；ci.yml 新增
  site-links 作业（纯 node 内置模块，不依赖 npm install）。
反向验证：修复前门禁报红 17 处；注入 1 处裸根相对链接 ⇒ 红（exit 1）⇒ 还原 ⇒ 绿。
产物级校验：本地 build 19 页、654 条内部链接，全部带 base 且在 dist 里有对应文件。

- 文档(agents)：补站点内部链接纪律——两类链接语义不同，不能同判

站点部署在子路径（base=/goagent-runtime/）：正文 markdown 链接是原生 <a href>，框架不加工
⇒ 写 '/guide/x.html' 就是 404；而导航/hero 是路由链接，框架运行时会给它们加 base ⇒ 写法相反
（必须是以 / 开头、不含 base 的纯路由路径）。
判据脚本 site/scripts/check-links.mjs 已前置到 site build 并接进 ci.yml；这里把口径写进仓
纪律，避免下一个改站点的人重犯（2026-11-23 实录：首页「接下来」三条链接全 404）。


## [v0.4.0] — 2026-11-23

- feat(cluster)：多副本高可用模板（PG + worker 池）+ 业务幂等键接缝

## cluster 模板（新增包 cluster/）
多副本共享一个 PostgreSQL：任一副本可接单可执行；同一会话在集群内**有且只有一个副本在执行**（认领 + 租约 + 接管代次，走 kernel taskboard 判据）；副本崩了由别的副本接管；结果经 PG 事件面跨副本回传（不另造结果表）。
- 判据全部来自 kernel（taskboard 抢占/租约/接管代次、sideeffect 幂等闸、roundguard 预算、eventbus 事件面），本仓只做接线，不另写第二套。
- 任务模型：一个任务 = 一次待执行的会话；任务 ID = 会话 ID；taskboard.Task.Title 承载本次输入；同一会话再次投递 = 下一轮（终态任务重开；进行中投递返回 taskboard.ErrTaken）。
- pgstore 补 Submit 投递面（宿主面，非 kernel 判据面）：新 id 插入、终态重开（清租约不重置 fence）、进行中 ErrTaken、已排队幂等。
- worker 池每 worker 独占一个 agentloop.Loop（Loop 的快照序号是进程内单调游标，共用即串扰）。
- 可认领判据用 kernel 单点函数 taskboard.LeaseHeld：终态不可领、活持有不可领、**租约已过期的 claimed 可接管**（否则崩溃副本的任务永久占用）。
## pipeline 业务幂等键接缝（新增 Tool.IdempotencyKey）
默认幂等键是编排侧铸造的 toolCallID——模型重放同一请求时通常生成新 ID（供应商不保证 tool_call_id 跨生成稳定）⇒ 闸认不出是同一次动作。宿主可按业务语义给出稳定键（订单号等），声明后跨副本接管重跑不会重复施加副作用（结构性保证）。不声明时行为逐字不变。
## 证据（真 PostgreSQL 集成用例）
- 投递 → worker 执行 → 结果经事件面回传；
- 两个独立副本抢同一会话 ⇒ 工具只被施加一次；
- 副本领了任务却不收场（模拟 SIGKILL）⇒ 租约到期后另一副本接管，已施加的副作用不再施加。
CI integration job 新增 cluster 一档（真 PostgreSQL service container）；无库时用例显式 skip，不伪造结果。
## 口径
README / AGENTS.md / 站点去掉「cluster 尚未提供」「不保证所有机制已组成高可用 Runtime」的自白，改为能力 + 前提 + 证据的正面陈述；删掉已被 VuePress 站取代的旧单页 docs/index.html。

- docs(site)：架构旗舰图换手绘 SVG 组件 + Mermaid 全站自定义主题

- 新组件 ArchDiagram（首页 + 架构页共用）：分层色带（宿主绿 / runtime 蓝 / kernel 紫 / 上游灰）、agentloop 发光高亮、圆角卡片容器、明暗双主题 CSS 变量驱动。
- 其余 mermaid 图统一 base 自定义主题：节点/连线/字色按站点设计语言（暗色蓝边深底、亮色蓝边浅底），治默认配色的廉价感。
- 底部行节点降号防文字溢出。

- docs(site)：Mermaid 图主题跟随站点暗/亮模式 + 卡片化样式

- 暗色用 mermaid dark 主题、亮色用 neutral（治默认浅黄主题在暗色站发灰发白）；
  监听 html data-theme 变化自动重渲染。
- 图容器卡片化：圆角/边框/阴影，明暗两套配色。
- svg max-width:100% + height:auto，手机端不再溢出（本问题来自手机截图反馈）。
- 所有图共用一个组件，一处修改全站生效。

- docs(site)：站点重建为 VuePress 2——多图 + 依赖与调用关系 + 小白友好

- 16 页文档站（site/）：指南（入门/上手/架构/概念）、8 个包参考、模板选型、中断恢复 walkthrough、贡献指南
- 图表全用 mermaid：分层架构图、全链路时序图、包依赖图、恢复流程图、装配时序图、决策树等 20+ 张；内置 Mermaid 组件带全局串行渲染队列（mermaid.render 不支持并发，并发/重 id 会让图空白或错位）
- GitHub Actions 自动构建部署（pages.yml），Pages 切 workflow 模式
- 删除旧的单页 docs/index.html

- docs(readme)：挂 GitHub Pages 站点链接
- docs：GitHub Pages 站点首页

- 格式(site)：架构图手机端三处根因 —— 居中溢出左端不可达 / 720px 下字号过小挤叠 / 无滑动提示

## [v0.3.0] — 2026-11-22

- feat：收编三个演示与容器后端参考实现（kernel v0.68.0 迁出）

- examples/reference-runtime、examples/worker-runtime：编排链路/worker 生命
  周期演示（package main，宿主抄的接线样板）
- containerbackend/：backend 契约的 Docker Engine / Kubernetes Job 参考实现
  （顶层库包，v0.2.0 的 backend 模板与将来的 cluster 执行面要用）
- networkpolicy_matrix_test.go：kernel faultmatrix 行十四的声明面判据随迁
  （TestNetworkPolicyDeclarationFailClosed，判据逐字未变）
- 对 kernel 的 import 保持契约面（backend/coord/worker/taskboard 等）


## [v0.2.0] — 2026-11-22

- feat：收编 pgstore 与 sqliteadapter 参考实现（自 goagent-kernel 迁入）

边界迁移第二批：kernel 的存储参考实现归位到装配层——runtime 本来就要
装配驱动（kernel 的「内核包不拖驱动」门禁正是为此而设），参考实现住这里
才是它的家。
- pgstore/：PostgreSQL 11 面统一门面 + 控制面 + SIGKILL 故障恢复 e2e
  （原 kernel examples/pgstore，42 文件）
- sqliteadapter/：SQLite WAL 11 面统一门面（原 kernel examples/sqliteadapter，
  46 文件）
- storage/ 的 SQLite 门面改指本仓包（不再 import kernel/examples）
- 对 kernel 的 import 保持契约面（checkpoint/controlplane/eventbus/
  sideeffect/sidetest/taskboard/tenant）——判据本体单源归 kernel
- go.mod：kernel v0.63.0 → v0.66.1（拿到 ForceReleaseAtomic 原子接缝）
  + pgx/v5 提为直接依赖
- CI 新增 integration job（postgres:16 service，跑 pgstore 的共享契约
  套件 + e2e，硬失败无环境开洞）
- 全量测试绿（PG 用例无 DSN 显式 skip）


## [v0.1.0] — 2026-11-21

- goagent-runtime v0.1.0：goagent-kernel 的 Agent 运行时装配层

- chat：供应商中立模型窄接口（唯一模型接缝，不 import 任何 LLM SDK）
- pipeline：工具副作用施加管线（toolCallID 幂等键 / 会话资源互斥 / 重试安全性显式声明）
- agentloop：可恢复回合循环（预算守卫调用前判定 / 检查点落历史 / 不完整历史 fail-closed）
- storage：存储装配面（内存判据移植自 kernel 参考运行时；SQLite 复用 kernel sqliteadapter）
- runtime：embedded / standalone 两模板；cluster 待 kernel 两项 P0 修复后提供
- observe：eventbus → 宿主回调
判据不另写一套：内存与 SQLite 副作用闸均跑 kernel sidetest 共享契约套件；
恢复语义由端到端用例钉住（恢复重放不重复施加，施加次数恒为 1）。
