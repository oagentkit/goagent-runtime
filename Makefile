.PHONY: check test vet build demo deps race

check: vet test deps

vet:
	go vet ./...

test:
	go test ./... -count=1 -timeout 120s

# 竞态门禁：并发面（快照序号、事件缓冲、共享 map）必须过竞态检测。
#
# ★ -race 在 Linux 上**强制要求 cgo**（CGO_ENABLED=0 会直接报
#   「-race requires cgo」）⇒ 这里显式写 CGO_ENABLED=1，别让全局开关
#   把它关掉（那会让本机绿、CI 红的最难查形态）。
#
# 为什么单独一个目标而不是 test 里带 -race：PG 判据套件在 -race 下慢得多，
# 且竞态判据要能**单独复跑**（怀疑竞态时先跑这一条）。
race:
	CGO_ENABLED=1 go test -race ./... -count=1 -timeout 600s

build:
	CGO_ENABLED=0 go build ./...

# 依赖面门禁：纯判据包不得链数据库驱动（issue #10）——
# 驱动边只许待在绑定包（如 storage/sqlitestore），不许进 storage 这类纯判据包。
deps:
	bash scripts/check-dep-edges.sh

demo:
	CGO_ENABLED=0 go run ./examples/embedded-demo
	CGO_ENABLED=0 go run ./examples/standalone-demo

# ── K8s 出站禁网真集群验收（issue #418 的判据体随边界迁移到本仓）──────────
#
# 为什么不在 check 里：它要一台**真的实现了 NetworkPolicy 的集群**——数据面
# 行为没有任何替身可证（httptest 替不出「apiserver 接受策略 ≠ 数据面生效」）。
# check 必须在任何机器上一条命令可跑 ⇒ 这条不进 check；CI 侧同样不进：起集群 +
# 装 Calico 要拉镜像，抖动会连带把别的门禁染红。
#
# 判据本体在 containerbackend/k8sjob_realcluster_test.go：同一台集群、**同一条**
# 探针脚本、**同一个**目标，只在「有没有禁网策略」这一个变量上不同（对照 Pod
# 必须连上、禁网作业必须连不上）⇒ 结论不会被「探针压根不通」冒充。
#
# 这条入口随判据体一起从 goagent-kernel 迁到本仓（那边 examples/ 已整体迁出，
# 留下的目标指向已删除的路径 ⇒ **验收路径一度不可复跑**；kernel 侧已撤掉那个
# 悬空目标，登记面（owner 指向本仓）见 kernel 的 docs/acceptance-paths.yaml）。
#
# 缺 docker / kind / kubectl 任一样都**明确报错退出**，不静默跳过。
# 跑完默认拆集群（K8S_E2E_KEEP=1 保留）。
#
# 复用已有集群：给齐 GAK_K8S_E2E_* 六个环境变量后直接跑
#   go test ./containerbackend/ -run '^TestK8sRealCluster' -v -count=1
K8S_E2E_CLUSTER ?= gak-e2e
K8S_E2E_NS ?= gak-e2e
K8S_E2E_PROBE_IMAGE ?= gak-egress-probe:local
K8S_E2E_CALICO_VERSION ?= v3.30.3
K8S_E2E_NODE_IMAGE ?= kindest/node:v1.31.0
K8S_E2E_WORKDIR ?= $(or $(TMPDIR),/tmp)/goagent-runtime-k8s-e2e
K8S_E2E_KEEP ?= 0

k8s-e2e:
	@command -v docker >/dev/null 2>&1 || { \
	  echo "docker 未安装：真集群验收要起 kind 集群（数据面行为没有替身可证）。"; exit 1; }
	@command -v kind >/dev/null 2>&1 || { \
	  echo "kind 未安装：go install sigs.k8s.io/kind@latest"; exit 1; }
	@command -v kubectl >/dev/null 2>&1 || { \
	  echo "kubectl 未安装：brew install kubectl"; exit 1; }
	@mkdir -p $(K8S_E2E_WORKDIR)
	@set -e; \
	prev_ctx=$$(kubectl config current-context 2>/dev/null || true); \
	trap 'if [ "$(K8S_E2E_KEEP)" = "1" ]; then echo "▸ 保留集群 $(K8S_E2E_CLUSTER)"; else kind delete cluster --name $(K8S_E2E_CLUSTER) >/dev/null 2>&1 || true; fi; if [ -n "$$prev_ctx" ]; then kubectl config use-context "$$prev_ctx" >/dev/null 2>&1 || true; fi' EXIT; \
	echo "▸ 构建探测镜像（非 root + shell 入口点：作业 Pod 级 runAsNonRoot 必需）"; \
	docker build -q --provenance=false --sbom=false -t $(K8S_E2E_PROBE_IMAGE) -f containerbackend/e2e/Dockerfile.probe containerbackend/e2e; \
	echo "▸ 起 kind 集群 $(K8S_E2E_CLUSTER)（关自带 CNI：kindnet 不实现 NetworkPolicy）"; \
	kind create cluster --name $(K8S_E2E_CLUSTER) --image $(K8S_E2E_NODE_IMAGE) --config containerbackend/e2e/kind-cluster.yaml; \
	kind export kubeconfig --name $(K8S_E2E_CLUSTER) >/dev/null; \
	for i in $$(seq 1 90); do kubectl get nodes >/dev/null 2>&1 && break; sleep 2; done; \
	kubectl get nodes >/dev/null || { echo "apiserver 180s 内没起来（kind 集群起失败）"; exit 1; }; \
	echo "▸ 预载探测镜像进节点（本地构建，仓库里没有 ⇒ 必须预载）"; \
	kind load docker-image $(K8S_E2E_PROBE_IMAGE) --name $(K8S_E2E_CLUSTER) || exit 1; \
	echo "▸ 预载 Calico 镜像进节点（节点不能自拉时必需；预载失败不致命——节点若能访问镜像仓库，kubelet 会自拉）"; \
	node_arch=$$(docker exec $(K8S_E2E_CLUSTER)-control-plane uname -m | sed -e 's/x86_64/amd64/' -e 's/aarch64/arm64/'); \
	for img in node cni kube-controllers; do \
	  ref=calico/$$img:$(K8S_E2E_CALICO_VERSION); \
	  docker pull -q $$ref >/dev/null 2>&1 || true; \
	  if kind load docker-image $$ref --name $(K8S_E2E_CLUSTER) >/dev/null 2>&1; then \
	    echo "  已预载 $$ref（kind load）"; \
	  else \
	    echo "  kind load 失败（多架构镜像在 containerd 存储下常见）⇒ 改走单架构导出 + ctr 导入"; \
	    docker save --platform linux/$$node_arch $$ref -o $(K8S_E2E_WORKDIR)/calico-$$img.tar || exit 1; \
	    docker exec --privileged -i $(K8S_E2E_CLUSTER)-control-plane ctr --namespace=k8s.io images import --all-platforms --digests --snapshotter=overlayfs - < $(K8S_E2E_WORKDIR)/calico-$$img.tar >/dev/null || exit 1; \
	    echo "  已预载 $$ref（ctr 导入）"; \
	  fi; \
	done; \
	echo "▸ 装 Calico（NetworkPolicy 数据面）"; \
	if [ ! -s $(K8S_E2E_WORKDIR)/calico.yaml ]; then \
	  curl -fsSL --retry 3 --retry-all-errors --retry-delay 2 -o $(K8S_E2E_WORKDIR)/calico.yaml https://raw.githubusercontent.com/projectcalico/calico/$(K8S_E2E_CALICO_VERSION)/manifests/calico.yaml || { \
	    echo "拉取 Calico 清单失败（网络不通？）：https://raw.githubusercontent.com/projectcalico/calico/$(K8S_E2E_CALICO_VERSION)/manifests/calico.yaml"; exit 1; }; \
	else \
	  echo "  复用已下载的 Calico 清单 $(K8S_E2E_WORKDIR)/calico.yaml"; \
	fi; \
	kubectl apply -f $(K8S_E2E_WORKDIR)/calico.yaml; \
	kubectl wait --for=condition=Ready node --all --timeout=300s || { \
	  echo "节点没能在 300s 内 Ready ⇒ Calico 数据面没起来（节点不能自拉镜像？预载失败？）"; \
	  kubectl -n kube-system get pods -o wide 2>&1 | head -20; exit 1; }; \
	echo "▸ 节点已 Ready（NetworkPolicy 数据面生效）"; \
	echo "▸ 准备执行命名空间与凭据（SA + cluster-admin 绑定 + token + CA）"; \
	kubectl create namespace $(K8S_E2E_NS) 2>/dev/null || true; \
	kubectl -n $(K8S_E2E_NS) create serviceaccount gak-e2e 2>/dev/null || true; \
	kubectl create clusterrolebinding gak-e2e-admin --clusterrole=cluster-admin --serviceaccount=$(K8S_E2E_NS):gak-e2e 2>/dev/null || true; \
	kubectl -n $(K8S_E2E_NS) create token gak-e2e > $(K8S_E2E_WORKDIR)/token; \
	kubectl config view --raw --minify -o jsonpath='{.clusters[0].cluster.certificate-authority-data}' | base64 -d > $(K8S_E2E_WORKDIR)/ca.crt; \
	echo "▸ 跑真集群验收（阳性对照 / 禁网作业 / 叠加语义）"; \
	status=0; \
	GAK_K8S_E2E_ENDPOINT=$$(kubectl config view --minify -o jsonpath='{.clusters[0].cluster.server}') \
	GAK_K8S_E2E_TOKEN_FILE=$(K8S_E2E_WORKDIR)/token \
	GAK_K8S_E2E_CA_FILE=$(K8S_E2E_WORKDIR)/ca.crt \
	GAK_K8S_E2E_NAMESPACE=$(K8S_E2E_NS) \
	GAK_K8S_E2E_PROBE_IMAGE=$(K8S_E2E_PROBE_IMAGE) \
	GAK_K8S_E2E_JOB_IMAGE=$(K8S_E2E_PROBE_IMAGE) \
	CGO_ENABLED=0 go test ./containerbackend/ -run '^TestK8sRealCluster' -v -count=1 -timeout 20m || status=1; \
	if [ $$status -ne 0 ]; then echo "K8s 出站禁网真集群验收未通过（见上方输出）"; exit 1; fi; \
	echo "K8s 出站禁网真集群验收 ✓（阳性对照连上 / 禁网作业连不上 / 叠加语义拒绝创建）"
