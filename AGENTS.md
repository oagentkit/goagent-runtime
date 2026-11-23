# AGENTS.md —— goagent-runtime 工作纪律

本仓是 goagent-kernel 的**装配层**：把内核机制接线成业务方可直接用的运行时。
内核判据（状态/幂等/栅栏/预算/恢复）一律归 kernel，本仓**不发明第二套**。

## 边界纪律

- **不复制内核判据**：存储语义只用 kernel 契约（`checkpoint.StoreCAS` /
  `sideeffect.Store` / `eventbus.Store`）表达；新存储实现必须跑 kernel 的
  `sidetest` 共享契约套件（本仓 `storage/storage_test.go` 是范例），
  不得在本仓重写一套「类似」的判据。
- **领域概念全注入**：系统提示、工具、模型供应商适配是宿主的事。本仓
  生产代码不得出现任何具体业务词。
- **chat.Model 是唯一的模型接缝**：不 import 具体 LLM SDK。
- **cluster 模板的判据仍归 kernel**：多副本形态的抢占/租约/接管代次走
  `taskboard`，工具副作用幂等走 `sideeffect`，预算走 `roundguard`，
  跨副本事件走 `eventbus`——本仓只做接线，不另写第二套判据。
- **前提要写清，不吹「绝对不重复」**：跨副本接管重跑要靠**业务幂等键**
  （`pipeline.Tool.IdempotencyKey`，宿主按业务语义给出如订单号）才不重复
  施加副作用；未声明时沿用 `toolCallID`，跨副本这一步依赖模型给出稳定的
  `tool_call_id`。文案写「能力 + 前提 + 证据」，不写没兑现的自白。

## 会话操作纪律（腐化病防线，2026-11-23 bianque 实录移植）

AI 编码代理在本仓工作时执行本节纪律。病灶=长会话中模型输出的**腐化病族**：
解说层高密重复短句/错字突发/token 拼接伪影（PageTurn 类）是**前兆签名**；
升级病相=解说词渗入工具调用参数（路径撕裂/引号崩坏/随机注入）；**最重病相=渗盘**
——工具报「成功」但垃圾落进文件。实录：2026-11-23 bianque 批656，一次 Edit
报成功但 new_string 带垃圾落盘（`manager.go:22`），靠 diff+Read 双读盘才实锤。

- **「报成功」≠安全**：任何写面工具返回成功后，必须用**独立读路径**核盘
  （git diff / 重新 Read = 磁盘真值）；写面自述与结果侧写都不算证据。
- **前兆即收**：解说出现高密重复/错字/拼接伪影，立刻收解说、只发调用；
  病发条件=长轮次+多调用块（实录中每块恰有一个调用被污染）——**单调用块
  全程零腐化**。
- **照抄已验证源**：工具参数里的路径/字符串一律照抄已验证源（成功命令的
  输出、Read/diff 的回显），不凭记忆重组。
- **渗盘即洗**：发现落盘垃圾且会话前树干净时，`git checkout -- <file>`
  回退洗盘，再干净重做；垃圾是未终结字符串时**活不过 build 闸**——
  `go build ./...` 是兜底安全网，提交前必跑。

## 工程约定

- 错误处理用 `github.com/oekit/pkg/errors`（New/Newf/Wrap），不混 stdlib errors。
- goroutine 走 `oekit/concurrency/async.GoSafe`，不裸 `go func`。
- 测试夹具用合成数据；fake model 的 errAt 语义：-1 = 不注入错误，
  >= 0 = 该次调用报错（零值 0 会在首次调用报错——不设就是注入，见
  本仓实录的两次踩坑）。
- `go test ./... -count=1` 提交前必须全绿；PG 判据用例（`pgstore/`）在无 `GAK_PGREF_DSN` 时显式 skip，不伪造结果。
- `pgstore/` 与 `sqliteadapter/` 是 kernel 迁移来的参考实现：对 kernel 的 import 只允许契约面（checkpoint/controlplane/eventbus/sideeffect/sidetest/taskboard/tenant），判据本体归 kernel，本仓不另写。
- 站点部署在**子路径**（base=`/goagent-runtime/`）：**正文 markdown 链接必须自带 base 前缀**。
  markdown 链接被 markdown-it 输出成原生 `<a href>`，框架不加工它 ⇒ 写成 `/guide/x.html`
  会被浏览器解析到站点根之外 ⇒ **404**（实录 2026-11-23：首页「接下来」三条链接全 404）。
  导航/侧栏/hero 按钮走的是**路由链接**（框架运行时加 base），写法相反：必须是纯路由路径，
  以 `/` 开头且**不含** base。判据：`site/scripts/check-links.mjs`（正文链接自带 base + 目标
  页面存在；路由链接为纯路由路径；base 解析不到即判红）——已前置到 `site` 的 build（Pages
  因此不可能再发出 404 链接），并在 ci.yml 有 `site-links` 作业。
- 站点的「机器事实 / 门禁体系 / 坑速查」三页是**生成物**：由
  `site/scripts/gen_reference_pages.mjs` 从仓库事实生成（磁盘上的包目录 / go.mod /
  Makefile / ci.yml / `site/data/pitfalls.json`）。**手改生成物会被 CI 判红**（生成器的
  `--check` 模式）；要改事实改源头文件后跑 `npm run gen:reference`。
  坑表数据文件里的 kernel 地板版本与 go.mod 对账，对不上即红。
  CI 作业：`site`（生成页同步 + 站点构建）。
