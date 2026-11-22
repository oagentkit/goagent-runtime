package containerbackend

// K8s Job 清理的 UID 前置条件（issue #276）。
//
// 清理此前只按名字 DELETE：Job 被删后同名可被重建（稳定 ExecutionID 命名下
// 名字可预测、可重复），一次延迟清理会删掉后来者的对象。修法是用对象身份
// （metadata.uid）做删除前置条件：DELETE 带 preconditions.uid，API 在 UID
// 不符时拒绝（409）；UID 未知时宁可不删，交给 TTL 兜底。

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/yi-nology/goagent-kernel/backend"
)

func backendJob() backend.Job { return backend.Job{Image: testK8sImage} }

func TestK8sCleanupDeleteCarriesUIDPrecondition(t *testing.T) {
	f := &fakeKube{
		statuses: []k8sJobStatus{{Succeeded: 1, Conditions: []k8sJobCondition{{Type: "Complete", Status: "True"}}}},
		pods:     []k8sPod{terminatedPod("pod-1", 0)},
	}
	k := newFakeK8sJob(t, f)
	res, err := k.Exec(context.Background(), backendJob())
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK {
		t.Fatalf("res=%+v", res)
	}
	snap := f.snapshot()
	if len(snap.jobDeletes) != 1 || len(snap.jobDeleteBodies) != 1 {
		t.Fatalf("应删除一次并带请求体：deletes=%v bodies=%d", snap.jobDeletes, len(snap.jobDeleteBodies))
	}
	var opts k8sDeleteOptions
	if err := json.Unmarshal(snap.jobDeleteBodies[0], &opts); err != nil {
		t.Fatalf("DELETE 请求体不是合法 JSON: %v (%s)", err, snap.jobDeleteBodies[0])
	}
	if opts.Preconditions == nil || opts.Preconditions.UID != "uid-1" {
		t.Fatalf("删除必须带本次对象的 UID 前置条件，实得 %+v（body=%s）", opts.Preconditions, snap.jobDeleteBodies[0])
	}
	if opts.PropagationPolicy != "Background" {
		t.Fatalf("传播策略丢失：%q", opts.PropagationPolicy)
	}
}

// TestK8sDeleteRejectsUIDMismatch UID 不符 ⇒ API 拒绝删除，对象仍在。
func TestK8sDeleteRejectsUIDMismatch(t *testing.T) {
	f := &fakeKube{jobUID: "uid-new"}
	k := newFakeK8sJob(t, f)
	f.mu.Lock()
	f.created["job-x"] = map[string]string{}
	f.mu.Unlock()

	err := k.deleteJobWithUID(context.Background(), testK8sNamespace, "job-x", "uid-old")
	if err != nil {
		t.Fatalf("UID 不符应按幂等清理处理（不删），不得上抛: %v", err)
	}
	snap := f.snapshot()
	if len(snap.jobDeletes) != 0 {
		t.Fatalf("UID 不符不得删除：%v", snap.jobDeletes)
	}
	f.mu.Lock()
	_, still := f.created["job-x"]
	f.mu.Unlock()
	if !still {
		t.Fatal("UID 不符的对象必须留在集群里")
	}

	// 对照：UID 相符 ⇒ 正常删除（证明上一条不是因为删除整体坏了）。
	if err := k.deleteJobWithUID(context.Background(), testK8sNamespace, "job-x", "uid-new"); err != nil {
		t.Fatalf("UID 相符应正常删除: %v", err)
	}
	if snap := f.snapshot(); len(snap.jobDeletes) != 1 || snap.jobDeletes[0] != "job-x" {
		t.Fatalf("UID 相符应删除 job-x：%v", snap.jobDeletes)
	}
}

// TestK8sCleanupSkipsDeleteWhenUIDUnknown 创建应答读不出 UID ⇒ 不按名字删，
// 交给 TTL 兜底（宁可不清理，也不误删后来者）。
func TestK8sCleanupSkipsDeleteWhenUIDUnknown(t *testing.T) {
	f := &fakeKube{
		jobUIDOmit: true,
		statuses:   []k8sJobStatus{{Succeeded: 1, Conditions: []k8sJobCondition{{Type: "Complete", Status: "True"}}}},
		pods:       []k8sPod{terminatedPod("pod-1", 0)},
	}
	k := newFakeK8sJob(t, f)
	res, err := k.Exec(context.Background(), backendJob())
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK {
		t.Fatalf("res=%+v", res)
	}
	snap := f.snapshot()
	if len(snap.jobDeletes) != 0 {
		t.Fatalf("UID 未知时不得按名字删除：%v（bodies=%d）", snap.jobDeletes, len(snap.jobDeleteBodies))
	}
	if len(snap.npDeletes) != 1 {
		t.Fatalf("NetworkPolicy 清理不受影响：%v", snap.npDeletes)
	}
}
