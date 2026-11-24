---
title: 门禁体系
---

# 门禁体系：判据在 CI 里，不在自觉

::: warning 本页由 `site/scripts/gen_reference_pages.mjs` 生成，勿手改
作业清单与命令取自 `.github/workflows/ci.yml`，本地目标取自 `Makefile`；
每次 `npm run build` 自动重新生成。
:::

## CI 作业（6 个）

| 作业 | 名称 | 判据 | 外部服务 |
|---|---|---|---|
| `test` | （无） | 准备 actions/checkout@v4<br>准备 actions/setup-go@v5<br>Vet → `go vet ./...`<br>Build → `CGO_ENABLED=0 go build ./...`<br>Test → `go test ./... -count=1 -timeout 120s -coverprofile=coverage.out`<br>Coverage → `go tool cover -func=coverage.out | tail -1`<br>依赖面门禁（纯判据包不得链数据库驱动） → `bash scripts/check-dep-edges.sh` | 无 |
| `race` | race（并发面竞态检测） | 准备 actions/checkout@v4<br>准备 actions/setup-go@v5<br>Race → `CGO_ENABLED=1 go test -race ./... -count=1 -timeout 600s` | 无 |
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
| `race` | — | `CGO_ENABLED=1 go test -race ./... -count=1 -timeout 600s` |
| `build` | — | `CGO_ENABLED=0 go build ./...` |
| `deps` | — | `bash scripts/check-dep-edges.sh` |
| `demo` | — | `CGO_ENABLED=0 go run ./examples/embedded-demo`<br>`CGO_ENABLED=0 go run ./examples/standalone-demo` |
| `k8s-e2e` | — | `@command -v docker >/dev/null 2>&1 || { \`<br>`echo "docker 未安装：真集群验收要起 kind 集群（数据面行为没有替身可证）。"; exit 1; }`<br>`@command -v kind >/dev/null 2>&1 || { \`<br>`echo "kind 未安装：go install sigs.k8s.io/kind@latest"; exit 1; }`<br>`@command -v kubectl >/dev/null 2>&1 || { \`<br>`echo "kubectl 未安装：brew install kubectl"; exit 1; }`<br>`@mkdir -p $(K8S_E2E_WORKDIR)`<br>`@set -e; \`<br>`prev_ctx=$$(kubectl config current-context 2>/dev/null || true); \`<br>`trap 'if [ "$(K8S_E2E_KEEP)" = "1" ]; then echo "▸ 保留集群 $(K8S_E2E_CLUSTER)"; else kind delete cluster --name $(K8S_E2E_CLUSTER) >/dev/null 2>&1 || true; fi; if [ -n "$$prev_ctx" ]; then kubectl config use-context "$$prev_ctx" >/dev/null 2>&1 || true; fi' EXIT; \`<br>`echo "▸ 构建探测镜像（非 root + shell 入口点：作业 Pod 级 runAsNonRoot 必需）"; \`<br>`docker build -q --provenance=false --sbom=false -t $(K8S_E2E_PROBE_IMAGE) -f containerbackend/e2e/Dockerfile.probe containerbackend/e2e; \`<br>`echo "▸ 起 kind 集群 $(K8S_E2E_CLUSTER)（关自带 CNI：kindnet 不实现 NetworkPolicy）"; \`<br>`kind create cluster --name $(K8S_E2E_CLUSTER) --image $(K8S_E2E_NODE_IMAGE) --config containerbackend/e2e/kind-cluster.yaml; \`<br>`kind export kubeconfig --name $(K8S_E2E_CLUSTER) >/dev/null; \`<br>`for i in $$(seq 1 90); do kubectl get nodes >/dev/null 2>&1 && break; sleep 2; done; \`<br>`kubectl get nodes >/dev/null || { echo "apiserver 180s 内没起来（kind 集群起失败）"; exit 1; }; \`<br>`echo "▸ 预载探测镜像进节点（本地构建，仓库里没有 ⇒ 必须预载）"; \`<br>`kind load docker-image $(K8S_E2E_PROBE_IMAGE) --name $(K8S_E2E_CLUSTER) || exit 1; \`<br>`echo "▸ 预载 Calico 镜像进节点（节点不能自拉时必需；预载失败不致命——节点若能访问镜像仓库，kubelet 会自拉）"; \`<br>`node_arch=$$(docker exec $(K8S_E2E_CLUSTER)-control-plane uname -m | sed -e 's/x86_64/amd64/' -e 's/aarch64/arm64/'); \`<br>`for img in node cni kube-controllers; do \`<br>`ref=calico/$$img:$(K8S_E2E_CALICO_VERSION); \`<br>`docker pull -q $$ref >/dev/null 2>&1 || true; \`<br>`if kind load docker-image $$ref --name $(K8S_E2E_CLUSTER) >/dev/null 2>&1; then \`<br>`echo "  已预载 $$ref（kind load）"; \`<br>`else \`<br>`echo "  kind load 失败（多架构镜像在 containerd 存储下常见）⇒ 改走单架构导出 + ctr 导入"; \`<br>`docker save --platform linux/$$node_arch $$ref -o $(K8S_E2E_WORKDIR)/calico-$$img.tar || exit 1; \`<br>`docker exec --privileged -i $(K8S_E2E_CLUSTER)-control-plane ctr --namespace=k8s.io images import --all-platforms --digests --snapshotter=overlayfs - < $(K8S_E2E_WORKDIR)/calico-$$img.tar >/dev/null || exit 1; \`<br>`echo "  已预载 $$ref（ctr 导入）"; \`<br>`fi; \`<br>`done; \`<br>`echo "▸ 装 Calico（NetworkPolicy 数据面）"; \`<br>`if [ ! -s $(K8S_E2E_WORKDIR)/calico.yaml ]; then \`<br>`curl -fsSL --retry 3 --retry-all-errors --retry-delay 2 -o $(K8S_E2E_WORKDIR)/calico.yaml https://raw.githubusercontent.com/projectcalico/calico/$(K8S_E2E_CALICO_VERSION)/manifests/calico.yaml || { \`<br>`echo "拉取 Calico 清单失败（网络不通？）：https://raw.githubusercontent.com/projectcalico/calico/$(K8S_E2E_CALICO_VERSION)/manifests/calico.yaml"; exit 1; }; \`<br>`else \`<br>`echo "  复用已下载的 Calico 清单 $(K8S_E2E_WORKDIR)/calico.yaml"; \`<br>`fi; \`<br>`kubectl apply -f $(K8S_E2E_WORKDIR)/calico.yaml; \`<br>`kubectl wait --for=condition=Ready node --all --timeout=300s || { \`<br>`echo "节点没能在 300s 内 Ready ⇒ Calico 数据面没起来（节点不能自拉镜像？预载失败？）"; \`<br>`kubectl -n kube-system get pods -o wide 2>&1 | head -20; exit 1; }; \`<br>`echo "▸ 节点已 Ready（NetworkPolicy 数据面生效）"; \`<br>`echo "▸ 准备执行命名空间与凭据（SA + cluster-admin 绑定 + token + CA）"; \`<br>`kubectl create namespace $(K8S_E2E_NS) 2>/dev/null || true; \`<br>`kubectl -n $(K8S_E2E_NS) create serviceaccount gak-e2e 2>/dev/null || true; \`<br>`kubectl create clusterrolebinding gak-e2e-admin --clusterrole=cluster-admin --serviceaccount=$(K8S_E2E_NS):gak-e2e 2>/dev/null || true; \`<br>`kubectl -n $(K8S_E2E_NS) create token gak-e2e > $(K8S_E2E_WORKDIR)/token; \`<br>`kubectl config view --raw --minify -o jsonpath='{.clusters[0].cluster.certificate-authority-data}' | base64 -d > $(K8S_E2E_WORKDIR)/ca.crt; \`<br>`echo "▸ 跑真集群验收（阳性对照 / 禁网作业 / 叠加语义）"; \`<br>`status=0; \`<br>`GAK_K8S_E2E_ENDPOINT=$$(kubectl config view --minify -o jsonpath='{.clusters[0].cluster.server}') \`<br>`GAK_K8S_E2E_TOKEN_FILE=$(K8S_E2E_WORKDIR)/token \`<br>`GAK_K8S_E2E_CA_FILE=$(K8S_E2E_WORKDIR)/ca.crt \`<br>`GAK_K8S_E2E_NAMESPACE=$(K8S_E2E_NS) \`<br>`GAK_K8S_E2E_PROBE_IMAGE=$(K8S_E2E_PROBE_IMAGE) \`<br>`GAK_K8S_E2E_JOB_IMAGE=$(K8S_E2E_PROBE_IMAGE) \`<br>`CGO_ENABLED=0 go test ./containerbackend/ -run '^TestK8sRealCluster' -v -count=1 -timeout 20m || status=1; \`<br>`if [ $$status -ne 0 ]; then echo "K8s 出站禁网真集群验收未通过（见上方输出）"; exit 1; fi; \`<br>`echo "K8s 出站禁网真集群验收 ✓（阳性对照连上 / 禁网作业连不上 / 叠加语义拒绝创建）"` |
