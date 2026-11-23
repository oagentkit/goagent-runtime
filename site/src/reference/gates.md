---
title: 门禁体系
---

# 门禁体系：判据在 CI 里，不在自觉

::: warning 本页由 `site/scripts/gen_reference_pages.mjs` 生成，勿手改
作业清单与命令取自 `.github/workflows/ci.yml`，本地目标取自 `Makefile`；
每次 `npm run build` 自动重新生成。
:::

## CI 作业（5 个）

| 作业 | 名称 | 判据 | 外部服务 |
|---|---|---|---|
| `test` | （无） | 准备 actions/checkout@v4<br>准备 actions/setup-go@v5<br>Vet → `go vet ./...`<br>Build → `CGO_ENABLED=0 go build ./...`<br>Test → `go test ./... -count=1 -timeout 120s -coverprofile=coverage.out`<br>Coverage → `go tool cover -func=coverage.out | tail -1`<br>依赖面门禁（纯判据包不得链数据库驱动） → `bash scripts/check-dep-edges.sh` | 无 |
| `integration` | integration（PG 参考实现：kernel 共享契约套件 + SIGKILL e2e） | 准备 actions/checkout@v7<br>准备 actions/setup-go@v7<br>PG 契约套件 + 故障恢复 e2e（硬失败，无环境开洞；判据本体归 kernel sidetest/boardtest） → `CGO_ENABLED=0 go test ./pgstore/ -count=1 -timeout 15m -v`（env: `GAK_PGREF_DSN`）<br>cluster 高可用集成（真 PG：双副本抢占 / 崩溃接管不重复施加） → `CGO_ENABLED=0 go test ./cluster/ -count=1 -timeout 15m -v`（env: `GAK_PGREF_DSN`） | postgres（postgres:16-alpine） |
| `site-links` | 站点内部链接门禁（正文链接自带 base + 目标页面存在） | 准备 actions/checkout@v4<br>准备 actions/setup-node@v4<br>内部链接检查（子路径部署下的 404 防线） → `node site/scripts/check-links.mjs` | 无 |
| `changelog` | CHANGELOG 同步校验（与 git 历史对账） | 准备 actions/checkout@v4<br>取家族工具（钉住版本） → `git clone --quiet https://github.com/oagentkit/matrix-tools.git /tmp/matrix-tools ; cd /tmp/matrix-tools ; git checkout --quiet efa5dea012ce9b7843cda282eb74cf712099d76a`<br>CHANGELOG 与 git 历史对账 → `python3 /tmp/matrix-tools/scripts/gen_changelogs.py --repo goagent-runtime --root "$GITHUB_WORKSPACE/.." --check` | 无 |
| `site` | 站点构建（生成页同步 + 链接门禁 + 构建） | 准备 actions/checkout@v4<br>准备 actions/setup-node@v4<br>生成页同步校验（生成物与仓库事实不同步即红） → `node site/scripts/gen_reference_pages.mjs --check`<br>站点构建（含内部链接门禁） → `npm ci && npm run build` | 无 |

> 判据的**本体**不在本仓：存储语义跑 `goagent-kernel` 的共享契约套件（`sidetest`），抢占 / 租约 / 接管代次跑 `taskboard` 契约，幂等闸跑 `sideeffect`。本仓只做接线，不另写第二套判据。

## 本地等价命令（Makefile）

| 目标 | 依赖 | 命令 |
|---|---|---|
| `check` | `vet test deps` | （组合目标） |
| `vet` | — | `go vet ./...` |
| `test` | — | `go test ./... -count=1 -timeout 120s` |
| `build` | — | `CGO_ENABLED=0 go build ./...` |
| `deps` | — | `bash scripts/check-dep-edges.sh` |
| `demo` | — | `CGO_ENABLED=0 go run ./examples/embedded-demo`<br>`CGO_ENABLED=0 go run ./examples/standalone-demo` |
