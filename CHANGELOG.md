# goagent-runtime CHANGELOG

> 本文件由 `matrix-tools/scripts/gen_changelogs.py` 从 git 历史生成，按 tag 切段、取每段内的 conventional commit 渲染。

## [v0.9.0] — 2026-11-28

### 新增(trace)：统一 Trace 可观测——run_id 贯通 + Session/Run/Turn 读模型 + 运营八问（#42 W5）

* feat(trace): 统一 Trace 可观测——run_id 进事件面 + Session/Run/Turn/ToolCall 读模型 + 运营八问（#42 W5）
run_id 贯通（agentloop）：
- 每次 Run/Approve 铸造执行身份，emit/emitDelta 注进每个事件 payload；
  Result.RunID 同源可对账；cluster 结果载荷带 run_id（跨副本回传）
- 模型调用失败 ⇒ session_status{failed, error} + ctx 存活时落终态快照
  （Joint 单事务；ctx 已取消 = 留待恢复不落终态）——「哪次模型调用失败/
  为什么 failed」从此可回答
- 挂起事件补 pending_call_id 且改到提交前（原顺序在 Joint 形态下事件
  挂在提交后 = 永远等不到下一次提交，必丢）
- 审批闸失败：只发事件不提交——中途失败遵循「最后干净快照为恢复点」
  原则（提交会写下被 verifyHistory 判篡改的历史，会话被写死）
- 计量面：chat.ChatResponse.Usage（新增，nil=未上报不猜）随 round
  事件出面
trace 包（新，读模型）：
- Collector 挂任意 Observer 注入面，投影 Session→Run→Turn→ToolCall
  层级（事件是真相，不落库不改事件面）；载荷双形态判值（进程内原生
  类型 / PG·SQLite 往返后 JSON map+float64）
- 八问查询：RunCount / TurnCount / SlowestToolCall（call_id 配对计时）/
  Failures（RunID+失败轮次+原因）/ PauseReason / TerminalFailure /
  Usage 聚合；问 5（Worker 身份）指路 cluster Board() 任务板判据面
  （cluster 用例钉 Owner+Fence）
用例：trace 三用例端到端钉八问（含 JSON 载荷形态）+ cluster 问 5 用例；
全量回归 + race 全绿；README 补 Trace 节与包表；站点投影重生成。
#42 #37
* docs(adr): ADR-0011 事件命名法与 Trace 身份面裁决（#42 修订输入验收件）
三形态实况核对后的裁决：
- 全栈命名法 = snake_case（kernel eventbus ~40 常量为法，不改名——
  破坏性 rename + 消费方连坐，拒绝）；总纲点号词表作语义清单吸收
- agentloop 6 个 agent.* 点号常量是既有事实面：冻结不再新增，新词
  一律 snake_case；与 kernel 同义词并存期各自成文，对齐是后续独立议题
- Run 身份走载荷键 run_id，不新造 run_started 事件族（round 序列 +
  session_status 已表达边界；载荷键纯加法，一轮四事件判据不动）
- Worker 身份不进事件面：Owner/Fence 在 taskboard 判据面，投影指路
  cluster.Board().List——观测面不复制判据面
验收件：词汇 → 统一层级视图投影表（执行层 6 词 + 载荷键 + 任务板 +
机制层语义清单四行族）；trace 包文档与 README 可观测节指回 ADR。
#42

### 新增(runtime)：会话级配置分层 + 产品入口对齐（#41 W4）

配置分层（SessionOptions 覆盖，判据=不泄漏）：
- System / Budget / Tools 三件覆盖：nil=继承 Runtime 级，非 nil=仅本会话；
  带覆盖的会话在 NewSession 即装配会话级 Loop（装配错误 fail-fast，不等
  首跑），无覆盖走共享 Loop（零额外成本）
- Observer 覆盖：附加在 Runtime 级之上、按 sessionID 过滤；detach 登记到
  Runtime.Close 统一收尾（与 trackSessionStop 同锁，关闭竞态不漏不重）
- 五用例钉「不泄漏」：System 覆盖只进本会话首跑 / Budget 掐点只掐本会话
  （timed_out vs 自然终态对照）/ Tools 换面 refused 不施加而基座正常施加 /
  Observer 过滤 + Close 幂等 / 覆盖工具面有审批需求而无闸 ⇒ NewSession
  fail-closed
产品入口对齐（README 最小接入 / quickstart / embedded+standalone 模板页 /
两示例）：统一到 NewSession → session.Run 形态，rt.Run 标注为兼容入口；
修 quickstart 的 chat.Model 签名错误（实为 Chat(ctx, ChatRequest)
(ChatResponse, error)）；补配置分层与恢复锚点说明。两示例实跑验证
（恢复不重放：施加次数恒 1）。
#41 #37

### 新增(cluster)：失败模式 e2e 矩阵 + CloseGraceful 排空收尾 + SideEffectTTL 旋钮（#40 W3）

失败模式矩阵（cluster/pg_cluster_failure_test.go，真 PG，六用例）：
- 租约续期：执行 > 2×TTL 的长任务，竞争认领在整个窗口恒被拒（续期
  节拍顶住；失灵即双执行入口）
- 双活窗口：旧持有者带过期令牌回来 Finish 被拒（ErrLeaseLost）、其
  过期结论不得出现在事件面（只有接管方一条结果）
- uncertain 跨副本：Safe 未声明 + 执行体报错 ⇒ res.Uncertain 经事件面
  回传等待方（携调用 ID，不阻断终答）
- 取消 vs 在途：Close 封口有界返回、执行体感知取消、任务不悬空（租约
  到期可接管）、接管后成功施加恰一次
- 优雅关闭排空：在途自然收场不被封口、收场后任务终态、重复收尾幂等
- 排空超时回落：ctx 到期回落取消语义并如实返回错误（不静默成成功）
CloseGraceful（新）：停止认领新任务（认领前后双检排空信号，竞争赢下
的一单立刻归还）→ 在途自然收场 → 关池；worker 专用 WaitGroup 与转发
器分离（转发器不参与排空）。Close/CloseGraceful 幂等（atomic CAS），
收尾后五入口 fail-fast（Start 拒绝已关闭实例）。
SideEffectTTL 旋钮（新）：副作用闸租约从硬编码 30s 提为 Config 字段
（零值 = DefaultSideEffectTTL）。实弹发现：取消路径上 Safe 失败的闸
回收（Abandon）用已取消 ctx 静默失败 ⇒ 闸被扣到 TTL、接管方首轮可能
contended——安全不变量不破（不重复施加）但接管延迟下限 = TTL；根治
在 kernel 侧（清理 ctx 必须 cancel-proof），另提上游单。
README 补失败模式矩阵/收尾语义/已知边界；站点投影重生成。
#40 #37

### 新增(runtime)：会话一等对象——NewSession/Session.Run/Status/Approve + Close 幂等（#38 W1）

* feat(runtime): 会话一等对象——NewSession/Session.Run/Status/Approve + Close 幂等 fail-fast（#38 W1 切片1+4）
- Session 对象：NewSession 打开（不制造状态，历史由快照承载）、session.Run
  与 Runtime.Run 同锚点语义、Status 投影自 checkpoint 快照（只读不解释，
  不另造状态面）、Approve 补上挂起→恢复的模板层接缝（Loop.Approve 此前
  未暴露，挂起后宿主无法经模板层落决策）
- Runtime.Run/Approve 薄委托到 Session（兼容入口）；空 sessionID 装配期
  fail-fast（匿名会话历史互相碰撞）
- Close 幂等（重复 Close 返回 nil）+ 关闭后 NewSession/Run/Status/Approve
  一律 fail-fast（存储已收尾，不炸含糊的驱动层错误）
- 七用例钉判据：空 ID 拒绝 / 会话端到端 / 双入口历史共享 / 状态生命周期 /
  挂起幂等短路+Approve 续跑副作用只施加一次 / Runtime.Approve 兼容 /
  Close 幂等与事后 fail-fast
判据不另写：状态词表与终态分类仍归 kernel contract；执行可变状态仍在
agentloop runState 与快照序号闸。
#38
* docs(site): 生成页随 #38 重生成——runtime 包行数/文件数 + 补 v0.8.0 版本段投影
npm run gen:reference 按仓库事实重算：session.go 落地后 runtime 包
1→2 文件、201→329 行；releases/llms 的 v0.8.0 事实面此前过期（预写
时生成的产物），一并追平。CI 生成页同步校验的执行体。
#38

- 构建(bridge)：地板追到 runtime v0.8.0（回迁后 yi-nology path 首版）

- 文档(adr)：ADR-0010 执行接口两模型分立——Loop/Pipeline 边界定型（#39 切片1）

- 边界判据=谁决定下一步：Loop 下一步由模型定（自主决策），Pipeline
  （若立项）由依赖图定（确定性流程）——不压成一个执行模型
- 共享的是 Run/Step/Checkpoint/Event/Cancellation 语义与读面，不是控制流
  抽象；判据仍全归 kernel（checkpoint/eventbus/roundguard/sideeffect）
- 家族组合规则钉死：流程力学组合 oekit/workflow、Multi-Agent 拓扑守卫
  组合 oagentkit/agent/dispatch——本仓不自造流程引擎与拓扑守卫；换执行
  模型不换副作用闸
- Pipeline 执行器条件立项（有消费方才建）；反例面（不共享 Round/回合）
  与拒收面成文，防统一压力与第二套引擎
- README 执行模型节留摘要指回 ADR；站点投影重生成（ADR 9→10 篇）
#39 #37

- 文档(site)：生成页重生成——releases.md 补 v0.8.0 段、llms.txt 地板随 kernel v0.80.0

npm run gen:reference（gen_reference_pages.mjs）按仓库事实重算：
v0.8.0 tag 落地后 releases 与 llms 的事实面变了，预写时生成的产物已过期
（CI 生成页同步校验红）。


## [v0.8.0] — 2026-11-26

### 新增(cluster)：claimNext 单次 Claim + README 裁定段（#24 方案 C①）

* feat(cluster): claimNext 单次 Claim 形态 + README 裁定段——runtime#24 方案 C①落地
- claimNext：筛出首个可领候选后只 Claim 一次（对齐 kernel/worker.Loop），
  消掉竞争激烈时每拍 n 个 FOR UPDATE 认领事务（认领风暴）；
- README cluster 节补「为何不整段换 kernel/worker.Loop」（Loop 同样 O(n) List
  + 强制 Registry 不存在 = 语义扩容；家族先例 kernel#507 同形不抽）；
- O(n) List 根治=pgstore 实现 taskboard.ReadyBoard.ClaimReady，另行排期。
Fixes #24（方案 C① 落地；C② ReadyBoard 挂账后续 PR）
* docs(site): 生成页随 claimNext/README 改动重生成（16 产物同步）

### 修复(sqliteadapter)：Complete 的写回判定与 pgstore 对齐——写回没落到判定时那行 ⇒ ErrLeaseLost

runtime 侧三处实现（`pgstore` / `sqliteadapter` / `examples/reference-runtime`）
在 kernel v0.78.0（v0.76.0 ⊂ v0.78.0）上已按新契约落地，共享套件的新子判据
「租约过期后完成被拒」在本仓 **PASS**。
本 PR 只收一处**两侧口径不一致**：同一个事实——「这次完成确实落在**判定时那一份**
行上」——pgstore 判了（行锁 `FOR UPDATE` + 命中行数 ≠ 1 ⇒ `ErrLeaseLost`），
sqliteadapter 没判（`UPDATE` 只带 `WHERE id = ?`，且不看命中行数）⇒ 同一事实两份
判据，必然漂移。
处置：sqliteadapter 的写回改成**条件写**（`UPDATE` 带 token + state 谓词，与同
文件 `Release` 路径的 `releaseOrComplete` 同款）+ 命中行数判定（≠ 1 ⇒
`ErrLeaseLost`，不静默成功）。
影响：happy path 行为不变；只在「判定与写回之间那行被换了令牌或状态」时，由静默
成功改为 `ErrLeaseLost`。
顺带更正注释：那条判据落在 kernel **v0.76.0**（原注释写 v0.77）。
验证：
- `make check` 全绿（vet + 全量测试含真 PG + 依赖面门禁）；
- 共享套件新子判据「租约过期后完成被拒」在 sqliteadapter 与 pgstore 均 PASS；
- `go build` / `go vet` 通过；改动文件 `gofmt` 干净。
Co-authored-by: zhangyi01 <zhangyi01@kylinos.cn>

### 重构(boundary)!：org 归属回迁——module path 变回 yi-nology（v0.8.0）

* refactor(boundary)!: org 归属回迁——module path 变回 yi-nology（v0.8.0 破窗）
仓库已迁回 yi-nology（runtime#34 公告）。module path：
github.com/oagentkit/goagent-runtime → github.com/yi-nology/goagent-runtime；
bridge 嵌套模块同步（.../bridge）。
- 全仓自身路径重写（代码/配置/文档/生成页，含对 kernel 的引用回改）；
- 生成页重生成（16 产物同步）；
- build/vet 全绿（scratch replace 指向本地已回改 kernel 验证；
  runtime tag 等 kernel v0.80.0 先行后发）。
v0.8.0 起回 yi-nology 路径；v0.7.0-0.7.1 的 oagentkit tag 只能经 oagentkit
路径消费（go.mod 声明即事实）。
* build: kernel 地板追到 v0.80.0（回迁后 yi-nology path 首版）
* build: kernelFloor → v0.80.0 + 生成页重生成

- 构建：依赖地板追到最新——goagent-kernel v0.79.0（v0.78.0 之后 kernel 发了地板收口版）

- 文档：补「本仓依赖的矩阵内仓」声明——矩阵 README↔go.mod 对账门禁（agent/goagent-kernel/guardx）
- 文档：矩阵层位声明行——tier 5（装配层）

矩阵 verify_repo_tiers 门禁要求 AGENTS.md「矩阵分层：**tier N（层名）**」
规范行 + README.md「—— oagentkit 模块矩阵 **tier N**（层名…）」定位行。
modules.json tiers 映射同步补 4/5。


## [v0.7.1] — 2026-11-26

- 构建：依赖地板追到最新——kernel v0.78.0 / guardx v0.30.0 / pkg v0.61.5（入驻 oagentkit 矩阵后的地板时效对齐）
- 构建(bridge)：嵌套模块 go.sum 落地 + CI job——bridge/llmchat 与 bridge/einotool 进门禁

runtime v0.7.0 tag 后 bridge 的依赖可解析（eino 0.9.23 随 llm v0.27.0 的
模块图解析）；CI 补 bridge job（build+vet+test），LLM SDK 边从此有门禁守着。


## [v0.7.0] — 2026-11-26

### 新增(agentloop)：接上工具调用账本——执行前落 planned、恢复按账本续跑

- 5edc368

### 新增(storage)：工具调用账本存储面——PG/SQLite 两份 DDL + invocation.Store 实现

- 89a8a0d

### 修复(site)：锚点落位认「读者意图」不认框架改写的 hash——修掉连跳两次的抖动

上一版（#19）把滚动接管做完了，但线上仍会抖。真实计时抓拍（headless Chrome，
线上 recovery.html）显示：落位**正确**（scrollY=1207、标题 top=70）之后，框架
随滚动改写了地址栏的 hash，守卫照着改写后的 hash 追 ⇒ 页面被拽上去两次
（1207 → 631 → 76）。
改写 hash 的是主题自带的 @vuepress/plugin-active-header-links：它随滚动把
「当前所在章节」写进地址栏（监听 scroll → 比对各标题 offsetTop →
router.replace({hash})）⇒ **地址栏里的 hash 是框架的派生态，不是读者的意图**。
改动：
- 落位目标改为「读者明确要的锚点」（URL 初始 hash / 点锚点 / 路由 push /
  前进后退），不再每拍去读 window.location.hash；
- 框架的「当前章节」判定与落位偏移对齐（define 覆盖 __AHL_OFFSET__：章节起点
  = 标题 offsetTop，而落位把标题放在导航栏下方 70px ⇒ 落位点落在**上一节**里
  ⇒ 插件会立刻把深链锚点抹掉）；
- 自己发起的滚动不再被接管窗口改写（否则点「最后一个小节」会被拽回旧目标）；
- 目标够不到顶时按文档底部落位并交还滚动权（否则接管窗口不关 ⇒ 读者上滚被劫持）；
- 落位期间把地址栏的 hash 钉在读者要的锚点上（框架会按「读者在页首」清空它）；
- 路由初始导航完成前不许动路由（否则 replace 会把待执行的初始导航顶掉 ⇒ 整站
  落到首页）。
实测（headless Chrome，真实计时，非虚拟时间）：
- 深链 #uncertain-另一种死法 ⇒ 一次落位 scrollY=1207、标题 top=70、hash 不漂；
- 侧栏目录项 ⇒ 一次落位 top=69；标题旁原生锚点 ⇒ 一次落位 top=70；
- 线上旧版（#19）对照：点侧栏后被拽到 y=76（标题 top=624，偏 555px）。

### 修复(site)：锚点落位等「布局已定」再滚——接管框架按陈旧布局算的平滑滚动

- fcbbceb

### 修复(site)：锚点落位按「布局已定」再滚——接管框架按陈旧布局算的平滑滚动

- 75ed374

### 修复(site)：接管 hash 定位 + 对齐循环持续复查（消除「目标已在位就收工」的早退）

- 32f7403

### 修复(site)：锚点落位别再抖——内容长高后重新对齐 + 同 hash 点击也滚

mermaid 图是纯客户端渲染的：围栏在构建期只产出一个空容器，SVG 要等浏览器里
mermaid.render() 跑完才注入 ⇒ 容器高度从几十 px 长到几百 px（本页三张图合计
约 1160px）。而锚点定位（浏览器 hash 定位 + vue-router 的 scrollBehavior）
算在「内容还没长出来」的那份布局上 ⇒ 落位按旧布局算，与最终位置差出几百 px
（实测 recovery 页偏 577px；开了平滑滚动时甚至被 Chrome 的 scroll anchoring
顶到页面底部），冷/热加载还时对时错 ⇒ 观感就是「抖」。
三件事互相抢同一个滚动位置：浏览器 hash 定位、vue-router 的平滑 scrollBehavior、
Chrome 的 scroll anchoring。谁最后落地取决于时序 ⇒ 判一次必然不稳。
改动：
- ① 对齐循环：每 250ms 复查目标在不在位，不在位就数值化地滚过去（位置自己从
  此刻的 rect 算，滚动自己做 ⇒ 不受 scrollIntoView 的陈旧位置影响），顺带打断
  在飞的平滑滚动。循环「稳了」的判据是「滚动位置连续 2 秒没动 且 目标已在位」——
  不是「目标已在位」（框架的平滑动画会从目标旁边滑过去那一刻，收工就等于把最后
  一棒让给了它）。
- ② 标题加 scroll-margin-top（导航栏高度 + 余量）：否则锚点标题会被 fixed 导航栏
  压住；这份偏移是浏览器、scrollIntoView 与本页判据共用的唯一口径。
- ③ 同 hash 锚点点击自己滚一次：vue-router 判它「重复导航」⇒ 不触发
  scrollBehavior ⇒ 点击完全没反应。
- ④ Mermaid 组件在渲染队列排空时派发信号，供上面的循环判「这一批图渲染完了」。

### 修复(执行完整性)：闭合评审 P0-01/02/03/04/06/07/08 与 P1-02（审批不绕过、竞争不 Panic、结果不串轮、收尾不半截）

第三方联合源码评审逐条核实后修复 8 项，每项带回归用例；关键项做过反向验证
（临时回退修复 ⇒ 用例必红）。
P0-06 资源竞争不再 Panic：kernel 的 contended 是「正常判据」（OutcomeContended +
nil error，刻意不返 error），装配层原走 default 分支盲调 err.Error() ⇒ 空指针。
补 KindContended 词表 + 显式分支；未定义 outcome 也不再盲调（失败形态是变红，
不是 Panic）。
P0-08 声明要审批的工具不再被静默放行：runtime/cluster 的装配面此前没有
ApprovalGate 字段 ⇒ 标准模板里 NeedsApproval=true 的工具直接执行。现暴露审批闸
并在**装配期** fail-closed：有工具要审批却无闸 ⇒ 构造失败（nil 闸 ≠ 放行）。
P0-01 异步审批恢复带回原始参数：此前只重建 ID+工具名 ⇒ 执行体收到空参数，
施加的不是获批的那个动作。现从挂起快照取回完整 ToolCall。
P0-07/P1-02 Loop 的可变状态改为每次执行独立（runState）：seq/pendings 原挂在
Loop 上 ⇒ 并发会话互相串扰（跨会话事件被并进别人的事务、序号回退导致提交被判
陈旧）+ 失败执行的事件滞留到下一次执行。
P0-02 同会话第二轮不再拿到第一轮的结果：结果事件按 sessionID 过滤，而 sessionID
是长期会话身份 ⇒ 等待方从事件流开头扫会先撞上上一轮的结果。等待起点改为「本次
投递前的事件面水位线」（事件 ID 单调 ⇒ 本轮结果必在其后）。
P0-03 结果事件与任务完成同事务（pgstore.Finish）：原先分开写 ⇒「任务已完成但
结果没到」或「旧执行者的结果已对外可见而任务没完成」两类半截状态。任一失败整批
回滚 ⇒ 任务保持可恢复状态。
P0-04 业务幂等键跨会话重试不再被判动作身份冲突：kernel 的动作身份三元组含
Resource，默认资源键掺 sessionID ⇒ 同一次业务动作换个会话重试即 ErrActionConflict
（判 failed 而非 replayed）。默认资源键在声明业务幂等键时改用业务口径，并新增
Tool.ResourceKey 供宿主显式声明真正的互斥域。
门禁：把 go test -race 接进 Makefile 与 CI（竞态判据要有人跑它；-race 在 Linux
强制要求 cgo，故显式写 CGO_ENABLED=1）。
评审中未在本 PR 闭合的项（需架构级改造，判据归 kernel）：P0-09/P0-10（执行前
落调用身份的 Tool Invocation Journal）、P0-05（Fence 传到工具执行体）、P1-01
（服务端批量认领）、P1-03（Complete 的租约有效期口径属 kernel 契约缺口）。

### 重构：org 归属迁移 + roundguard 新路径 + 家族钦定适配器（v0.7.0）

* refactor!: org 归属迁移 + roundguard 新路径 + 家族钦定适配器（v0.7.0 破窗）
① module path：github.com/yi-nology/goagent-runtime → github.com/oagentkit/goagent-runtime
（仓已迁入 oagentkit org，见 #22；旧 URL 经 GitHub 重定向可继续访问）。
全仓 import/文档/site 引用同步改写。
② kernel 地板：github.com/oagentkit/goagent-kernel v0.77.0（kernel 同波改名 +
对话域七包上抛，roundguard import 改指 github.com/oagentkit/agent/roundguard）。
③ 家族钦定适配器（嵌套模块 bridge，LLM SDK 边只在 bridge 内——主 module
「模型接缝只有 chat.Model、零 LLM SDK import」的声明面不变）：
- bridge/llmchat：oagentkit/llm → chat.Model（裸模型路径承接工具调用；
  流式逐片搬运 + DeltaAggregator 归并出最终响应）
- bridge/einotool：eino InvokableTool → pipeline.Tool（副作用闸字段不猜）
④ AGENTS.md 白名单补 invocation（pgstore/sqliteadapter 实际 import 面，
文档漂移修正）。README 补 bridge 两包与接缝声明。
发版顺序：kernel v0.77.0 → agent v0.16.0 → 本仓 v0.7.0 → bridge/v0.1.0。
cluster 池接线复评（vs kernel/worker.Loop、queue/worker.Pool）另开 issue，
不在本 wave 夹带。
* fix: go.sum 补齐新路径条目——kernel v0.77.0 / agent v0.16.0 tag 落地后的真实 tidy
* fix(taskboard): 三套 Board 实现补 Complete 凭据面——租约过期拒收（kernel v0.77 boardtest 新判据）
kernel v0.76.0 起 taskboard.Complete 的凭据面 = 令牌 + 租约未过期
（ErrLeaseLost；判据走 taskboard.LeaseHeld 单点）。本仓三套实现随 kernel
v0.77.0 地板补齐，判定与写回在同一事务内（与 Renew 同一纪律）：
- examples/reference-runtime（内存）：s.clock() 锁外取 + LeaseHeld
- pgstore：lockTask 行锁内 nowIn + boardRow.held + 条件写回
- sqliteadapter：同事务读行 + boardRow.held + 条件写回
boardtest 新增子判据「租约过期后完成被拒」三实现全绿。
站点生成器同步：KERNEL_MOD/站点 URL 改指 oagentkit；pitfalls.json
kernelFloor v0.75.1→v0.77.0；16 个生成产物对账绿。

- 文档(site)：站点口径同步 kernel v0.75.1 + 参考页重生成
- 文档(site)：生成页随 v0.6.4 合流 + race 门禁重生成（门禁体系页新增 race 作业与 make race 目标）

## [v0.6.4] — 2026-11-24

- 文档(site)：生成页随 v0.6.4 段重生成——版本记录页 + llms.txt 追平

站点的「版本记录」页与 llms.txt 是**生成物**（由 site/scripts/gen_reference_pages.mjs
从仓内 CHANGELOG.md 投影），手改会被 CI 判红。本提交只重生成，不手改内容。
- 版本记录页新增 v0.6.4 一行；
- llms.txt 的「已发布版本 13 个，最新 v0.6.4」随事实面追平。

- 文档(readme)：v0.6.4 发布面——补「版本与发布」入口（README 原本无版本面）+ 记下 tag 快照必须含本版段这条纪律

README 此前**没有任何版本面**：下游落到仓库首页看不到发布说明入口、也不知道版本号
的事实源在哪。本节补三件事：
① 发布说明入口（GitHub Releases + 仓内 CHANGELOG.md + 站点「版本记录」页是它的投影）；
② 版本号的事实源指明是 tag 与 CHANGELOG——本 README 不抄第二份版本号（抄了只会漂）；
③ 一条纪律：`go get` 拿到的模块里 CHANGELOG 必须含该 tag 自己那一段，tag 打在 CHANGELOG
   生成之前就会缺段——v0.6.3 就是这么缺了一段（CI 的「CHANGELOG 同步校验」作业判红），
   其修复随 v0.6.4 出货。
本版零代码变化（Go 代码零改动）。

- 文档(changelog)：清流氓本地 v0.6.2 tag 后按远端边界重生成——CI 对账归一

根因：本会话早前 git tag v0.6.2 本地成功/推送失败（远端已被并行会话占用），
本地从此留了指向 rebase 前旧提交的流氓 tag，切段边界与 CI（远端 tag=73feddd）
不同 ⇒ 本地对账恒过、CI 恒红。tag -d + fetch --tags --force 后按远端边界
重生成，两侧对齐。

- 文档(changelog)：v0.6.3 段改由生成器产出（手写段与对账产物不一致，CI 判红）+ releases 追平
- 文档(changelog)：补 v0.6.3 版本段（tag 已打，生成器对账）+ 站点 releases/packages 追平

v0.6.3 tag 已打但 CHANGELOG.md 缺该版本段 ⇒ CI「CHANGELOG 同步校验」判红
（v0.6.1/v0.6.2 同款收尾工序）；站点生成页随段重生成。


## [v0.6.3] — 2026-11-24

### 新增(agentloop)：工具事件拆成执行前请求+执行后收口（批669 面2 事件映射的上游前置）

EventToolCall 语义收紧为**执行前**的请求事件（payload 补 args——宿主 SSE
工具卡在卡创建时即拿到参数）；新增 EventToolResult 收口事件（call_id/tool/
outcome/content），两侧 call_id 配对；审批闸拒绝以 tool_result 收口
（outcome=approval_denied、content=拒绝观察）。outcome 复用 pipeline.Kind
词表 + approval_denied 专用值。执行前后的发射点在同一顺序代码段内（与
「回填与提交同段」同一纪律）。
宿主映射（bianque 批669）：agent.tool_call→agentrun tool_call（卡创建含参）、
agent.tool_result→agentrun tool_result（卡收口含结果）——loop 路径的 SSE
工具卡与既有 agentrun 路径同形态。相关测试口径同步（一轮四事件→五事件）。

- 文档(changelog)：补 v0.6.2 版本段（tag 已打，生成器对账）+ 站点 releases 追平

## [v0.6.2] — 2026-11-24

### 新增(agentloop)：tool_call 事件补结果字节数（审计轨迹面）

执行轮 EventToolCall payload 增加 bytes=len(result.Content)——宿主落
工具轨迹日志只记长度不记内容的既有纪律需要该维度；审批拒绝等无结果
的调用轮不带本键（桥接方按缺省处理）。判据：counter 返回 "ok" →
bytes=2 断言。

### 新增(make)：真集群验收入口随判据体落到本仓（#418 的可复跑入口）

判据体随边界从 goagent-kernel 迁到本仓，可复跑入口（make k8s-e2e）一并搬过来——kernel 侧那个目标指向已删目录，是悬空入口（跑必失败，且让人误以为该路径在那个仓可跑），已撤。站点生成页随新目标重生成。

### 新增(chat)：#11 收口——承载面扩展（工具片段+usage）+ 框架归并器 + Joint 模式 delta 实时

#11 的验收标准是「流式宿主不再写聚合桥」。v0.6.0 的 OnDelta 只搬走了
「逐片投递」，**归并**（按 Index 归并工具调用、usage 累计）仍留在每个
宿主手里——各写各的、各踩各的坑。本笔把那段收进框架。
① 承载面（chat/chat.go）：Delta 增加可选的 ToolCall 片段与 Usage 计量
   （可选字段 ⇒ chat.Model 契约一字未改，非流式宿主零改动）。
② 归并器（chat/stream.go）：DeltaAggregator 按 Index 归并工具调用、累计
   usage、拼接文本——**唯一一份**实现，宿主用它拼响应。
③ loop 接线（agentloop/loop.go）：
   - 挂载条件从「Emitter 非 nil」放宽为「Emitter 或 Joint 非 nil」——
     此前事件随快照落库的形态（standalone/cluster）里流式增量静默消失；
   - Joint 形态下增量**只即时广播、不落事件行**（观测面 ≠ 状态面：落进去
     会污染状态面，也会打破「一轮四事件」判据）；
   - 修正一处说谎的注释：联合提交失败时本批事件**作废**（不是「保留待下轮
     重试」——重试是重跑该回合，把上一批挂着等下一轮提交等于混两轮事件）。
④ 判据：chat 包从零测试到 6 条（归并/usage/文本/非法 Index 丢弃/词表）；
   agentloop 新增 Joint 形态用例（逐片广播 + 不落事件行 + 状态面四事件
   不受增量影响）。
⑤ 文面：站点 chat.md 的**过期签名**改正（Chat(ctx, messages, tools)
   (*Response, error) / Response / ToolDecl 全都不存在），依赖图改正
   （chat 零外部依赖）；补流式旁路段；README 补「流式模型」小节；
   新增 ADR-0009（形态取舍：不采纳拉取式 Streamer 的理由）与 ADR-0008。
本地校验：make check 全绿；site build 33 页、链接门禁 32 页全过。

### 修复(storage)：#10/#12 收口——判据面零驱动边（门禁化）+ 驱动名选型红线 + ADR-0007

#10 的代码拆分已由 cf60a3f 落地，但**纪律没有落地**：同形面可再生、
对账表自己漏报、文档零提示。本笔把三处补齐。
① 机器判据（新增 scripts/check-dep-edges.sh + make deps + CI 作业）
   纯判据包（storage）的生产依赖面与测试依赖面都不得出现数据库驱动；
   go list 跑不起来同样判红（判据跑不起来不许报绿）。
   反向验证：往 storage 注入 import _ "modernc.org/sqlite" ⇒ 判红退出 1。
② 判据面盲区（站点生成器）
   listDiskPackages 原来只扫顶层目录 ⇒ 嵌套子包（storage/sqlitestore）
   对它隐形 ⇒「README 包表 ⇄ 磁盘目录」那张对账表自己在漏报。
   改为递归下钻；自测 13 → 17 条；反向验证：去掉递归 ⇒ 3 条判红。
③ 选型红线（#12）
   README「SQLite 驱动名约束」+ sqliteadapter 包文档：驱动名 "sqlite"
   是进程级全局注册表，同名第二次注册 init 即 panic；给出去路
   （只要内存面就只 import storage）与排查线索。
另：README「包」表补 adjudicate 与 storage/sqlitestore（对账 1 处漂移 → 0 处）；
触驱动的测试迁到绑定包（storage 的测试依赖面也归零），新包补判据——三面齐备 /
空路径 fail-fast / **直接实现 storage.JointCommitter**（只给 AsJoint 转发器时
runtime 装配的能力断言恒 false ⇒ 静默落回两步提交路径）；新增 ADR-0007 + 索引。
本地校验：make check 全绿（vet + 全量测试 + 依赖面门禁）；site build 31 页全过。

- 杂务(release)：补 v0.6.1 版本段（tag 已打）+ 站点生成物跟平

v0.6.1 tag 已打但 CHANGELOG.md 缺该版本段 ⇒ CI「CHANGELOG 同步校验」判红；
cf60a3f 的 storage 拆分改了包表 ⇒ 站点「机器事实」页与 llms.txt 的行数快照失效 ⇒
CI「站点构建」判红。两条都是生成物滞后，按纪律重跑生成器跟平（未手改生成物）：
- CHANGELOG.md（家族工具 gen_changelogs.py，10 个版本段）
- site/src/reference/{packages,releases}.md、site/src/.vuepress/public/llms.txt
本地校验：gen_changelogs.py --check ⇒ 覆盖完整；gen_reference_pages.mjs --check ⇒
OK：13 个生成产物与仓库事实同步；check:links ⇒ 29 个页面全过。


- 格式：收掉 main 上既存的 gofmt 漂移（7 个文件 / 10 行，零行为变化）

这些漂移从初版提交起就在（CI 只跑 vet，不跑 gofmt ⇒ 一直漏过）：
注释缩进（`//（...` → `// (...`）与结构体字段对齐。纯格式，无行为变化。
本机校验：go build ./... 与 go vet ./... 全绿；gofmt -l 全仓为空。


- 测试(pgstore,cluster,storage)：#9 判据收口——PG 一轮四事件装配端到端 + 反向原子性（两侧形态）

#9 的验收判据两条：① 原子性直证（联合提交失败 ⇒ 事件行零落）；② Joint 装配
端到端（一轮四事件）。核验发现两处缺口：① 只证了「快照 CAS 先失败」一个
方向（另一半「事件面失败 ⇒ 快照行不落」两侧都缺）；② 「一轮四事件」只有
SQLite 形态有断言，**PG 侧完全空白** ⇒ cluster 走联合提交这条路径无任何
断言保护（哪天静默落回两步路径，没有用例会红）。
① 反向原子性（两侧形态各一条）：注入一个不可序列化的 payload（函数值）⇒
   事件面必失败 ⇒ 断言快照行**读不出来**且事件行零落。
   用例：storage/sqlitestore/joint_test.go、pgstore/joint_test.go。
② PG 侧一轮四事件（装配端到端）：cluster 跑一轮（两轮：请求工具 + 终答）
   ⇒ 断言事件面里 2×round_start + tool_call + session_state。
   用例：cluster/pg_cluster_test.go（真 PG）。
本机实跑：真 PostgreSQL 16（docker 容器 gak-cluster-pg）下 pgstore 与
cluster 全绿；make check 全绿；site build 33 页。ADR-0008 补记：两条判据
现在两侧形态都齐。


## [v0.6.1] — 2026-11-23

### 重构(storage)：storage 纯化拆分——sqlite.go 移 storage/sqlitestore（modernc 驱动边不进纯化包；bianque 双 driver 注册 panic 实弹）+ *SQLite 补 CommitWithEvents 转发（批662 assemble 能力断言恒 false 缺口）+ 检测回归钉

- cf60a3f

- 文档(changelog)：补 v0.6.0 版本段（tag 已打，生成器对账）+ 站点 releases 追平

## [v0.6.0] — 2026-11-23

### 新增(chat)：流式增量回调 OnDelta + agentloop content_delta 事件

- chat：Delta{Kind,Text} + 词表 DeltaKindText/Reasoning + ChatRequest.OnDelta
  （可选回调；非流式实现可不调，nil 语义同缺省，响应语义不变）
- agentloop：EventContentDelta="agent.content_delta"；drive 在 Emitter 非 nil
  时挂 OnDelta 逐片发事件（kind/text 原样、保序），无观测面零开销
- 测试：有观测面逐片断言（含 reasoning kind）+ 无观测面不挂回调
- 顺带 gofmt 修 main 既有漂移（文件头注释缩进 + 工具回合块缩进）
- 站点生成页随 API 追平（packages.md/llms.txt）
* docs(changelog): 补 v0.5.1 版本段（tag 已打，生成器对账缺段）+ 站点 releases 追平

## [v0.5.1] — 2026-11-23

### 新增(adjudicate)：逐项观测钩子 Observer + ErrNotUncertain 并发分类

* feat(adjudicate): 逐项观测钩子 Observer + ErrNotUncertain 并发分类（#1 增强，宿主收编前置）
提出方 argus 收编 effect.Reconciler 的两处前置（oekit 侧对等语义已实证）：
- WithObserver：每项结算后同步回调 Observation{Item,Event,Resolution,
  Outcome,Err}——宿主接自己的 obs/日志面（观测分布/告警/结构化日志），Loop
  保持零日志零指标纯库纪律。事件词表 settled/skipped/deferred/failed。
- ErrNotUncertain → Skipped 分类（不进错误面）：并发对账时另一对账者/人工
  先落结论不是故障——否则每个可重试 Key 每周期刷失败告警（argus 生产实证
  语义，v0.5.0 漏搬）。观测面带 sentinel 可辨。
- ReconcileOnce 项间 ctx 取消检查：中止并返回 ctx 错误（已结算 Stats 保留）。
测试 +3：observer 三事件面（settled 带 Resolution/Outcome、skipped Err=nil、
deferred 带错）/ErrNotUncertain 计 skipped 且零错误返回/ctx 取消中止。
* docs(changelog): 补 v0.5.0 版本段（tag 已打，生成器对账缺段）
* feat(adjudicate): WithGrace 接受显式 0（即时对账/测试形态，负值仍忽略）+测试
* docs(site): 生成页追平 adjudicate Observer 增强后的生产行数

### 重构(agentloop)：StatusAwaitingApproval 对齐 kernel contract 既有词表（StatusWaitingApproval=waiting_approval Open 态，同义异名统一）

- 646fff2

## [v0.5.0] — 2026-11-23

### 新增(adjudicate)：副作用未知态周期裁决框架（#1）

kernel 判据把「结果未知」的 Key 硬阻塞（再动即重复施加），pipeline 分
类出 KindUncertain——但 uncertain 之后没有下文。本包把「证据回查→三态
判定→落裁决」变成机制（提出方 argus internal/effect 生产实证，多版本
迭代在役；domain 裁决器留宿主，本包零业务词）：
- Source 接口：枚举未知态 Key（kernel Store 契约只有 Mark/Resolve 无
  枚举——枚举是宿主存储面的事，AGENTS 边界纪律不另写判据）
- grace 静置窗（DefaultGrace 5m）：给施加方「失败→标未知→释放」窗口，
  避免与在途动作抢判；batch 上限（DefaultBatch 50）
- Router 裁决分发：按声明顺序第一个「认识」的生效；error 透传=判据不足
  本轮不裁决（判据不足≠未生效——把「查不到」当「没生效」放行就是二次
  施加入口）；全不认识=Skipped 留痕绝不猜
- Loop.ReconcileOnce 无副作用单轮（周期由宿主 ticker 驱动，不起 goroutine），
  单行失败不阻断；Stats 积压/裁决分布可观测
- 落裁决走 kernel Runner.Reconcile（ForceRetryable/重放语义归 kernel，
  本仓不发明第二套判据——AGENTS 红线）；oekit/pkg/errors 单源不混 stdlib
测试七组（storage.NewMem 契约实现 + 合成 Source/Resolver）：已生效记完成
后 Acquire 重放取既有结果/safe 未生效释放重授予（资源栅栏 TTL 正交实证）/
无人认识跳过不改态/判据不足 defer 且 Key 原样/grace·batch 参数如实透传/
装配期 fail-fast/Router 顺序与 error 短路口径。
* docs(site): 生成页同步——adjudicate 包入站点事实面

### 新增(pgstore,cluster)：联合提交面 PG 形态——cluster worker 事件随快照单事务（part of #9；pgEmitter 落库职责并入联合提交，转发语义不变）

- 37d526f

### 新增(storage,agentloop)：联合提交面——事件行与快照行同生共死（fixes #2 主体：孤儿事件/终态丢失/恢复重复三窗口在 SQLite 形态结构闭合；Joint 模式自动装配）

- c0eade2

### 新增(agentloop)：审批闸异步挂起/恢复——三态决策闸+awaiting 快照+Approve 续跑（fixes #4；780baa3 同步闸+本笔异步形态=完整闭环）

- 99160ce

### 新增(agentloop,pipeline)：工具执行前人工审批闸第一切片（part of #4，同步闸：NeedsApproval 声明+闸拒绝回喂+fail-closed）

- 780baa3

### 新增(agentloop)：收尾提示词宿主注入 + 事件发射失败可观测（fixes #3, fixes #2 可观测面）

- 8c24915

### 新增(site)：补齐参考面（CHANGELOG / ADR / llms.txt）+ 导航补本站 llms.txt 与决策记录

- e7b087c

### 新增(site)：主导航统一成 kernel 形状 + 补参考页（机器事实 / 门禁体系 / 坑速查）

- 33622fd

### 修复(agentloop)：approval.go 补 commit 新签名缺的 res 实参（99160ce 改签名漏改调用点，main 编译红）

- 108c1a8

- 杂务(site)：跟平生成物（agentloop 生产行数随 #2/#3 变更）

8c24915 改了 agentloop/loop.go，生产行数 333 → 359，机器事实页与 llms.txt
的行数快照随之失效 ⇒ CI「生成页同步校验」判红（site 作业 failure / exit 1）。
本提交只重新生成这两个产物，未手改生成物：
- site/src/reference/packages.md
- site/src/.vuepress/public/llms.txt
本地校验：gen_reference_pages.mjs --check → OK：13 个生成产物与仓库事实同步；
npm run build → 自测 + 链接门禁（29 页）+ vuepress 构建（30 页）全绿。


- 文档(agents)：会话操作纪律——腐化病防线（报成功≠安全须核盘/单调用块/照抄已验证源/渗盘即洗，bianque 批656 渗盘实录移植）

- 格式(site)：站点三态配色（白 / 暗 / 自动）+ 对齐 kernel 视觉语言 + 首页配套链接行

runtime 站此前跑的是 VuePress 默认调色板（绿品牌色），与 kernel 站那套 GitHub 风
不是同一种视觉语言 ⇒ 读者在两站之间来回跳，会读成「这是另一个站」。
改动：
- 新增 styles/index.scss：与 kernel 站逐值对齐的两套配色板
  （亮 = GitHub Light 官方值，暗 = kernel 迁站前那套深色），外加 hero /
  特性卡 / 首页胶囊行的样式；品牌辅色在亮色下压深一档（#58a6ff 在白底上
  对比度不足）。
- 新增 components/ThemeModeSwitch.vue：用 VuePress 的公开别名机制
  （app.pluginApi.hooks.alias）替换主题自带的 VPToggleColorModeButton，
  给出「跟随系统 / 亮色 / 暗色」三态入口。接的是主题**同一份**存储
  （localStorage['vuepress-color-scheme']），不是另造一套配色逻辑。
  为什么必须覆盖：VuePress 2 的原生开关只有两态（亮 ⇄ 暗），
  「跟随系统」只是**首次访问**时的取值来源，用户手动切过之后就再也回不到。
- config.ts：colorMode 'auto' + colorModeSwitch true；别名覆盖的 plugins；
  顶栏加「AI 直读」下拉（站群配套链接，与 kernel 站同一入口同一标题）；
  theme-color 改为跟随系统明暗（固定一个值总有一半模式对不上）。
- README.md：首页加压轴横幅 / 安装条 / 配套链接胶囊行（正文链接按本仓
  链接门禁的纪律自带 base 前缀——原生 <a> 不加工 base，子路径部署下 404）。
验证：npm run build 全绿（含站点内部链接门禁：18 个页面）；浏览器实测三态
往返（auto → light → dark）与 ArchDiagram 自身的 --ad-* 配色同步切换。


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
