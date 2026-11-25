// k8sjobegress.go —— Egress=denied 的「声明面 / 验证面」分离（issue #262）。
//
// # 缺口与修法
//
// 修前 `Egress()` 只要没关策略就自报 EgressDenied，但这个档位不由本后端
// 直接兑现：禁网靠逐作业 NetworkPolicy，而「策略被 apiserver 接受 ≠ 数据面
// 生效」（取决于 CNI），且原生 NetworkPolicy 的允许规则是**叠加**语义——
// 存在一条全拒策略时，其他选中同一 Pod 的策略若允许出站，最终仍可能放行。
// 缺的不是诚实声明（包注释早已写明边界），是**把声明与实测绑在一起的机制**。
//
// 四件机制（与 issue 验收标准一一对应）：
//
//  1. 声明面门禁：`Egress()` 只在「策略装配开启 + CNI 能力事实为真 + 出站
//     探测已装配」三者齐备时才报 EgressDenied；缺任何一半降级为
//     EgressUnrestricted（fail-closed）。CNI 是否实现 NetworkPolicy 是装配期
//     可问的事实（注入 K8sCNICapability），答不上来就不许选禁网档。
//  2. 独立执行命名空间：WithK8sExecutionNamespace 让禁网作业整体落在专用
//     命名空间（job / policy / pod / 日志 / 清理同namespace），把「别人家」的
//     策略面与业务面隔在门外。
//  3. 严格策略装配 + 创建后读回：创建前 LIST 执行命名空间的全部
//     NetworkPolicy，发现「允许出站且可能选中本作业 Pod」的策略即拒绝
//     创建作业——叠加语义被显式处理掉，不是碰运气；LIST 失败同样拒绝
//     （对账不了策略面 = 兑现不了禁网，fail-closed）。**创建成功也不代表
//     落库形态正确**（mutating webhook / 控制器可改写，响应丢失时对象存在
//     与否未知）：建后必须 GET 读回并断言「就是本作业的 Egress 全拒策略」，
//     读回失败 / 不存在 / 形态不符一律拒绝创建作业（verifyNetworkPolicy）；
//     409 同名复用走同一条读回校验。
//  4. 实际出站探测：禁网作业装配 init 容器（探测镜像/命令由宿主注入），
//     作业启动即尝试访问外部地址。约定：exit 0 = 出站尝试失败（禁网实测
//     成立，放行业务容器）；exit ≠ 0 = 连上外部或探测自身不可判 ⇒ init
//     失败，Pod 永远到不了业务容器（fail-closed）。实测结果写进
//     Result.Detail["egress_verified"]；Detail 不是第二真相，成败仍以
//     OK / ExitCode 为准。
package containerbackend

import (
	"context"
	"fmt"

	"github.com/oekit/pkg/errors"

	"github.com/oagentkit/guardx/sandbox"
	"github.com/oagentkit/goagent-kernel/backend"
)

// K8sCNICapability CNI 能力探测接口：回答「本集群 CNI 是否实现 NetworkPolicy
// 数据面」。这是**装配期可问的事实**，不是运行期探测——apiserver 接受
// NetworkPolicy 对象证明不了数据面生效，答案只能来自了解集群的一方（宿主
// 按集群选型注入）。实现必须廉价（如读装配配置），Egress() 会在选择期调用
// 它（结果缓存，答一次即可）。
type K8sCNICapability interface {
	// NetworkPolicyEnforced 返回能力事实：true = CNI 实现 NetworkPolicy；
	// false = 不实现；error = 答不上来（声明面按兜不住处理）。
	NetworkPolicyEnforced() (bool, error)
}

// K8sEgressProbe 出站探测装配：禁网作业的 init 容器（镜像与命令由宿主注入）。
//
// 探测命令的约定（本后端只解释退出码，不规定探测方式）：
//
//	exit 0  = 出站尝试失败（连接被阻断）⇒ 禁网实测成立，业务容器放行；
//	exit ≠0 = 连上外部 或 探测自身不可判 ⇒ init 失败，作业被拒。
//
// 不可判也算失败是刻意的：探测命令跑不起来（镜像缺工具、拼错命令）与
// 「连上了」同样无法证明禁网，一律 fail-closed。
type K8sEgressProbe struct {
	// Image 探测镜像引用（带 digest 钉住）。空 = 未装配探测，禁网档声明
	// 随之降级（声明面门禁）。
	Image string
	// Command 探测命令（完整覆盖镜像入口点）。
	Command []string
}

// cniNetworkPolicyEnforced CNI 能力事实（懒答 + 缓存：Egress() 在选择期与
// Exec 期都会问，探测接口只该被调用一次）。能力缺失（未注入探测接口）按
// 「答不上来」处理，禁网档声明随之降级——这不是需要调用方分诊的哨兵错误，
// 声明面把它消化成更弱的自报档位，不外泄。
func (k *K8sJob) cniNetworkPolicyEnforced() (bool, error) {
	k.cniOnce.Do(func() {
		if k.cniCap == nil {
			k.cniErr = errors.New("containerbackend: 未注入 CNI 能力探测（答不上来）")
			return
		}
		k.cniOK, k.cniErr = k.cniCap.NetworkPolicyEnforced()
	})
	return k.cniOK, k.cniErr
}

// k8sEgressTierName 声明面的可判名（写进 Detail，与 Egress() 同源——同一
// 函数计算，不抄第二份）。
func k8sEgressTierName(m sandbox.EgressMode) string {
	if m == sandbox.EgressDenied {
		return "denied"
	}
	return "unrestricted"
}

// k8sEgressProbeRequired 报告禁网路径是否装配了出站探测（策略开启 + 探测注入）。
// 探测缺失时策略仍会创建（纵深防御），但声明面降级、实测面记 none。
func (k *K8sJob) k8sEgressProbeRequired() bool {
	return k.networkPolicy && k.probe.Image != "" && len(k.probe.Command) > 0
}

// k8sCheckForeignPolicies 严格策略装配：执行命名空间里存在其他**允许出站**
// 的策略且可能选中本作业 Pod 时，NetworkPolicy 的叠加语义会放行出站 ⇒
// 拒绝创建作业（fail-closed）。LIST 失败同样拒绝——对账不了策略面就是
// 兑现不了禁网。
func k8sCheckForeignPolicies(ctx context.Context, k *K8sJob, ns, name, executionID string) error {
	policies, err := k.listNetworkPolicies(ctx, ns)
	if err != nil {
		return errors.Wrap(ErrNetworkPolicy, "策略面对账失败（fail-closed）: "+err.Error())
	}
	for _, p := range policies {
		if p.Metadata.Name == name {
			continue // 本作业同名策略：重试路径上的既有对象，由创建侧校验其语义
		}
		if len(p.Spec.Egress) == 0 || !k8sCouldSelectOurPods(p, name, executionID) {
			continue // 选不中本作业 Pod，或只拒不放 ⇒ 不构成叠加
		}
		return errors.Wrap(ErrNetworkPolicy, fmt.Sprintf(
			"执行命名空间 %q 存在允许出站的策略 %q 且可能选中本作业 Pod——NetworkPolicy 叠加语义会放行出站，拒绝创建作业",
			ns, p.Metadata.Name))
	}
	return nil
}

// verifyNetworkPolicy 读回并断言策略就是本作业的 Egress 全拒策略（issue #262
// 的「创建时实测校验」）：apiserver 接受创建请求不代表落库形态正确——
// mutating admission webhook / 集群控制器可以改写 spec，创建响应也可能在
// 网络上丢失（超时）。两类都按 fail-closed：读回失败 / 读回不存在 / 读回
// 不是本作业的全拒策略，一律 ErrNetworkPolicy，作业不创建。
//
// 注意回收边界：本函数不登记 policyCreated（由调用方决定）——409 复用路径
// 上校验失败的对象可能是别人的，绝不能删。
func (k *K8sJob) verifyNetworkPolicy(ctx context.Context, ns, name string) error {
	p, found, err := k.getNetworkPolicy(ctx, ns, name)
	if err != nil {
		return errors.Wrap(ErrNetworkPolicy,
			"NetworkPolicy 读回校验失败（创建结果不可证，fail-closed）: "+err.Error())
	}
	if !found {
		return errors.Wrap(ErrNetworkPolicy,
			"NetworkPolicy 创建成功但读回不存在（创建结果不可证，fail-closed）")
	}
	if !k8sPolicyIsOursDenyAll(p, name) {
		return errors.Wrap(ErrNetworkPolicy,
			"NetworkPolicy 读回形态不是本作业的 Egress 全拒策略（拒绝创建作业）")
	}
	return nil
}

// k8sCouldSelectOurPods 判别人的策略是否可能选中本作业的 Pod。
//
// 本作业 Pod 的标签集 = {gak-job: 名, app.kubernetes.io/managed-by: …,
// harness.io/execution-id: …(幂等作业)}。NetworkPolicy 的 podSelector 是
// **子集匹配**（Pod 带有 selector 全部键值即被选中），故 matchLabels 是本
// 作业标签集子集 ⇒ 可能选中；matchExpressions 无法在本侧求值 ⇒ 按可能选中
// 处理（fail-closed）；空 selector 选中全命名空间 ⇒ 可能选中。
func k8sCouldSelectOurPods(p k8sNetworkPolicy, jobName, executionID string) bool {
	if len(p.Spec.PodSelector.MatchExpressions) > 0 {
		return true
	}
	ours := map[string]string{k8sJobLabel: jobName, k8sManagedByLabel: k8sManagedByValue}
	if executionID != "" {
		ours[k8sExecutionIDLabel] = executionID
	}
	for key, want := range p.Spec.PodSelector.MatchLabels {
		if got, ok := ours[key]; !ok || got != want {
			return false
		}
	}
	return true
}

// k8sProbeVerifiedFact 实测面：从 Pod 的 init 容器终止状态读探测退出码。
// 读不到（Pod 已被清理）记 unseen——不猜。
func k8sProbeVerifiedFact(pods []k8sPod) string {
	for i := range pods {
		for _, cs := range pods[i].Status.InitContainerStatuses {
			if cs.Name == k8sEgressProbeContainerName && cs.State.Terminated != nil {
				return fmt.Sprintf("probe_exit_%d", cs.State.Terminated.ExitCode)
			}
		}
	}
	return "unseen"
}

// k8sAttachEgressFacts 把声明面与实测面写进 Result.Detail。声明面与 Egress()
// 同源（同一函数）；实测面由 collect 从 Pod 状态读出（已存在时不覆盖），这里
// 只兜底 unknown 等没有可读状态的路径：按探测是否装配记 unseen / none，不猜。
func k8sAttachEgressFacts(k *K8sJob, res backend.Result, verified string) backend.Result {
	detail, ok := res.Detail.(map[string]any)
	if !ok {
		detail = map[string]any{}
	}
	detail["egress_declared"] = k8sEgressTierName(k.Egress())
	if _, exists := detail["egress_verified"]; !exists {
		detail["egress_verified"] = verified
	}
	res.Detail = detail
	return res
}
