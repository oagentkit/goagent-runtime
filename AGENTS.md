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
- **cluster 模板未提供是有意为之**：kernel 的控制面原子审计与租约失格
  P0 未修复前，不把多副本形态做成默认路径。

## 工程约定

- 错误处理用 `github.com/oekit/pkg/errors`（New/Newf/Wrap），不混 stdlib errors。
- goroutine 走 `oekit/concurrency/async.GoSafe`，不裸 `go func`。
- 测试夹具用合成数据；fake model 的 errAt 语义：-1 = 不注入错误，
  >= 0 = 该次调用报错（零值 0 会在首次调用报错——不设就是注入，见
  本仓实录的两次踩坑）。
- `go test ./... -count=1` 提交前必须全绿；PG 判据用例（`pgstore/`）在无 `GAK_PGREF_DSN` 时显式 skip，不伪造结果。
- `pgstore/` 与 `sqliteadapter/` 是 kernel 迁移来的参考实现：对 kernel 的 import 只允许契约面（checkpoint/controlplane/eventbus/sideeffect/sidetest/taskboard/tenant），判据本体归 kernel，本仓不另写。
