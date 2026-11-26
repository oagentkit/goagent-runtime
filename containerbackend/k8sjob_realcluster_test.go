// k8sjob_realcluster_test.go —— issue #262 的真集群验收 + issue #418 的实测结论。
//
// # 跳过是显式声明，不是「观察到但跳过」
//
// 本文件的用例只能在真集群上兑现判定：数据面（CNI 是否真的实现
// NetworkPolicy）没有任何 httptest 替身可以证明。环境不齐时用例显式
// Skipf 并打印所需环境（与 worker/faultmatrix 的 matrixCase.requiresEnv
// 同一条纪律），缺什么写什么，绝不静默跳过。
//
// 所需环境（全部必填，缺一即 Skipf）：
//
//	GAK_K8S_E2E_ENDPOINT     apiserver 端点（https://…）
//	GAK_K8S_E2E_TOKEN_FILE   bearer token 文件路径（每请求重读）
//	GAK_K8S_E2E_CA_FILE      apiserver CA 文件路径
//	GAK_K8S_E2E_NAMESPACE    专用执行命名空间（须已存在且可建 Job/NetworkPolicy）
//	GAK_K8S_E2E_PROBE_IMAGE  出站探测镜像（探测约定：尝试访问外部地址，
//	                         连上=exit 1，被阻断=exit 0）
//	GAK_K8S_E2E_JOB_IMAGE    业务镜像（须含 sh 与 wget/curl，本用例用它复核探测）
//
// 镜像还有两条**硬要求**（少一条作业根本起不来，断言就永远到不了）：
//
//  1. **非 root 运行**：作业 Pod 级带 runAsNonRoot=true，镜像默认用户是 root
//     ⇒ kubelet 直接拒绝启动（"cannot verify user is non-root"）。
//  2. **ENTRYPOINT 是 shell**：业务容器的命令是以 args 形态下发的
//     （"-ec", "<脚本>"），镜像没有 shell 入口点就 exec 不到脚本。
//
// # 判定面：同一条命令，只在「有没有禁网策略」这一个变量上不同
//
// 探测目标若写成 https://…，结论**不成立**：探针脚本是
// `if wget …; then …; else …; fi`，而 busybox wget 不支持 HTTPS（alpine 的
// busybox 未编 TLS）⇒「wget 失败」既可能是被 NetworkPolicy 拦下，也可能是
// wget 压根不会说 HTTPS ⇒ **禁网没生效时用例照样绿**（假绿）。
//
// 本版把判据改成**有阳性对照**的形态：同一台集群、**同一条**探针脚本、
// **同一个**目标（集群内见证端点的数字 IP），唯一的变量是那张 NetworkPolicy：
//
//   - 对照 Pod（无策略）：必须连上 ⇒ 脚本打印 EGRESS_OPEN、退出码 1；
//   - 禁网作业（有策略）：必须连不上 ⇒ 脚本打印 EGRESS_BLOCKED、退出码 0。
//
// 两个方向都断言 ⇒「绿灯」不再可能由「探针压根不通」冒充。
//
// 目标用**数字 IP**（见证端点的 Pod IP）而不是域名：域名解析本身也是出站，
// 被拦时失败落在 DNS 层 ⇒「连不上」与「解析不了」分不开，两种失败不可区分。
package containerbackend

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	kerrors "github.com/oekit/pkg/errors"

	"github.com/oagentkit/guardx/sandbox"
	"github.com/yi-nology/goagent-kernel/backend"
)

const (
	envK8sE2EEndpoint   = "GAK_K8S_E2E_ENDPOINT"
	envK8sE2ETokenFile  = "GAK_K8S_E2E_TOKEN_FILE"
	envK8sE2ECAFile     = "GAK_K8S_E2E_CA_FILE"
	envK8sE2ENamespace  = "GAK_K8S_E2E_NAMESPACE"
	envK8sE2EProbeImage = "GAK_K8S_E2E_PROBE_IMAGE"
	envK8sE2EJobImage   = "GAK_K8S_E2E_JOB_IMAGE"
)

// k8sE2EEnvNames 环境清单：Skipf 文案与逐项检查共用，缺什么打印什么。
var k8sE2EEnvNames = []string{
	envK8sE2EEndpoint, envK8sE2ETokenFile, envK8sE2ECAFile,
	envK8sE2ENamespace, envK8sE2EProbeImage, envK8sE2EJobImage,
}

func k8sE2EEnv(t *testing.T) map[string]string {
	t.Helper()
	env := map[string]string{}
	var missing []string
	for _, name := range k8sE2EEnvNames {
		v := os.Getenv(name)
		if v == "" {
			missing = append(missing, name)
			continue
		}
		env[name] = v
	}
	if len(missing) > 0 {
		t.Skipf("本用例只能真集群验证（数据面行为没有替身可证），需要环境：%s；缺：%s",
			strings.Join(k8sE2EEnvNames, ", "), strings.Join(missing, ", "))
	}
	return env
}

// k8sE2EBackend 造一个指向真集群的后端：独立执行命名空间 + CNI 能力事实为真
// （跑在已确认实现 NetworkPolicy 的集群上）。
//
// script 非空时装配出站探测（探测镜像与脚本由宿主注入，见 K8sEgressProbe 约定：
// exit 0 = 出站尝试失败 ⇒ 禁网实测成立；exit ≠ 0 = 连上了或不可判 ⇒ init 失败）。
func k8sE2EBackend(env map[string]string, script string) *K8sJob {
	opts := []K8sJobOption{
		WithK8sEndpoint(env[envK8sE2EEndpoint]),
		WithK8sNamespace(env[envK8sE2ENamespace]),
		WithK8sExecutionNamespace(env[envK8sE2ENamespace]),
		WithK8sTokenFile(env[envK8sE2ETokenFile]),
		WithK8sCAFile(env[envK8sE2ECAFile]),
		WithK8sCNICapability(k8sE2ECapability{}),
		WithK8sPollInterval(2 * time.Second),
		// 孤儿宽限是「Job 认为有 Pod、集群里却一个都没有」的判定窗口（缺省 30s，
		// 那是给生产热路径的预算）。真集群上 kube-controller-manager 首次调度
		// 可能明显慢（本机实测见过 apiserver TLS 握手超时），30s 判成 no_pods
		// 会把「机器面慢」误报成「机制不兑现」——这里放宽到 5 分钟。
		WithK8sOrphanGrace(5 * time.Minute),
	}
	if script != "" {
		opts = append(opts, WithK8sEgressProbe(K8sEgressProbe{
			Image:   env[envK8sE2EProbeImage],
			Command: []string{"/bin/sh", "-ec", script},
		}))
	}
	return NewK8sJob(opts...)
}

// k8sE2EProbeScript 探针脚本：**唯一一份**，对照 Pod 与禁网作业都用它。
//
// 脚本同时打印 EGRESS_OPEN / EGRESS_BLOCKED，让结论落在可读的实测证据上，
// 而不只落在退出码上。
func k8sE2EProbeScript(target string) string {
	return fmt.Sprintf(
		"if wget -T 5 -q -O /dev/null %s; then echo EGRESS_OPEN; exit 1; else echo EGRESS_BLOCKED; exit 0; fi",
		target)
}

// TestK8sRealClusterEgressDenied 验收标准 1：真集群上禁网 Job 的出站尝试
// **必须失败**（探测被阻断 ⇒ 作业放行；作业内的出站尝试也必须失败），
// 且业务容器自身要能跑完（探测失败不放行是对的，但业务本身要能跑）。
//
// 用例含**阳性对照**：同一脚本、同一目标，在无策略的对照 Pod 上必须连上
// （EGRESS_OPEN / 退出码 1）——否则「被阻断」只是「探针压根不通」的同义词。
func TestK8sRealClusterEgressDenied(t *testing.T) {
	env := k8sE2EEnv(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	k := k8sE2EBackend(env, "")

	// 见证端点：集群内一个真在服务的 HTTP 端点，探针打的就是它。
	target := k8sE2EStartWitness(ctx, t, k, env)
	script := k8sE2EProbeScript(target)

	// 阳性对照：无禁网策略的 Pod 跑**同一条**脚本 ⇒ 必须连上。
	k8sE2ERunControl(ctx, t, k, env, script)

	// 被测：禁网作业（Egress=denied）跑同一条脚本 ⇒ 必须连不上。
	k8sE2ERunDenied(ctx, t, env, script)
}

// TestK8sRealClusterEgressStacking 验收标准 2：同命名空间内先放一条允许出站的
// 策略 ⇒ 禁网 Job 必须被拒绝创建（叠加语义被处理掉），而不是带病执行。
func TestK8sRealClusterEgressStacking(t *testing.T) {
	env := k8sE2EEnv(t)
	k := k8sE2EBackend(env, "")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	// 同一后端的传输面允许用例自建/自删叠加策略（真集群，不引第三方依赖）。
	allow := `{"apiVersion":"networking.k8s.io/v1","kind":"NetworkPolicy",` +
		`"metadata":{"name":"e2e-allow-all-stacking"},` +
		`"spec":{"podSelector":{},"policyTypes":["Egress"],"egress":[{}]}}`
	if err := k8sE2ECreateRawPolicy(ctx, k, env[envK8sE2ENamespace], allow); err != nil {
		t.Fatalf("准备叠加策略失败（检查 RBAC：create/get/delete networkpolicies）：%v", err)
	}
	defer k8sE2EDeleteRawPolicy(ctx, k, env[envK8sE2ENamespace], "e2e-allow-all-stacking")

	_, err := k.Exec(ctx, backend.Job{Image: env[envK8sE2EJobImage]})
	if !kerrors.Is(err, ErrNetworkPolicy) {
		t.Fatalf("存在允许出站的叠加策略时必须拒绝创建禁网作业：%v", err)
	}
}

// k8sE2ECapability 真集群用例的能力事实：跑在已确认实现 NetworkPolicy 的
// 集群上（e2e 集群的选型前提），事实由装配方显式给出。
type k8sE2ECapability struct{}

func (k8sE2ECapability) NetworkPolicyEnforced() (bool, error) { return true, nil }

// ── 见证端点 / 对照 Pod（阳性对照）──────────────────────────────────

// k8sE2EStartWitness 起一个集群内的 HTTP 见证端点，返回它的数字 IP。
//
// 为什么要有见证端点：探测目标若指向集群外地址，「连不上」可能是外网不可达；
// 指向集群内地址，「连不上」就只能是这张 NetworkPolicy 拦的（对照 Pod 同样
// 连得上 ⇒ 变量只剩策略）。
func k8sE2EStartWitness(ctx context.Context, t *testing.T, k *K8sJob, env map[string]string) string {
	t.Helper()
	const name = "gak-e2e-witness"
	_ = k8sE2EDeletePod(ctx, k, env[envK8sE2ENamespace], name) // 幂等：上一轮残留先清
	// 见证端点用 busybox nc 手写一个 HTTP 200 响应：alpine 的 busybox **没有**
	// httpd applet（只有 nc / wget / nslookup），而见证端点必须与探测镜像同源
	// （镜像由宿主注入，验收不该额外要求第三个镜像）。
	script := `mkdir -p /tmp/www && echo witness > /tmp/www/index.html && while true; do printf 'HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: 8\r\n\r\nwitness\n' | busybox nc -l -p 8080; done`
	if err := k8sE2ECreatePod(ctx, k, env[envK8sE2ENamespace], name, env[envK8sE2EProbeImage], script); err != nil {
		t.Fatalf("起见证端点失败（检查 RBAC：create/delete pods）：%v", err)
	}
	t.Cleanup(func() { _ = k8sE2EDeletePod(context.Background(), k, env[envK8sE2ENamespace], name) })

	pod := k8sE2EWaitPodRunning(ctx, t, k, env[envK8sE2ENamespace], name, 3*time.Minute)
	if pod.Status.PodIP == "" {
		t.Fatalf("见证端点没有拿到 Pod IP（phase=%s）", pod.Status.Phase)
	}
	target := fmt.Sprintf("http://%s:8080/", pod.Status.PodIP)
	t.Logf("见证端点：pod=%s ip=%s target=%s", name, pod.Status.PodIP, target)
	return target
}

// k8sE2ERunControl 阳性对照：无禁网策略的 Pod 跑同一条探针脚本，必须连上。
//
// 断言两个方向：退出码 1（探针约定「连上 ⇒ 失败」）且日志打印 EGRESS_OPEN。
// 少了这一条，被测侧的「EGRESS_BLOCKED」就可能只是「探针压根不通」的同义词。
//
// 允许重试：见证端点的 httpd 刚起时可能还没监听 ⇒ 重试几次再判失败；
// 但**只重试对照侧**（对照失败 ⇒ 见证端点或网络有问题，与被测侧无关）。
func k8sE2ERunControl(ctx context.Context, t *testing.T, k *K8sJob, env map[string]string, script string) {
	t.Helper()
	const name = "gak-e2e-control"
	var last string
	for attempt := 1; attempt <= 3; attempt++ {
		_ = k8sE2EDeletePod(ctx, k, env[envK8sE2ENamespace], name)
		if err := k8sE2ECreatePod(ctx, k, env[envK8sE2ENamespace], name, env[envK8sE2EProbeImage], script); err != nil {
			t.Fatalf("起对照 Pod 失败（检查 RBAC：create/delete pods）：%v", err)
		}
		pod := k8sE2EWaitPodTerminal(ctx, t, k, env[envK8sE2ENamespace], name, 3*time.Minute)
		logs := k8sE2ELogs(ctx, t, k, env[envK8sE2ENamespace], name)
		code, ok := k8sE2EExitCode(pod, "main")
		if ok && code == 1 && strings.Contains(logs, "EGRESS_OPEN") {
			t.Logf("阳性对照（无禁网策略）：退出码=%d logs=%q ⇒ 出站可达", code, strings.TrimSpace(logs))
			_ = k8sE2EDeletePod(ctx, k, env[envK8sE2ENamespace], name)
			return
		}
		last = fmt.Sprintf("第 %d 次：退出码=%d(可读=%v) logs=%q", attempt, code, ok, logs)
		time.Sleep(3 * time.Second)
	}
	_ = k8sE2EDeletePod(ctx, k, env[envK8sE2ENamespace], name)
	t.Fatalf("阳性对照失败：无禁网策略时探针必须「连上」（退出码 1 + EGRESS_OPEN）——%s；"+
		"见证端点没起来或网络不通 ⇒ 被测侧的「被阻断」不可信", last)
}

// k8sE2ERunDenied 被测：禁网作业跑同一条探针脚本，必须连不上。
func k8sE2ERunDenied(ctx context.Context, t *testing.T, env map[string]string, script string) {
	t.Helper()
	k := k8sE2EBackend(env, script)
	res, err := k.Exec(ctx, backend.Job{
		Image:  env[envK8sE2EJobImage],
		Args:   []string{"-ec", script},
		Budget: sandbox.Budget{MaxMemory: 1 << 26},
	})
	if err != nil {
		t.Fatalf("真集群 Exec 基础设施故障：%v", err)
	}
	if !res.OK {
		t.Fatalf("探测被阻断后业务容器应能跑完自身判定：res=%+v\n现场：%s", res, k8sE2EScene(ctx, k, env))
	}
	if !strings.Contains(res.Stdout, "EGRESS_BLOCKED") || strings.Contains(res.Stdout, "EGRESS_OPEN") {
		t.Fatalf("禁网 Job 的出站尝试必须失败：stdout=%q detail=%+v", res.Stdout, res.Detail)
	}
	if d, ok := res.Detail.(map[string]any); ok && d["egress_verified"] != "probe_exit_0" {
		t.Fatalf("实测面必须记录探测被阻断：%+v", d)
	}
	t.Logf("禁网作业（有禁网策略）：stdout=%q detail=%+v ⇒ 出站被阻断", strings.TrimSpace(res.Stdout), res.Detail)
}
