// k8sjobstatus.go —— K8s Job 的状态对账面：轮询、终态判定、退出码与日志收集。
//
// 拆分只为满足单文件 400 行阈值（make architecture-check）：语义（自报什么、
// 禁网的声明/验证分离）在 k8sjob.go 与 k8sjobegress.go，装配面在 k8sjobconfig.go。
package containerbackend

import (
	"context"
	"time"

	"github.com/yi-nology/goagent-kernel/backend"
)

// await 轮询 Job 直到终态或对账给不出结论。
func (k *K8sJob) await(ctx context.Context, ns, name string) (backend.Result, error) {
	var pendingSince time.Time
	for {
		job, found, err := k.getJob(ctx, ns, name)
		if err != nil {
			return backend.Result{}, err
		}
		if !found {
			// 墙钟 watcher 先取消再删 Job：若「查不到」与取消同时发生，
			// 按取消处理（Exec 会把 errWallClock 归入 wall_clock 预算）；
			// 否则墙钟到点删 Job 的正常收场会被报成 job_not_found。
			if ctx.Err() != nil {
				return backend.Result{}, k8sWaitError(ctx)
			}
			return k.unknown(ns, name, "job_not_found"), nil
		}
		if job.Metadata.DeletionTimestamp != nil {
			return k.unknown(ns, name, "job_deleting"), nil
		}
		if done, res, reason := classifyK8sJob(job.Status); done {
			return k.collect(ctx, ns, name, res, reason), nil
		}
		pods, err := k.listPods(ctx, ns, name)
		if err != nil {
			return backend.Result{}, err
		}
		switch {
		case job.Status.Active > 0 && len(pods) == 0:
			// Job 认为有活着的 Pod、集群里却一个都没有：Pod 被强删/节点失联。
			return k.unknown(ns, name, "active_without_pods"), nil
		case anyPodPhaseUnknown(pods):
			return k.unknown(ns, name, "pod_phase_unknown"), nil
		case len(pods) == 0 && job.Status.Succeeded == 0 && job.Status.Failed == 0:
			if pendingSince.IsZero() {
				pendingSince = time.Now()
			} else if time.Since(pendingSince) >= k.orphanGrace {
				return k.unknown(ns, name, "no_pods"), nil
			}
		default:
			pendingSince = time.Time{}
		}
		t := time.NewTimer(k.pollInterval)
		select {
		case <-ctx.Done():
			t.Stop()
			return backend.Result{}, k8sWaitError(ctx)
		case <-t.C:
		}
	}
}

// K8s Job 终态结论的理由（本后端自己的词表）。
//
// 为什么要有名字：这些字符串与 contract 的会话状态取值同形（"succeeded" /
// "failed"），但它们说的是**K8s Job 的终态**不是会话状态——命名之后两个概念
// 在代码里显形区分（harness-word-table-handcopy 门禁的判定面）。
const (
	reasonJobComplete        = "complete"
	reasonJobFailedCondition = "failed_condition"
	reasonJobSucceeded       = "succeeded"
	reasonJobFailed          = "failed"
)

// classifyK8sJob 把 Job 状态映射成终态结论（false = 还没到终态）。
//
// 判定只认 conditions 与计数：Complete=True / succeeded>0 ⇒ 成功；
// Failed=True 或 failed>0 且无 active ⇒ 失败。条件缺失不是成功。
func classifyK8sJob(st k8sJobStatus) (bool, backend.Result, string) {
	for _, c := range st.Conditions {
		if c.Status != "True" {
			continue
		}
		switch c.Type {
		case "Failed":
			reason := c.Reason
			if reason == "" {
				reason = reasonJobFailedCondition
			}
			return true, backend.Result{OK: false}, reason
		case "Complete":
			return true, backend.Result{OK: true}, reasonJobComplete
		}
	}
	if st.Succeeded > 0 {
		return true, backend.Result{OK: true}, reasonJobSucceeded
	}
	if st.Failed > 0 && st.Active == 0 {
		return true, backend.Result{OK: false}, reasonJobFailed
	}
	return false, backend.Result{}, ""
}

// collect 终态后取 Pod 退出码与日志（读取失败只进明细，不改写终态）。
// 出站探测的实测事实（init 容器终止状态）也在这里读——探测未通过的作业
// 一定到不了业务容器，失败结论连同实测一起留痕（issue #262 验证面）。
func (k *K8sJob) collect(ctx context.Context, ns, name string, res backend.Result, reason string) backend.Result {
	detail := map[string]any{
		"job": name, "namespace": ns, "reason": reason, "log_stream": "combined",
	}
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	pods, err := k.listPods(cctx, ns, name)
	if err != nil {
		detail["pods_error"] = err.Error()
		res.Detail = detail
		return res
	}
	pod := pickK8sPod(pods)
	if pod == nil {
		if !res.OK {
			res.ExitCode = 1 // 失败结论已定，但没有 Pod 退出码：给非零值，不伪装成「没跑」
		}
		res.Detail = detail
		return res
	}
	detail["pod"] = pod.Metadata.Name
	if code, ok := terminatedExitCode(*pod); ok {
		res.ExitCode = code
	}
	if !res.OK && res.ExitCode == 0 {
		res.ExitCode = 1
	}
	logs, lerr := k.podLogs(cctx, ns, pod.Metadata.Name)
	if lerr != nil {
		detail["log_error"] = lerr.Error()
	} else {
		res.Stdout = logs
	}
	if k.k8sEgressProbeRequired() {
		detail["egress_verified"] = k8sProbeVerifiedFact(pods)
	}
	res.Detail = detail
	return res
}

// unknown 对账给不出结论：Result 层失败 + 明确 unknown 明细（不返回 error——
// 「不知道」不是「后端坏了」，但绝不能猜成成功）。
func (k *K8sJob) unknown(ns, name, reason string) backend.Result {
	return backend.Result{OK: false, Detail: map[string]any{
		"job": name, "namespace": ns, "state": "unknown", "reason": reason,
	}}
}

// pickK8sPod 优先取有终止状态的 Pod（失败作业可能留下多个 Pod）。
func pickK8sPod(pods []k8sPod) *k8sPod {
	for i := range pods {
		if _, ok := terminatedExitCode(pods[i]); ok {
			return &pods[i]
		}
	}
	if len(pods) > 0 {
		return &pods[0]
	}
	return nil
}

// terminatedExitCode 取容器终止退出码。
func terminatedExitCode(p k8sPod) (int, bool) {
	for _, cs := range p.Status.ContainerStatuses {
		if cs.State.Terminated != nil {
			return cs.State.Terminated.ExitCode, true
		}
	}
	return 0, false
}

// anyPodPhaseUnknown 节点失联的 Pod 会被标记为 Unknown——此时给结论是猜测，
// 一律按未知失败收场。
func anyPodPhaseUnknown(pods []k8sPod) bool {
	for _, p := range pods {
		if p.Status.Phase == "Unknown" {
			return true
		}
	}
	return false
}
