// k8sjobconfig.go —— K8s Job 后端的装配面：选项、in-cluster 凭据解析与 HTTP 客户端。
//
// 语义（四件兑现 / Egress 边界 / 状态对账）在 k8sjob.go，传输与载荷在
// k8sjobwire.go。拆分只为满足单文件 400 行阈值：宿主改造接法时先读 k8sjob.go。
package containerbackend

import (
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/oekit/pkg/errors"
)

const (
	backendK8sJobID = "k8s-job"

	defaultK8sEndpoint     = "https://kubernetes.default.svc"
	defaultK8sNamespace    = "default"
	defaultK8sPollInterval = 2 * time.Second
	defaultK8sOrphanGrace  = 30 * time.Second
	defaultK8sTTLSeconds   = int32(300)

	serviceAccountDir       = "/var/run/secrets/kubernetes.io/serviceaccount"
	defaultK8sTokenFile     = serviceAccountDir + "/token"
	defaultK8sCAFile        = serviceAccountDir + "/ca.crt"
	defaultK8sNamespaceFile = serviceAccountDir + "/namespace"
)

// K8sJobOption K8s 后端的装配项。
type K8sJobOption func(*K8sJob)

// WithK8sEndpoint 覆盖 apiserver 端点（http:// 或 https://；测试以 httptest
// 注入假 apiserver）。缺省按 in-cluster 环境变量解析，再不济落到集群内 DNS。
func WithK8sEndpoint(raw string) K8sJobOption {
	return func(k *K8sJob) { k.endpoint = raw }
}

// WithK8sNamespace 指定作业 Namespace（缺省：service account namespace 文件，
// 读不到则 "default"）。Namespace 是隔离边界，生产装配应显式给出专用命名空间。
func WithK8sNamespace(ns string) K8sJobOption {
	return func(k *K8sJob) { k.namespace = ns }
}

// WithK8sServiceAccount 指定 Pod 使用的 ServiceAccount 名（缺省 = 命名空间的
// "default" SA）。最小权限 Role/RoleBinding 由宿主在集群侧准备——本后端只做
// 名字注入，不越界创建 RBAC 资产。
func WithK8sServiceAccount(name string) K8sJobOption {
	return func(k *K8sJob) { k.serviceAccount = name }
}

// WithK8sID 覆盖后端标识（多套集群并存时区分登记）。
func WithK8sID(id string) K8sJobOption {
	return func(k *K8sJob) { k.id = id }
}

// WithK8sTokenFile 显式指定 bearer token 文件（每请求重读——token 会轮换）。
// 缺省读 in-cluster service account token；文件不存在且未显式指定时按无认证处理。
func WithK8sTokenFile(path string) K8sJobOption {
	return func(k *K8sJob) { k.tokenFile, k.tokenExplicit = path, true }
}

// WithK8sCAFile 显式指定 apiserver CA 文件。缺省读 in-cluster CA；显式指定但
// 读不到按装配错误处理（不静默退回系统根证书）。
func WithK8sCAFile(path string) K8sJobOption {
	return func(k *K8sJob) { k.caFile, k.caExplicit = path, true }
}

// WithK8sPollInterval 覆盖状态轮询间隔（缺省 2s）。
func WithK8sPollInterval(d time.Duration) K8sJobOption {
	return func(k *K8sJob) { k.pollInterval = d }
}

// WithK8sOrphanGrace 覆盖「Job 在、Pod 一直不出现」的判定宽限（缺省 30s）。
// 超过宽限仍无 Pod 即给未知结论——提交后 controller 建 Pod 有正常延迟，
// 宽限只是为了不把这段正常延迟误判成失联。
func WithK8sOrphanGrace(d time.Duration) K8sJobOption {
	return func(k *K8sJob) { k.orphanGrace = d }
}

// WithK8sTTLSeconds 覆盖 spec.ttlSecondsAfterFinished（缺省 300s）。TTL 是
// 进程崩溃/对账中断时控制面兜底清理的最后一道；正常路径 Exec 结束即删 Job。
func WithK8sTTLSeconds(sec int32) K8sJobOption {
	return func(k *K8sJob) { k.ttlSeconds = sec }
}

// WithK8sNetworkPolicy 控制是否逐作业创建 Egress 拒绝策略（缺省 true）。
//
// 集群 CNI 不实现 NetworkPolicy 时必须显式关闭：关闭后 Egress() 如实降为
// EgressUnrestricted，要求禁网的 Spec 不会选中它——不静默假报。开启时禁网
// 档的声明还要求能力事实（WithK8sCNICapability）与出站探测
// （WithK8sEgressProbe）齐备，见 k8sjobegress.go 的声明面门禁。
func WithK8sNetworkPolicy(enabled bool) K8sJobOption {
	return func(k *K8sJob) { k.networkPolicy = enabled }
}

// WithK8sExecutionNamespace 禁网任务的独立执行命名空间：策略装配开启时，
// 作业整体（Job / NetworkPolicy / Pod / 日志 / 清理）落在此处，与业务命名
// 空间的策略面隔离开。未装配时退回 WithK8sNamespace 的命名空间——严格策略
// 装配（叠加语义对账）在任何命名空间都执行，独立命名空间是让对账便宜且
// 可预期的结构前提。
func WithK8sExecutionNamespace(ns string) K8sJobOption {
	return func(k *K8sJob) { k.execNamespace = ns }
}

// WithK8sCNICapability 注入 CNI 能力探测：回答「本集群 CNI 是否实现
// NetworkPolicy 数据面」（issue #262 的装配期事实）。这是禁网档声明面的
// 必要条件——答不上来（未注入 / 报错）或不实现，Egress() 都降级为
// EgressUnrestricted，要求禁网的 Spec 不会选中本后端（fail-closed）。
func WithK8sCNICapability(cap K8sCNICapability) K8sJobOption {
	return func(k *K8sJob) { k.cniCap = cap }
}

// WithK8sEgressProbe 注入出站探测（禁网作业的 init 容器）：作业启动即尝试
// 访问外部地址，探测约定见 K8sEgressProbe。没有实测面的禁网声明不许存在：
// 探测未装配时 Egress() 降级为 EgressUnrestricted；探测已装配时实测结果写进
// Result.Detail["egress_verified"]，探测未通过 ⇒ 作业被拒（Pod 到不了业务容器）。
func WithK8sEgressProbe(p K8sEgressProbe) K8sJobOption {
	return func(k *K8sJob) { k.probe = p }
}

// WithK8sMountPolicy 装配挂载白名单（issue #187 的 K8s 落点）。缺省零值 =
// 拒绝一切挂载；job.Mounts 映射为 hostPath 卷（来源是节点本地路径）。
func WithK8sMountPolicy(p MountPolicy) K8sJobOption {
	return func(k *K8sJob) { k.mounts = p }
}

// NewK8sJob 构造 Kubernetes Job 后端。
func NewK8sJob(opts ...K8sJobOption) *K8sJob {
	k := &K8sJob{
		id:            backendK8sJobID,
		tokenFile:     defaultK8sTokenFile,
		caFile:        defaultK8sCAFile,
		pollInterval:  defaultK8sPollInterval,
		orphanGrace:   defaultK8sOrphanGrace,
		ttlSeconds:    defaultK8sTTLSeconds,
		networkPolicy: true,
	}
	for _, o := range opts {
		o(k)
	}
	if k.endpoint == "" {
		k.endpoint = inClusterK8sEndpoint()
	}
	k.endpoint = strings.TrimRight(k.endpoint, "/")
	if k.namespace == "" {
		k.namespace = inClusterK8sNamespace()
	}
	if k.pollInterval <= 0 {
		k.pollInterval = defaultK8sPollInterval
	}
	k.client, k.cfgErr = k.buildClient()
	return k
}

// inClusterK8sEndpoint 按 Downward API 注入的环境变量拼 in-cluster 端点。
func inClusterK8sEndpoint() string {
	host := os.Getenv("KUBERNETES_SERVICE_HOST")
	port := os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return defaultK8sEndpoint
	}
	return "https://" + net.JoinHostPort(host, port)
}

func inClusterK8sNamespace() string {
	b, err := os.ReadFile(defaultK8sNamespaceFile)
	if err != nil {
		return defaultK8sNamespace
	}
	if ns := strings.TrimSpace(string(b)); ns != "" {
		return ns
	}
	return defaultK8sNamespace
}

// buildClient 按端点与 CA 装配 HTTP 客户端（非 https 端点不需要 TLS 配置）。
func (k *K8sJob) buildClient() (*http.Client, error) {
	if !strings.HasPrefix(k.endpoint, "https://") {
		return &http.Client{}, nil
	}
	tr := &http.Transport{}
	if k.caFile != "" {
		pem, err := os.ReadFile(k.caFile)
		switch {
		case err == nil:
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(pem) {
				return nil, errors.New("containerbackend: CA 文件不含可用证书: " + k.caFile)
			}
			tr.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
		case os.IsNotExist(err) && !k.caExplicit:
			// 非 in-cluster 且未显式装配 CA：走系统根证书。
		default:
			return nil, errors.New("containerbackend: 读取 apiserver CA 失败: " + err.Error())
		}
	}
	return &http.Client{Transport: tr}, nil
}
