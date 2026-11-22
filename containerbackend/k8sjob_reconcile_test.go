package containerbackend

import (
	"context"
	"testing"
	"time"

	kerrors "github.com/oekit/pkg/errors"

	"github.com/oagentkit/guardx/sandbox"
	"github.com/yi-nology/goagent-kernel/backend"
)

func TestK8sExecWallClockTimeout(t *testing.T) {
	f := &fakeKube{
		statuses: []k8sJobStatus{{Active: 1}},
		pods:     []k8sPod{terminatedPod("pod-1", 0)},
	}
	k := newFakeK8sJob(t, f)
	res, err := k.Exec(context.Background(), backend.Job{
		Image:  testK8sImage,
		Budget: sandbox.Budget{WallClock: 40 * time.Millisecond},
	})
	if err != nil {
		t.Fatalf("墙钟超时应记作业失败而非基础设施故障：%v", err)
	}
	if res.OK {
		t.Fatal("超时不得算成功")
	}
	detail := detailOf(t, res)
	if detail["budget_exceeded"] != "wall_clock" {
		t.Fatalf("超时明细缺失：%+v", detail)
	}
	if snap := f.snapshot(); len(snap.jobDeletes) == 0 {
		t.Fatal("墙钟到点必须删除 Job（activeDeadlineSeconds 之外的第二道强制面）")
	}
}

func TestK8sExecUnknownOnJobNotFound(t *testing.T) {
	f := &fakeKube{jobGetStatus: 404}
	k := newFakeK8sJob(t, f)
	res, err := k.Exec(context.Background(), backend.Job{Image: testK8sImage})
	if err != nil {
		t.Fatal(err)
	}
	assertUnknown(t, res, "job_not_found")
}

func TestK8sExecUnknownOnDeletingJob(t *testing.T) {
	f := &fakeKube{statuses: []k8sJobStatus{{Active: 1}}, deleting: true}
	k := newFakeK8sJob(t, f)
	res, err := k.Exec(context.Background(), backend.Job{Image: testK8sImage})
	if err != nil {
		t.Fatal(err)
	}
	assertUnknown(t, res, "job_deleting")
}

func TestK8sExecUnknownWhenNoPodsAppear(t *testing.T) {
	f := &fakeKube{}
	k := newFakeK8sJob(t, f, WithK8sOrphanGrace(30*time.Millisecond))
	res, err := k.Exec(context.Background(), backend.Job{Image: testK8sImage})
	if err != nil {
		t.Fatal(err)
	}
	assertUnknown(t, res, "no_pods")
}

func TestK8sExecUnknownOnPodPhase(t *testing.T) {
	f := &fakeKube{
		statuses: []k8sJobStatus{{Active: 1}},
		pods: []k8sPod{{
			Metadata: k8sObjectMeta{Name: "pod-1"},
			Status:   k8sPodStatus{Phase: "Unknown"},
		}},
	}
	k := newFakeK8sJob(t, f)
	res, err := k.Exec(context.Background(), backend.Job{Image: testK8sImage})
	if err != nil {
		t.Fatal(err)
	}
	assertUnknown(t, res, "pod_phase_unknown")
}

func TestK8sNetworkPolicyFailClosed(t *testing.T) {
	f := &fakeKube{npCreateStatus: 403}
	k := newFakeK8sJob(t, f)
	_, err := k.Exec(context.Background(), backend.Job{Image: testK8sImage})
	if !kerrors.Is(err, ErrNetworkPolicy) {
		t.Fatalf("NetworkPolicy 建不出来必须响亮失败：%v", err)
	}
	if snap := f.snapshot(); len(snap.jobRaw) != 0 {
		t.Fatal("禁网无法兑现时不得创建 Job（fail-closed）")
	}
}

func TestK8sNetworkPolicyDisabled(t *testing.T) {
	f := &fakeKube{
		statuses: []k8sJobStatus{{Succeeded: 1, Conditions: []k8sJobCondition{{Type: "Complete", Status: "True"}}}},
		pods:     []k8sPod{terminatedPod("pod-1", 0)},
	}
	k := newFakeK8sJob(t, f, WithK8sNetworkPolicy(false))
	res, err := k.Exec(context.Background(), backend.Job{Image: testK8sImage})
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK {
		t.Fatalf("res=%+v", res)
	}
	if snap := f.snapshot(); len(snap.npRaw) != 0 || len(snap.npDeletes) != 0 {
		t.Fatalf("显式关闭策略后不得再调用 NetworkPolicy 接口：%d/%d", len(snap.npRaw), len(snap.npDeletes))
	}
}

func TestK8sMountDenied(t *testing.T) {
	f := &fakeKube{}
	k := newFakeK8sJob(t, f)
	_, err := k.Exec(context.Background(), backend.Job{
		Image:  testK8sImage,
		Mounts: []backend.Mount{{Source: "/etc", Target: "/dst", ReadOnly: true}},
	})
	if !kerrors.Is(err, ErrMountDenied) {
		t.Fatalf("白名单外挂载必须在创建阶段拒绝：%v", err)
	}
	if snap := f.snapshot(); len(snap.jobRaw) != 0 || len(snap.npRaw) != 0 {
		t.Fatal("挂载被拒不得触达 apiserver")
	}
}

func TestK8sBadTTL(t *testing.T) {
	f := &fakeKube{}
	k := newFakeK8sJob(t, f, WithK8sTTLSeconds(-1))
	if _, err := k.Exec(context.Background(), backend.Job{Image: testK8sImage}); err == nil {
		t.Fatal("负 TTL 必须拒绝（装配期声明错误）")
	}
	if snap := f.snapshot(); len(snap.jobRaw) != 0 || len(snap.npRaw) != 0 {
		t.Fatal("装配错误不得触达 apiserver")
	}
}

func TestK8sJobCreateFailureCleansPolicy(t *testing.T) {
	f := &fakeKube{jobCreateStatus: 422}
	k := newFakeK8sJob(t, f)
	_, err := k.Exec(context.Background(), backend.Job{Image: testK8sImage})
	if !kerrors.Is(err, ErrAPICall) {
		t.Fatalf("创建被拒属基础设施故障：%v", err)
	}
	if snap := f.snapshot(); len(snap.npDeletes) != 1 {
		t.Fatalf("Job 创建失败后必须回收已建的 NetworkPolicy：%v", snap.npDeletes)
	}
}

func TestK8sLogFailureDoesNotMaskResult(t *testing.T) {
	f := &fakeKube{
		statuses:  []k8sJobStatus{{Succeeded: 1, Conditions: []k8sJobCondition{{Type: "Complete", Status: "True"}}}},
		pods:      []k8sPod{terminatedPod("pod-1", 0)},
		logStatus: 500,
	}
	k := newFakeK8sJob(t, f)
	res, err := k.Exec(context.Background(), backend.Job{Image: testK8sImage})
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK {
		t.Fatalf("日志读取失败不得改写终态：%+v", res)
	}
	if detailOf(t, res)["log_error"] == nil {
		t.Fatalf("日志失败必须留明细：%+v", res.Detail)
	}
}

func detailOf(t *testing.T, res backend.Result) map[string]any {
	t.Helper()
	detail, ok := res.Detail.(map[string]any)
	if !ok {
		t.Fatalf("Detail 形态错误：%+v", res.Detail)
	}
	return detail
}

func assertUnknown(t *testing.T, res backend.Result, reason string) {
	t.Helper()
	if res.OK {
		t.Fatal("对账给不出结论时不得谎报成功")
	}
	detail := detailOf(t, res)
	if detail["state"] != "unknown" || detail["reason"] != reason {
		t.Fatalf("未知结论明细错误：%+v", detail)
	}
}
