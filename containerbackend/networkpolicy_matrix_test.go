// networkpolicy_matrix_test.go —— 故障矩阵行十四的声明面判据（随包自
// goagent-kernel 迁入：原 worker/faultmatrix 的 networkpolicy_test.go）。
//
// 进程内可判的是**声明面门禁**（kernel issue #262）：禁网档只在「策略装配开启 +
// CNI 能力事实为真 + 出站探测已装配」三者齐备时自报 EgressDenied；缺任何
// 一半都必须降级为 EgressUnrestricted（fail-closed，不静默假报）。
//
// 真集群格（requiresEnv，kernel 矩阵里的 network-policy-real-dataplane）验证的
// 是数据面：apiserver 接受 NetworkPolicy 不等于 CNI 真的阻断出站；该格用实际
// 出站探测断言（k8sjob_realcluster_test.go）。
package containerbackend

import (
	"errors"
	"testing"

	"github.com/oagentkit/guardx/sandbox"
)

// errInjectedResponseLost 注入「响应丢失」形态的能力探测错误（原 faultmatrix
// 套件的同名哨兵，随测试迁入本包）。
var errInjectedResponseLost = errors.New("injected: response lost")

// staticCNI 固定答案的 CNI 能力探测替身。
type staticCNI struct {
	enforced bool
	err      error
}

func (c staticCNI) NetworkPolicyEnforced() (bool, error) { return c.enforced, c.err }

func newDeclarationJob(opts ...K8sJobOption) *K8sJob {
	base := []K8sJobOption{
		WithK8sEndpoint("http://127.0.0.1:1"),
		WithK8sNamespace("faultmatrix"),
		WithK8sNetworkPolicy(true),
	}
	return NewK8sJob(append(base, opts...)...)
}

func probeSpec() K8sEgressProbe {
	return K8sEgressProbe{
		Image:   "registry.example/probe@sha256:feedface",
		Command: []string{"/probe", "-expect-blocked"},
	}
}

// TestNetworkPolicyDeclarationFailClosed 声明面三条全齐才报禁网；缺一条就降级。
// 这是「缺环境时仍可判」的进程内事实，真集群格也会先跑它（原矩阵的
// network-policy-declaration-fail-closed 格，判据逐字随迁）。
func TestNetworkPolicyDeclarationFailClosed(t *testing.T) {
	// 缺 CNI 能力事实：答不上来 ⇒ 不得自报禁网。
	if got := newDeclarationJob(WithK8sEgressProbe(probeSpec())).Egress(); got != sandbox.EgressUnrestricted {
		t.Fatalf("缺 CNI 能力事实时必须降级为 unrestricted（实得 %v）", got)
	}
	// CNI 明确不实现 NetworkPolicy：不得自报禁网。
	if got := newDeclarationJob(
		WithK8sCNICapability(staticCNI{enforced: false}),
		WithK8sEgressProbe(probeSpec()),
	).Egress(); got != sandbox.EgressUnrestricted {
		t.Fatalf("CNI 不实现 NetworkPolicy 时必须降级（实得 %v）", got)
	}
	// CNI 能力探测报错：不可判 ⇒ 不得自报禁网。
	if got := newDeclarationJob(
		WithK8sCNICapability(staticCNI{err: errInjectedResponseLost}),
		WithK8sEgressProbe(probeSpec()),
	).Egress(); got != sandbox.EgressUnrestricted {
		t.Fatalf("CNI 能力不可判时必须降级（实得 %v）", got)
	}
	// 有 CNI 但没装配出站探测：没有实测面 ⇒ 不得自报禁网。
	if got := newDeclarationJob(WithK8sCNICapability(staticCNI{enforced: true})).Egress(); got != sandbox.EgressUnrestricted {
		t.Fatalf("缺出站探测时必须降级（实得 %v）", got)
	}
	// 策略装配被显式关闭：如实降级。
	if got := NewK8sJob(
		WithK8sEndpoint("http://127.0.0.1:1"),
		WithK8sNamespace("faultmatrix"),
		WithK8sNetworkPolicy(false),
		WithK8sCNICapability(staticCNI{enforced: true}),
		WithK8sEgressProbe(probeSpec()),
	).Egress(); got != sandbox.EgressUnrestricted {
		t.Fatalf("策略装配关闭时必须如实降级（实得 %v）", got)
	}
	// 三条齐备：声明禁网档。
	if got := newDeclarationJob(
		WithK8sCNICapability(staticCNI{enforced: true}),
		WithK8sEgressProbe(probeSpec()),
	).Egress(); got != sandbox.EgressDenied {
		t.Fatalf("策略 + CNI 能力 + 出站探测齐备时必须自报禁网档（实得 %v）", got)
	}
}
