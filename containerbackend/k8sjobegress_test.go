// k8sjobegress_test.go —— issue #262：禁网档的「声明面」与「验证面」分离。
//
// 判据先于实现：本文件先于机制落地写成（红），实现转绿后固定为门禁。
// 四条验收的机判位置：
//  1. 真集群出站必须失败        → k8sjob_realcluster_test.go（requiresEnv 显式 Skipf）
//  2. 同 NS 存在允许出站策略 ⇒ 拒绝 → TestK8sExecEgressStackingRefused
//  3. CNI 不实现 NetworkPolicy ⇒ 装配期拒绝禁网档 → TestK8sEgressDeclarationGate
//  4. 真集群用例显式 Skipf       → k8sjob_realcluster_test.go
package containerbackend

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	kerrors "github.com/oekit/pkg/errors"

	"github.com/oagentkit/guardx/sandbox"
	"github.com/oagentkit/goagent-kernel/backend"
)

// stubCNICapability 能力探测替身：注入「CNI 是否实现 NetworkPolicy」的答案。
type stubCNICapability struct {
	ok  bool
	err error
}

func (s stubCNICapability) NetworkPolicyEnforced() (bool, error) { return s.ok, s.err }

var errFakeCapability = kerrors.New("fake: 能力探测失败（答不上来）")

// foreignNetworkPolicy 构造一条「别人家」的策略载荷（裸 JSON，形态与集群里的一致）。
func foreignNetworkPolicy(name string, matchLabels map[string]string, withExpressions bool, allowEgress bool) json.RawMessage {
	ps := map[string]any{}
	if matchLabels != nil {
		ps["matchLabels"] = matchLabels
	}
	if withExpressions {
		ps["matchExpressions"] = []any{map[string]any{"key": "app", "operator": "Exists"}}
	}
	egress := []any{}
	if allowEgress {
		egress = append(egress, map[string]any{})
	}
	spec := map[string]any{"podSelector": ps, "policyTypes": []string{"Egress"}, "egress": egress}
	b, _ := json.Marshal(map[string]any{
		"apiVersion": "networking.k8s.io/v1", "kind": "NetworkPolicy",
		"metadata": map[string]any{"name": name}, "spec": spec,
	})
	return b
}

// TestK8sEgressDeclarationGate 声明面门禁：禁网档只在「策略装配 + CNI 能力事实 +
// 出站探测」三者齐备时才许自报；缺任何一半，声明降级为 unrestricted（fail-closed）。
func TestK8sEgressDeclarationGate(t *testing.T) {
	probe := K8sEgressProbe{Image: testK8sProbeImage, Command: testK8sProbeCommand}
	cases := []struct {
		name string
		k    *K8sJob
		want sandbox.EgressMode
	}{
		{"缺省装配：无能力事实无探测 ⇒ 不得声明禁网", NewK8sJob(), sandbox.EgressUnrestricted},
		{"能力=false：CNI 不实现 ⇒ 不得声明禁网", NewK8sJob(
			WithK8sCNICapability(stubCNICapability{ok: false}), WithK8sEgressProbe(probe)), sandbox.EgressUnrestricted},
		{"能力答不上来：探测报错 ⇒ 不得声明禁网", NewK8sJob(
			WithK8sCNICapability(stubCNICapability{err: errFakeCapability}), WithK8sEgressProbe(probe)), sandbox.EgressUnrestricted},
		{"有能力事实但无探测：声明没有实测面 ⇒ 不得声明禁网", NewK8sJob(
			WithK8sCNICapability(stubCNICapability{ok: true})), sandbox.EgressUnrestricted},
		{"三者齐备 ⇒ 声明禁网", NewK8sJob(
			WithK8sCNICapability(stubCNICapability{ok: true}), WithK8sEgressProbe(probe)), sandbox.EgressDenied},
		{"显式关策略 ⇒ 如实 unrestricted", NewK8sJob(WithK8sNetworkPolicy(false),
			WithK8sCNICapability(stubCNICapability{ok: true}), WithK8sEgressProbe(probe)), sandbox.EgressUnrestricted},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.k.Egress(); got != tc.want {
				t.Fatalf("Egress()=%s，期望 %s", got, tc.want)
			}
		})
	}
	t.Run("判据联动：三者齐备才被禁网 Spec 选中", func(t *testing.T) {
		spec := backend.Spec{MinIsolation: backend.IsolationContainer, Egress: sandbox.EgressDenied,
			Budget: sandbox.Budget{MaxMemory: 1 << 20}}
		full := NewK8sJob(WithK8sCNICapability(stubCNICapability{ok: true}), WithK8sEgressProbe(probe))
		if err := spec.SatisfiedBy(full); err != nil {
			t.Fatalf("三者齐备的装配必须满足禁网要求：%v", err)
		}
		half := NewK8sJob(WithK8sCNICapability(stubCNICapability{ok: true})) // 缺探测
		if err := spec.SatisfiedBy(half); !kerrors.Is(err, backend.ErrNotSatisfied) {
			t.Fatalf("缺实测面的装配不得被禁网 Spec 选中：err=%v", err)
		}
		unknown := NewK8sJob(WithK8sEgressProbe(probe)) // 缺能力事实
		if err := spec.SatisfiedBy(unknown); !kerrors.Is(err, backend.ErrNotSatisfied) {
			t.Fatalf("答不上来 CNI 能力的装配不得被禁网 Spec 选中：err=%v", err)
		}
	})
}

// TestK8sExecEgressStackingRefused 严格策略装配：执行命名空间里存在其他**允许出站**
// 的策略且可能选中本作业 Pod 时，叠加语义会放行出站 ⇒ 拒绝创建作业（fail-closed）。
func TestK8sExecEgressStackingRefused(t *testing.T) {
	// 本作业 Pod 实际携带 managed-by 标签：别人的策略按它选即子集命中 ⇒ 会叠加。
	managedByLabels := map[string]string{k8sManagedByLabel: k8sManagedByValue}
	cases := []struct {
		name        string
		foreign     json.RawMessage
		wantRefused bool
	}{
		{"全选 + 允许出站 ⇒ 叠加放行，必须拒绝", foreignNetworkPolicy("allow-all", map[string]string{}, false, true), true},
		{"子集标签 + 允许出站 ⇒ 会选中本作业 Pod，必须拒绝", foreignNetworkPolicy("allow-managed", managedByLabels, false, true), true},
		{"matchExpressions 无法判 ⇒ fail-closed 拒绝", foreignNetworkPolicy("expr-allow", nil, true, true), true},
		{"只拒不放（空 egress）⇒ 不构成叠加，放行", foreignNetworkPolicy("deny-other", map[string]string{}, false, false), false},
		{"标签不交集 ⇒ 选不中本作业 Pod，放行", foreignNetworkPolicy("allow-other", map[string]string{"app": "elsewhere"}, false, true), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeKube{
				foreignNPs: []json.RawMessage{tc.foreign},
				statuses:   []k8sJobStatus{{Succeeded: 1, Conditions: []k8sJobCondition{{Type: "Complete", Status: "True"}}}},
				pods:       []k8sPod{probedPod("pod-1", 0, 0)},
			}
			k := newFakeK8sJob(t, f)
			_, err := k.Exec(context.Background(), backend.Job{Image: testK8sImage})
			if tc.wantRefused {
				if !kerrors.Is(err, ErrNetworkPolicy) {
					t.Fatalf("存在允许出站的叠加策略必须拒绝创建作业：%v", err)
				}
				if snap := f.snapshot(); len(snap.jobRaw) != 0 {
					t.Fatalf("拒绝时不得创建 Job：%d", len(snap.jobRaw))
				}
				return
			}
			if err != nil {
				t.Fatalf("不构成叠加的策略不得误伤：%v", err)
			}
		})
	}
	t.Run("LIST 失败 ⇒ 对账不了策略面，拒绝（fail-closed）", func(t *testing.T) {
		f := &fakeKube{npListStatus: 500}
		k := newFakeK8sJob(t, f)
		if _, err := k.Exec(context.Background(), backend.Job{Image: testK8sImage}); !kerrors.Is(err, ErrNetworkPolicy) {
			t.Fatalf("策略面对账失败必须拒绝：%v", err)
		}
		if snap := f.snapshot(); len(snap.jobRaw) != 0 {
			t.Fatal("策略面对账不了时不得创建 Job")
		}
	})
}

// TestK8sExecEgressProbePayload 验证面装配：禁网作业必须携带出站探测 init 容器，
// 且探测容器与业务容器同等受 Pod 安全约束。
func TestK8sExecEgressProbePayload(t *testing.T) {
	f := &fakeKube{
		statuses: []k8sJobStatus{{Succeeded: 1, Conditions: []k8sJobCondition{{Type: "Complete", Status: "True"}}}},
		pods:     []k8sPod{probedPod("pod-1", 0, 0)},
	}
	k := newFakeK8sJob(t, f)
	res, err := k.Exec(context.Background(), backend.Job{Image: testK8sImage})
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK {
		t.Fatalf("res=%+v", res)
	}
	snap := f.snapshot()
	if len(snap.jobRaw) != 1 {
		t.Fatalf("创建调用：%d", len(snap.jobRaw))
	}
	var job k8sJobObject
	if err := json.Unmarshal(snap.jobRaw[0], &job); err != nil {
		t.Fatal(err)
	}
	inits := job.Spec.Template.Spec.InitContainers
	if len(inits) != 1 {
		t.Fatalf("禁网作业必须装配出站探测 init 容器：%d", len(inits))
	}
	p := inits[0]
	if p.Name != k8sEgressProbeContainerName {
		t.Fatalf("探测容器名：%q", p.Name)
	}
	if p.Image != testK8sProbeImage || !slices.Equal(p.Command, testK8sProbeCommand) {
		t.Fatalf("探测镜像/命令未按装配携带：%s %v", p.Image, p.Command)
	}
	if !p.SecurityContext.ReadOnlyRootFilesystem || p.SecurityContext.AllowPrivilegeEscalation ||
		p.SecurityContext.Capabilities == nil || !slices.Equal(p.SecurityContext.Capabilities.Drop, []string{"ALL"}) {
		t.Fatalf("探测容器必须与业务容器同等受安全约束：%+v", p.SecurityContext)
	}
}

// TestK8sExecEgressVerifiedFacts 验证面实测：Detail 报告探测的**实测结果**——
// 探测被阻断（exit 0）才放行业务容器；探测连上外部（exit≠0）⇒ 作业被拒。
func TestK8sExecEgressVerifiedFacts(t *testing.T) {
	t.Run("探测被阻断（exit 0）⇒ 实测=denied verified，作业放行", func(t *testing.T) {
		f := &fakeKube{
			statuses: []k8sJobStatus{{Succeeded: 1, Conditions: []k8sJobCondition{{Type: "Complete", Status: "True"}}}},
			pods:     []k8sPod{probedPod("pod-1", 0, 0)},
		}
		k := newFakeK8sJob(t, f)
		res, err := k.Exec(context.Background(), backend.Job{Image: testK8sImage})
		if err != nil {
			t.Fatal(err)
		}
		if !res.OK {
			t.Fatalf("res=%+v", res)
		}
		detail := detailOf(t, res)
		if detail["egress_declared"] != "denied" {
			t.Fatalf("声明面未进明细：%+v", detail)
		}
		if detail["egress_verified"] != "probe_exit_0" {
			t.Fatalf("实测面必须记录探测被阻断：%+v", detail)
		}
	})
	t.Run("探测连上外部（exit 1）⇒ 作业被拒，实测留痕", func(t *testing.T) {
		f := &fakeKube{
			statuses: []k8sJobStatus{{Failed: 1, Conditions: []k8sJobCondition{{Type: "Failed", Status: "True", Reason: "BackoffLimitExceeded"}}}},
			pods:     []k8sPod{probedPod("pod-1", 3, 1)},
		}
		k := newFakeK8sJob(t, f)
		res, err := k.Exec(context.Background(), backend.Job{Image: testK8sImage})
		if err != nil {
			t.Fatalf("探测未通过属作业失败，不是基础设施故障：%v", err)
		}
		if res.OK || res.ExitCode != 3 {
			t.Fatalf("res=%+v", res)
		}
		if detail := detailOf(t, res); detail["egress_verified"] != "probe_exit_1" {
			t.Fatalf("实测面必须记录探测未通过：%+v", detail)
		}
	})
	t.Run("无探测装配 ⇒ 实测=none，声明=unrestricted（策略仍创建，纵深防御）", func(t *testing.T) {
		f := &fakeKube{
			statuses: []k8sJobStatus{{Succeeded: 1, Conditions: []k8sJobCondition{{Type: "Complete", Status: "True"}}}},
			pods:     []k8sPod{terminatedPod("pod-1", 0)},
		}
		k := newFakeK8sJobNoProbe(t, f)
		res, err := k.Exec(context.Background(), backend.Job{Image: testK8sImage})
		if err != nil {
			t.Fatal(err)
		}
		if !res.OK {
			t.Fatalf("res=%+v", res)
		}
		detail := detailOf(t, res)
		if detail["egress_declared"] != "unrestricted" || detail["egress_verified"] != "none" {
			t.Fatalf("声明/实测必须如实：%+v", detail)
		}
		if snap := f.snapshot(); len(snap.npRaw) != 1 {
			t.Fatalf("策略仍应创建（纵深防御）：%d", len(snap.npRaw))
		}
		var job k8sJobObject
		if err := json.Unmarshal(f.snapshot().jobRaw[0], &job); err != nil {
			t.Fatal(err)
		}
		if len(job.Spec.Template.Spec.InitContainers) != 0 {
			t.Fatal("无探测装配时不得装配探测容器")
		}
	})
}

// TestK8sExecEgressNamespace 禁网作业落在独立执行命名空间（叠加语义的结构隔离）。
func TestK8sExecEgressNamespace(t *testing.T) {
	f := &fakeKube{
		statuses: []k8sJobStatus{{Succeeded: 1, Conditions: []k8sJobCondition{{Type: "Complete", Status: "True"}}}},
		pods:     []k8sPod{probedPod("pod-1", 0, 0)},
	}
	k := newFakeK8sJob(t, f, WithK8sExecutionNamespace(testK8sExecNamespace))
	if _, err := k.Exec(context.Background(), backend.Job{Image: testK8sImage}); err != nil {
		t.Fatal(err)
	}
	snap := f.snapshot()
	if snap.jobNS != testK8sExecNamespace || snap.npNS != testK8sExecNamespace || snap.podsNS != testK8sExecNamespace {
		t.Fatalf("禁网作业必须整体落在执行命名空间：job=%s np=%s pods=%s", snap.jobNS, snap.npNS, snap.podsNS)
	}
}

// probedPod 带出站探测 init 容器终止状态的 Pod（probeCode=0 表示探测被阻断）。
func probedPod(name string, code, probeCode int) k8sPod {
	return k8sPod{
		Metadata: k8sObjectMeta{Name: name},
		Status: k8sPodStatus{
			Phase: "Succeeded",
			InitContainerStatuses: []k8sContainerStatus{{
				Name:  k8sEgressProbeContainerName,
				State: k8sContainerState{Terminated: &k8sTerminated{ExitCode: probeCode}},
			}},
			ContainerStatuses: []k8sContainerStatus{{
				Name:  k8sJobContainerName,
				State: k8sContainerState{Terminated: &k8sTerminated{ExitCode: code}},
			}},
		},
	}
}
