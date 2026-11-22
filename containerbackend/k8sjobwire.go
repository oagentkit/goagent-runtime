// k8sjobwire.go —— K8s Job 的传输管道与 batch/v1 / networking.k8s.io/v1 载荷。
//
// 拆分只为满足单文件 400 行阈值（make architecture-check）：语义（自报什么、
// 对账判据）在 k8sjob.go，装配面在 k8sjobconfig.go。宿主改造接法时先读 k8sjob.go。
package containerbackend

import (
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/yi-nology/goagent-kernel/backend"
)

const (
	k8sJobLabel                 = "gak-job"
	k8sJobContainerName         = "job"
	k8sEgressProbeContainerName = "egress-probe"
	k8sManagedByLabel           = "app.kubernetes.io/managed-by"
	k8sManagedByValue           = "goagent-kernel"
)

// ── 请求/应答对象（只声明本实现用到的字段）──────────────────────────────

type k8sObjectMeta struct {
	Name              string            `json:"name,omitempty"`
	Namespace         string            `json:"namespace,omitempty"`
	UID               string            `json:"uid,omitempty"`
	Labels            map[string]string `json:"labels,omitempty"`
	DeletionTimestamp *string           `json:"deletionTimestamp,omitempty"`
}

type k8sEnvVar struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type k8sVolumeMount struct {
	Name      string `json:"name"`
	MountPath string `json:"mountPath"`
	ReadOnly  bool   `json:"readOnly,omitempty"`
}

type k8sHostPath struct {
	Path string `json:"path"`
}

type k8sVolume struct {
	Name     string       `json:"name"`
	HostPath *k8sHostPath `json:"hostPath,omitempty"`
}

type k8sCapabilities struct {
	Drop []string `json:"drop,omitempty"`
}

type k8sSeccompProfile struct {
	Type string `json:"type"`
}

type k8sPodSecurityContext struct {
	RunAsNonRoot   bool               `json:"runAsNonRoot"`
	SeccompProfile *k8sSeccompProfile `json:"seccompProfile,omitempty"`
}

type k8sContainerSecurityContext struct {
	AllowPrivilegeEscalation bool             `json:"allowPrivilegeEscalation"`
	ReadOnlyRootFilesystem   bool             `json:"readOnlyRootFilesystem"`
	Capabilities             *k8sCapabilities `json:"capabilities,omitempty"`
}

type k8sResources struct {
	Limits map[string]string `json:"limits,omitempty"`
}

type k8sContainer struct {
	Name            string                      `json:"name"`
	Image           string                      `json:"image"`
	Command         []string                    `json:"command,omitempty"`
	Args            []string                    `json:"args,omitempty"`
	Env             []k8sEnvVar                 `json:"env,omitempty"`
	WorkingDir      string                      `json:"workingDir,omitempty"`
	Resources       k8sResources                `json:"resources"`
	SecurityContext k8sContainerSecurityContext `json:"securityContext"`
	VolumeMounts    []k8sVolumeMount            `json:"volumeMounts,omitempty"`
}

type k8sPodSpec struct {
	RestartPolicy                string                `json:"restartPolicy"`
	ServiceAccountName           string                `json:"serviceAccountName,omitempty"`
	AutomountServiceAccountToken bool                  `json:"automountServiceAccountToken"`
	SecurityContext              k8sPodSecurityContext `json:"securityContext"`
	InitContainers               []k8sContainer        `json:"initContainers,omitempty"`
	Containers                   []k8sContainer        `json:"containers"`
	Volumes                      []k8sVolume           `json:"volumes,omitempty"`
}

type k8sTemplateMeta struct {
	Labels map[string]string `json:"labels,omitempty"`
}

type k8sPodTemplate struct {
	Metadata k8sTemplateMeta `json:"metadata"`
	Spec     k8sPodSpec      `json:"spec"`
}

type k8sJobSpec struct {
	BackoffLimit            int32          `json:"backoffLimit"`
	TTLSecondsAfterFinished int32          `json:"ttlSecondsAfterFinished"`
	ActiveDeadlineSeconds   int64          `json:"activeDeadlineSeconds,omitempty"`
	Template                k8sPodTemplate `json:"template"`
}

type k8sJobObject struct {
	APIVersion string        `json:"apiVersion"`
	Kind       string        `json:"kind"`
	Metadata   k8sObjectMeta `json:"metadata"`
	Spec       k8sJobSpec    `json:"spec"`
}

type k8sJobCondition struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

type k8sJobStatus struct {
	Active     int               `json:"active,omitempty"`
	Succeeded  int               `json:"succeeded,omitempty"`
	Failed     int               `json:"failed,omitempty"`
	Conditions []k8sJobCondition `json:"conditions,omitempty"`
}

type k8sJobResponse struct {
	Metadata k8sObjectMeta `json:"metadata"`
	Status   k8sJobStatus  `json:"status"`
}

type k8sContainerState struct {
	Terminated *k8sTerminated `json:"terminated,omitempty"`
}

type k8sTerminated struct {
	ExitCode int    `json:"exitCode"`
	Reason   string `json:"reason,omitempty"`
	Message  string `json:"message,omitempty"`
}

type k8sContainerStatus struct {
	Name  string            `json:"name"`
	State k8sContainerState `json:"state"`
}

type k8sPodStatus struct {
	Phase                 string               `json:"phase,omitempty"`
	InitContainerStatuses []k8sContainerStatus `json:"initContainerStatuses,omitempty"`
	ContainerStatuses     []k8sContainerStatus `json:"containerStatuses,omitempty"`
}

type k8sPod struct {
	Metadata k8sObjectMeta `json:"metadata"`
	Status   k8sPodStatus  `json:"status"`
}

type k8sPodList struct {
	Items []k8sPod `json:"items"`
}

type k8sLabelSelector struct {
	MatchLabels map[string]string `json:"matchLabels"`
	// MatchExpressions 只为**读取**别人的策略而声明（严格策略装配要判叠加
	// 语义）：本侧不求值表达式，遇到即按「可能选中本作业 Pod」处理
	// （fail-closed）。构造本作业自己的策略时恒空（omitempty 不落载荷）。
	MatchExpressions []any `json:"matchExpressions,omitempty"`
}

type k8sNetworkPolicySpec struct {
	PodSelector k8sLabelSelector `json:"podSelector"`
	PolicyTypes []string         `json:"policyTypes"`
	Egress      []struct{}       `json:"egress"`
}

type k8sNetworkPolicy struct {
	APIVersion string               `json:"apiVersion"`
	Kind       string               `json:"kind"`
	Metadata   k8sObjectMeta        `json:"metadata"`
	Spec       k8sNetworkPolicySpec `json:"spec"`
}

// k8sNetworkPolicyList LIST networkpolicies 的应答（严格策略装配的对账入口）。
type k8sNetworkPolicyList struct {
	Items []k8sNetworkPolicy `json:"items"`
}

// ── 载荷构造 ────────────────────────────────────────────────────────────

// newK8sJobName 生成 DNS label 形态的作业名（base36 纳秒，天然小写字母数字）。
func newK8sJobName() string {
	return "gak-job-" + strconv.FormatInt(time.Now().UnixNano(), 36)
}

// buildK8sJob 把 backend.Job 映射成 batch/v1 Job（docker.go 的同一张表）。
// 禁网路径装配出站探测 init 容器（issue #262）；稳定执行标识绑定 Job 名与
// 标签（issue #263，harness.io/execution-id：Job 对象与 Pod 模板都带）。
func buildK8sJob(name, ns string, j backend.Job, k *K8sJob) k8sJobObject {
	labels := map[string]string{k8sJobLabel: name, k8sManagedByLabel: k8sManagedByValue}
	podLabels := map[string]string{k8sJobLabel: name}
	if j.ExecutionID != "" {
		labels[k8sExecutionIDLabel] = j.ExecutionID
		podLabels[k8sExecutionIDLabel] = j.ExecutionID
	}
	env := make([]k8sEnvVar, 0, len(j.Env))
	for _, kv := range j.Env {
		key, value, _ := strings.Cut(kv, "=")
		env = append(env, k8sEnvVar{Name: key, Value: value})
	}
	var volumes []k8sVolume
	var mounts []k8sVolumeMount
	for i, m := range j.Mounts {
		volName := "mnt-" + strconv.Itoa(i)
		volumes = append(volumes, k8sVolume{Name: volName, HostPath: &k8sHostPath{Path: m.Source}})
		mounts = append(mounts, k8sVolumeMount{Name: volName, MountPath: m.Target, ReadOnly: m.ReadOnly})
	}
	limits := map[string]string{}
	if j.Budget.MaxMemory > 0 {
		limits["memory"] = strconv.FormatInt(j.Budget.MaxMemory, 10)
	}
	var deadline int64
	if j.Budget.WallClock > 0 {
		deadline = int64(math.Ceil(j.Budget.WallClock.Seconds()))
		if deadline < 1 {
			deadline = 1
		}
	}
	var inits []k8sContainer
	if k.k8sEgressProbeRequired() {
		inits = append(inits, k8sEgressProbeContainer(k.probe))
	}
	return k8sJobObject{
		APIVersion: "batch/v1",
		Kind:       "Job",
		Metadata:   k8sObjectMeta{Name: name, Namespace: ns, Labels: labels},
		Spec: k8sJobSpec{
			BackoffLimit:            0, // 作业只跑一次：重试不是本契约的字段
			TTLSecondsAfterFinished: k.ttlSeconds,
			ActiveDeadlineSeconds:   deadline,
			Template: k8sPodTemplate{
				Metadata: k8sTemplateMeta{Labels: podLabels},
				Spec: k8sPodSpec{
					RestartPolicy:                "Never",
					ServiceAccountName:           k.serviceAccount,
					AutomountServiceAccountToken: false,
					SecurityContext: k8sPodSecurityContext{
						RunAsNonRoot:   true,
						SeccompProfile: &k8sSeccompProfile{Type: "RuntimeDefault"},
					},
					InitContainers: inits,
					Containers: []k8sContainer{{
						Name:       k8sJobContainerName,
						Image:      j.Image, // 带 digest 的引用原样携带
						Args:       j.Args,
						Env:        env,
						WorkingDir: j.WorkDir,
						Resources:  k8sResources{Limits: limits},
						SecurityContext: k8sContainerSecurityContext{
							AllowPrivilegeEscalation: false,
							ReadOnlyRootFilesystem:   true,
							Capabilities:             &k8sCapabilities{Drop: []string{"ALL"}},
						},
						VolumeMounts: mounts,
					}},
					Volumes: volumes,
				},
			},
		},
	}
}

// k8sEgressProbeContainer 出站探测 init 容器：与业务容器同等受 Pod 安全约束
// （runAsNonRoot 由 Pod 级 SecurityContext 覆盖 init 容器，镜像同样必须能
// 非 root 运行）。探测约定见 K8sEgressProbe。
func k8sEgressProbeContainer(p K8sEgressProbe) k8sContainer {
	return k8sContainer{
		Name:      k8sEgressProbeContainerName,
		Image:     p.Image,
		Command:   p.Command,
		Resources: k8sResources{},
		SecurityContext: k8sContainerSecurityContext{
			AllowPrivilegeEscalation: false,
			ReadOnlyRootFilesystem:   true,
			Capabilities:             &k8sCapabilities{Drop: []string{"ALL"}},
		},
	}
}

// buildK8sNetworkPolicy 逐作业 Egress 全拒策略：空 egress 列表 = 默认拒绝。
func buildK8sNetworkPolicy(name, ns string) k8sNetworkPolicy {
	return k8sNetworkPolicy{
		APIVersion: "networking.k8s.io/v1",
		Kind:       "NetworkPolicy",
		Metadata:   k8sObjectMeta{Name: name, Namespace: ns, Labels: map[string]string{k8sJobLabel: name}},
		Spec: k8sNetworkPolicySpec{
			PodSelector: k8sLabelSelector{MatchLabels: map[string]string{k8sJobLabel: name}},
			PolicyTypes: []string{"Egress"},
			Egress:      []struct{}{},
		},
	}
}

// ── 传输在 k8sjobtransport.go（拆文件只为满足 400 行阈值）──────────────
