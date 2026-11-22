package containerbackend

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	kerrors "github.com/oekit/pkg/errors"

	"github.com/oagentkit/guardx/sandbox"
	"github.com/yi-nology/goagent-kernel/backend"
)

func TestK8sJobDeclarations(t *testing.T) {
	// 禁网档声明已改为 fail-closed（issue #262）：没有能力事实与出站探测的
	// 缺省装配不得自报 EgressDenied——完整门禁见 TestK8sEgressDeclarationGate。
	k := NewK8sJob()
	if k.ID() != "k8s-job" {
		t.Fatalf("id=%s", k.ID())
	}
	if k.Isolation() != backend.IsolationContainer {
		t.Fatalf("Job 落容器边界，必须自报容器档：%s", k.Isolation())
	}
	if k.Egress() != sandbox.EgressUnrestricted {
		t.Fatal("缺省装配（无 CNI 能力事实、无出站探测）不得自报禁网档")
	}
	cases := []struct {
		name string
		b    sandbox.Budget
		want bool
	}{
		{"只要求内存：请求/上限兜得住", sandbox.Budget{MaxMemory: 1 << 20}, true},
		{"要求输出截断：如实报兜不住", sandbox.Budget{MaxMemory: 1 << 20, MaxBytes: 100}, false},
		{"要求逐调用计数：作业形态兜不住", sandbox.Budget{MaxMemory: 1 << 20, MaxCalls: 10}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := k.Capable(tc.b); got != tc.want {
				t.Fatalf("Capable(%+v)=%v，期望 %v", tc.b, got, tc.want)
			}
		})
	}
	t.Run("关闭 NetworkPolicy ⇒ 自报降为 unrestricted，不谎报禁网", func(t *testing.T) {
		off := NewK8sJob(WithK8sNetworkPolicy(false))
		if off.Egress() != sandbox.EgressUnrestricted {
			t.Fatalf("关闭策略后不得继续声称 EgressDenied：%s", off.Egress())
		}
		spec := backend.Spec{MinIsolation: backend.IsolationContainer, Egress: sandbox.EgressDenied}
		if err := spec.SatisfiedBy(off); !kerrors.Is(err, backend.ErrNotSatisfied) {
			t.Fatalf("要求禁网的 Spec 不得选中未兑现禁网的后端：err=%v", err)
		}
	})
}

func TestK8sExecCreatePayload(t *testing.T) {
	f := &fakeKube{
		statuses: []k8sJobStatus{{Succeeded: 1, Conditions: []k8sJobCondition{{Type: "Complete", Status: "True"}}}},
		pods:     []k8sPod{terminatedPod("pod-1", 0)},
	}
	k := newFakeK8sJob(t, f)
	res, err := k.Exec(context.Background(), backend.Job{
		Image:   testK8sImage,
		Args:    []string{"--flag", "v"},
		Env:     []string{"K=V", "A=B=C"},
		Mounts:  []backend.Mount{{Source: "/src", Target: "/dst", ReadOnly: true}},
		WorkDir: "/w",
		Budget:  sandbox.Budget{MaxMemory: 256 << 20, WallClock: 30 * time.Second},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK {
		t.Fatalf("res=%+v", res)
	}
	snap := f.snapshot()
	if len(snap.jobRaw) != 1 || len(snap.npRaw) != 1 {
		t.Fatalf("创建调用：job=%d np=%d", len(snap.jobRaw), len(snap.npRaw))
	}
	var job k8sJobObject
	if err := json.Unmarshal(snap.jobRaw[0], &job); err != nil {
		t.Fatal(err)
	}
	if job.APIVersion != "batch/v1" || job.Kind != "Job" {
		t.Fatalf("对象形态：%s %s", job.APIVersion, job.Kind)
	}
	if job.Metadata.Name == "" || job.Metadata.Namespace != testK8sNamespace {
		t.Fatalf("namespace/name 未按装配携带：%+v", job.Metadata)
	}
	if job.Spec.BackoffLimit != 0 {
		t.Fatalf("作业只跑一次（不重试），backoffLimit=%d", job.Spec.BackoffLimit)
	}
	if job.Spec.TTLSecondsAfterFinished != 45 {
		t.Fatalf("TTL 未按装配携带：%d", job.Spec.TTLSecondsAfterFinished)
	}
	if job.Spec.ActiveDeadlineSeconds != 30 {
		t.Fatalf("墙钟预算未映射 activeDeadlineSeconds：%d", job.Spec.ActiveDeadlineSeconds)
	}
	ps := job.Spec.Template.Spec
	if ps.ServiceAccountName != testK8sSA {
		t.Fatalf("SA 未携带：%q", ps.ServiceAccountName)
	}
	if ps.RestartPolicy != "Never" || ps.AutomountServiceAccountToken {
		t.Fatalf("Pod 语义：restart=%s automount=%v", ps.RestartPolicy, ps.AutomountServiceAccountToken)
	}
	if !ps.SecurityContext.RunAsNonRoot || ps.SecurityContext.SeccompProfile == nil ||
		ps.SecurityContext.SeccompProfile.Type != "RuntimeDefault" {
		t.Fatalf("Pod 安全约束缺失：%+v", ps.SecurityContext)
	}
	if len(ps.Containers) != 1 {
		t.Fatalf("容器数：%d", len(ps.Containers))
	}
	c := ps.Containers[0]
	if c.Image != testK8sImage {
		t.Fatalf("镜像 digest 被改动：%s", c.Image)
	}
	if !slices.Equal(c.Args, []string{"--flag", "v"}) || c.WorkingDir != "/w" {
		t.Fatalf("args/workdir 未携带：%v / %q", c.Args, c.WorkingDir)
	}
	if len(c.Env) != 2 || c.Env[0].Name != "K" || c.Env[0].Value != "V" || c.Env[1].Value != "B=C" {
		t.Fatalf("Env 未按 K=V 原样拆分：%+v", c.Env)
	}
	if c.Resources.Limits["memory"] != "268435456" {
		t.Fatalf("内存上限未兑现：%v", c.Resources.Limits)
	}
	if !c.SecurityContext.ReadOnlyRootFilesystem || c.SecurityContext.AllowPrivilegeEscalation ||
		c.SecurityContext.Capabilities == nil || !slices.Equal(c.SecurityContext.Capabilities.Drop, []string{"ALL"}) {
		t.Fatalf("容器安全约束缺失：%+v", c.SecurityContext)
	}
	if len(ps.Volumes) != 1 || ps.Volumes[0].HostPath == nil || ps.Volumes[0].HostPath.Path != "/src" {
		t.Fatalf("挂载来源未映射 hostPath：%+v", ps.Volumes)
	}
	if len(c.VolumeMounts) != 1 || c.VolumeMounts[0].MountPath != "/dst" || !c.VolumeMounts[0].ReadOnly {
		t.Fatalf("只读挂载未携带：%+v", c.VolumeMounts)
	}

	var np map[string]any
	if err := json.Unmarshal(snap.npRaw[0], &np); err != nil {
		t.Fatal(err)
	}
	spec, _ := np["spec"].(map[string]any)
	selector, _ := spec["podSelector"].(map[string]any)
	labels, _ := selector["matchLabels"].(map[string]any)
	if labels[k8sJobLabel] != job.Metadata.Name {
		t.Fatalf("NetworkPolicy 未选中本作业的 Pod：%v", labels)
	}
	types, _ := spec["policyTypes"].([]any)
	if len(types) != 1 || types[0] != "Egress" {
		t.Fatalf("policyTypes 必须是 Egress：%v", types)
	}
	egress, ok := spec["egress"].([]any)
	if !ok || len(egress) != 0 {
		t.Fatalf("egress 必须显式为空数组（默认拒绝全部出站）：%v", spec["egress"])
	}
	if len(snap.jobDeletes) != 1 || len(snap.npDeletes) != 1 {
		t.Fatalf("正常路径必须清理 Job 与 NetworkPolicy：job=%v np=%v", snap.jobDeletes, snap.npDeletes)
	}
}

func TestK8sExecSucceeded(t *testing.T) {
	f := &fakeKube{
		statuses: []k8sJobStatus{
			{Active: 1},
			{Succeeded: 1, Conditions: []k8sJobCondition{{Type: "Complete", Status: "True"}}},
		},
		pods: []k8sPod{terminatedPod("pod-1", 0)},
		logs: map[string]string{"pod-1": "out\n"},
	}
	k := newFakeK8sJob(t, f)
	res, err := k.Exec(context.Background(), backend.Job{Image: testK8sImage})
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK || res.ExitCode != 0 {
		t.Fatalf("res=%+v", res)
	}
	if res.Stdout != "out\n" {
		t.Fatalf("Pod 日志未带回：%q", res.Stdout)
	}
	if f.snapshot().getCalls < 2 {
		t.Fatal("必须轮询到终态，而不是首轮猜成功")
	}
}

func TestK8sExecFailed(t *testing.T) {
	f := &fakeKube{
		statuses: []k8sJobStatus{{
			Failed:     1,
			Conditions: []k8sJobCondition{{Type: "Failed", Status: "True", Reason: "BackoffLimitExceeded"}},
		}},
		pods: []k8sPod{terminatedPod("pod-1", 3)},
		logs: map[string]string{"pod-1": "boom\n"},
	}
	k := newFakeK8sJob(t, f)
	res, err := k.Exec(context.Background(), backend.Job{Image: testK8sImage})
	if err != nil {
		t.Fatalf("作业失败不是基础设施故障：%v", err)
	}
	if res.OK || res.ExitCode != 3 {
		t.Fatalf("res=%+v", res)
	}
	if res.Stdout != "boom\n" {
		t.Fatalf("失败日志未带回：%q", res.Stdout)
	}
	detail, ok := res.Detail.(map[string]any)
	if !ok || detail["reason"] != "BackoffLimitExceeded" {
		t.Fatalf("失败原因未留明细：%+v", res.Detail)
	}
}

func TestK8sExecConditionMissingFailsClosed(t *testing.T) {
	f := &fakeKube{statuses: []k8sJobStatus{{Active: 1}}}
	k := newFakeK8sJob(t, f)
	res, err := k.Exec(context.Background(), backend.Job{Image: testK8sImage})
	if err != nil {
		t.Fatal(err)
	}
	detail, ok := res.Detail.(map[string]any)
	if res.OK || !ok || detail["state"] != "unknown" || detail["reason"] != "active_without_pods" {
		t.Fatalf("条件缺失/对账不一致必须 fail-closed：res=%+v detail=%+v", res, res.Detail)
	}
}

func terminatedPod(name string, code int) k8sPod {
	return k8sPod{
		Metadata: k8sObjectMeta{Name: name},
		Status: k8sPodStatus{
			Phase: "Succeeded",
			ContainerStatuses: []k8sContainerStatus{{
				Name:  k8sJobContainerName,
				State: k8sContainerState{Terminated: &k8sTerminated{ExitCode: code}},
			}},
		},
	}
}
