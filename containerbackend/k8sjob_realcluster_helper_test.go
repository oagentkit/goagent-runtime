// k8sjob_realcluster_helper_test.go —— 真集群验收的裸 API 助手与取证面。
//
// 为什么单独成文件：本仓的结构阈值是**单文件 400 行**（make architecture-check），
// 验收用例与它的助手合在一起超线。拆分按「谁在读」分：用例文件讲**判据**
// （同一条命令、两个方向都断言），助手文件讲**怎么把真集群的现场取回来**
// （建/删 Pod、等终态、读退出码与日志、失败取证）。
//
// 助手一律走本后端的同一条传输管道（k.call），不引第三方依赖：真集群验收
// 要证明的就是这条管道在真集群上的行为，多一条路就多一份「测的不是它」的余地。
package containerbackend

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	kerrors "github.com/oekit/pkg/errors"
)

// k8sE2EScene 失败现场：作业 Pod 与命名空间事件。
//
// 为什么要有它：「作业没跑起来」有多种成因（调度没排上 / 镜像拉不到 /
// runAsNonRoot 被拒 / 策略装配后作业被拒），只看后端自报的 reason 分不开。
// 取证落到 Pod 状态与事件上，失败才可归因（与 raceprobe 同一纪律：结论要有出处）。
func k8sE2EScene(ctx context.Context, k *K8sJob, env map[string]string) string {
	ns := env[envK8sE2ENamespace]
	resp, err := k.call(ctx, http.MethodGet, "/api/v1/namespaces/"+ns+"/pods", nil, nil)
	pods := "(读不到 Pod 列表)"
	if err == nil && resp.status == 200 {
		pods = string(resp.body)
	}
	ev, err2 := k.call(ctx, http.MethodGet, "/api/v1/namespaces/"+ns+"/events", nil, nil)
	events := "(读不到事件)"
	if err2 == nil && ev.status == 200 {
		events = string(ev.body)
	}
	return "pods=" + pods + " events=" + events
}

// ── 裸 API 助手（真集群，不引第三方依赖）────────────────────────────

// k8sE2ECreatePod / k8sE2EDeletePod 用例自建/自删 Pod（走本后端的同一条
// 传输管道；同包测试可及）。
func k8sE2ECreatePod(ctx context.Context, k *K8sJob, ns, name, image, script string) error {
	cmd, err := json.Marshal([]string{"/bin/sh", "-ec", script})
	if err != nil {
		return err
	}
	payload := fmt.Sprintf(
		`{"apiVersion":"v1","kind":"Pod","metadata":{"name":%q,"namespace":%q,"labels":{"gak-e2e":"probe"}},`+
			`"spec":{"restartPolicy":"Never","terminationGracePeriodSeconds":5,`+
			`"containers":[{"name":"main","image":%q,"command":%s}]}}`,
		name, ns, image, cmd)
	resp, err := k.call(ctx, http.MethodPost, "/api/v1/namespaces/"+ns+"/pods", json.RawMessage(payload), nil)
	if err != nil {
		return err
	}
	return k8sAPIError("创建 Pod", resp)
}

func k8sE2EDeletePod(ctx context.Context, k *K8sJob, ns, name string) error {
	resp, err := k.call(ctx, http.MethodDelete, "/api/v1/namespaces/"+ns+"/pods/"+name, nil, nil)
	if err != nil {
		return err
	}
	if resp.status == http.StatusNotFound {
		return nil
	}
	return k8sAPIError("删除 Pod", resp)
}

// k8sE2EPodView 用例自读的 Pod 视图（只取用得到的字段）。
type k8sE2EPodView struct {
	Status struct {
		Phase             string `json:"phase"`
		PodIP             string `json:"podIP"`
		ContainerStatuses []struct {
			Name  string `json:"name"`
			State struct {
				Terminated *struct {
					ExitCode int `json:"exitCode"`
				} `json:"terminated"`
			} `json:"state"`
		} `json:"containerStatuses"`
	} `json:"status"`
}

// k8sE2EWaitPodRunning 等 Pod Running 且拿到 IP（见证端点可以开始服务了）。
func k8sE2EWaitPodRunning(ctx context.Context, t *testing.T, k *K8sJob, ns, name string, timeout time.Duration) k8sE2EPodView {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		view, err := k8sE2EGetPod(ctx, k, ns, name)
		if err == nil && view.Status.Phase == "Running" && view.Status.PodIP != "" {
			return view
		}
		if time.Now().After(deadline) {
			t.Fatalf("等 Pod %s Running 超时（%s）：%v", name, timeout, err)
		}
		time.Sleep(2 * time.Second)
	}
}

// k8sE2EWaitPodTerminal 等 Pod 落到终态（Running 不算终态：容器还在跑）。
func k8sE2EWaitPodTerminal(ctx context.Context, t *testing.T, k *K8sJob, ns, name string, timeout time.Duration) k8sE2EPodView {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		view, err := k8sE2EGetPod(ctx, k, ns, name)
		if err == nil {
			switch view.Status.Phase {
			case "Succeeded", "Failed":
				return view
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("等 Pod %s 终态超时（%s）：%v", name, timeout, err)
		}
		time.Sleep(2 * time.Second)
	}
}

func k8sE2EGetPod(ctx context.Context, k *K8sJob, ns, name string) (k8sE2EPodView, error) {
	var out k8sE2EPodView
	resp, err := k.call(ctx, http.MethodGet, "/api/v1/namespaces/"+ns+"/pods/"+name, nil, nil)
	if err != nil {
		return out, err
	}
	if err := k8sAPIError("查询 Pod", resp); err != nil {
		return out, err
	}
	if err := json.Unmarshal(resp.body, &out); err != nil {
		return out, kerrors.Wrap(ErrAPICall, "containerbackend: 解析 Pod 应答失败: "+err.Error())
	}
	return out, nil
}

// k8sE2EExitCode 读容器终止退出码（读不到返回 ok=false，不猜）。
func k8sE2EExitCode(pod k8sE2EPodView, container string) (int, bool) {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name == container && cs.State.Terminated != nil {
			return cs.State.Terminated.ExitCode, true
		}
	}
	return 0, false
}

// k8sE2ELogs 读容器日志（判据落在可读证据上，不只落在退出码上）。
func k8sE2ELogs(ctx context.Context, t *testing.T, k *K8sJob, ns, name string) string {
	t.Helper()
	resp, err := k.call(ctx, http.MethodGet,
		"/api/v1/namespaces/"+ns+"/pods/"+name+"/log", nil, nil)
	if err != nil || resp.status < 200 || resp.status >= 300 {
		return ""
	}
	return string(resp.body)
}

// k8sE2ECreateRawPolicy / k8sE2EDeleteRawPolicy 用例自管叠加策略（走本后端
// 的同一条传输管道；同包测试可及，不引第三方依赖）。
func k8sE2ECreateRawPolicy(ctx context.Context, k *K8sJob, ns, payload string) error {
	resp, err := k.call(ctx, http.MethodPost,
		"/apis/networking.k8s.io/v1/namespaces/"+ns+"/networkpolicies",
		json.RawMessage(payload), nil)
	if err != nil {
		return err
	}
	return k8sAPIError("创建叠加策略", resp)
}

func k8sE2EDeleteRawPolicy(ctx context.Context, k *K8sJob, ns, name string) {
	_, _ = k.call(ctx, http.MethodDelete,
		"/apis/networking.k8s.io/v1/namespaces/"+ns+"/networkpolicies/"+name, nil, nil)
}
