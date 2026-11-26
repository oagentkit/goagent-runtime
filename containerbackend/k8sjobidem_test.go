// k8sjobidem_test.go —— issue #263：稳定执行幂等标识与创建前对账。
//
// 判据先于实现：本文件先于机制落地写成（红），实现转绿后固定为门禁。
// 四条验收的机判位置：
//  1. 响应丢失后重试 ⇒ 集群只有一份 Job → TestK8sExecExecutionIDResponseLostRetry
//  2. 重试返回既有作业的结果 → TestK8sExecExecutionIDResponseLostRetry
//  3. 对账先于创建：已存在时不发创建请求 → TestK8sExecExecutionIDReconcileBeforeCreate
//  4. 反向验证：去掉 ExecutionID 绑定 ⇒ 用例必须红 → TestK8sExecExecutionIDStableName
//     （绑定去掉后，重试按随机名对账必然 404 ⇒ 二次创建 ⇒ jobRaw==2 ⇒ 红）
package containerbackend

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	kerrors "github.com/oekit/pkg/errors"

	"github.com/yi-nology/goagent-kernel/backend"
)

const testExecID = "run-42-a1B2c3"

func execWithID() backend.Job {
	return backend.Job{Image: testK8sImage, ExecutionID: testExecID}
}

func TestK8sStableJobName(t *testing.T) {
	a := k8sStableJobName(testExecID)
	if a != k8sStableJobName(testExecID) {
		t.Fatal("同一执行标识必须派生出同一作业名（幂等键的前提）")
	}
	if k8sStableJobName("run-43") == a {
		t.Fatal("不同执行标识不得派生同名作业")
	}
	// 奇形怪状的标识也必须落进 DNS-1123 label 词表（小写字母数字与 -，两端非 -）。
	for _, id := range []string{"Run_42.X//y", "😀", strings.Repeat("x", 200), "-lead-trail-"} {
		name := k8sStableJobName(id)
		if len(name) > 63 || !strings.HasPrefix(name, "gak-job-") {
			t.Fatalf("派生名越界：%q (id=%q)", name, id)
		}
		for _, r := range name {
			inLabelVocab := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-'
			if !inLabelVocab {
				t.Fatalf("派生名含非法字符 %q：%q", r, name)
			}
		}
		if strings.HasPrefix(strings.TrimPrefix(name, "gak-job-"), "-") || strings.HasSuffix(name, "-") {
			t.Fatalf("派生名两端不得是 -：%q", name)
		}
	}
}

// TestK8sExecExecutionIDReconcileBeforeCreate 验收标准 3：对账查询先于创建——
// 同标识作业已存在 ⇒ 不发出创建请求，返回既有作业的结果。
func TestK8sExecExecutionIDReconcileBeforeCreate(t *testing.T) {
	f := &fakeKube{
		preseeded: map[string]fakeJobSeed{
			k8sStableJobName(testExecID): {
				status: k8sJobStatus{Succeeded: 1, Conditions: []k8sJobCondition{{Type: "Complete", Status: "True"}}},
				labels: map[string]string{k8sExecutionIDLabel: testExecID},
			},
		},
		pods: []k8sPod{terminatedPod("pod-1", 0)},
		logs: map[string]string{"pod-1": "from-existing-job\n"},
	}
	k := newFakeK8sJob(t, f)
	res, err := k.Exec(context.Background(), execWithID())
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK || res.Stdout != "from-existing-job\n" {
		t.Fatalf("必须返回既有作业的结果：res=%+v", res)
	}
	detail := detailOf(t, res)
	if detail["execution_id"] != testExecID || detail["reconciled"] != true {
		t.Fatalf("对账事实未进明细：%+v", detail)
	}
	snap := f.snapshot()
	if len(snap.jobRaw) != 0 {
		t.Fatalf("已存在时不得发出创建请求：%d", len(snap.jobRaw))
	}
	if snap.jobGets < 1 {
		t.Fatal("创建前必须先对账（GET /jobs/{稳定名}）")
	}
	if !slices.Contains(snap.getNames, k8sStableJobName(testExecID)) {
		t.Fatalf("对账没有按稳定名查询：%v", snap.getNames)
	}
}

// TestK8sExecExecutionIDResponseLostRetry 验收标准 1+2：「Job 已创建但创建响应
// 丢失」后重试 ⇒ 集群里只有一份 Job（不得出现第二个名字），且重试返回既有
// 作业的结果（而不是新建并重跑）。
func TestK8sExecExecutionIDResponseLostRetry(t *testing.T) {
	f := &fakeKube{
		createLost: true, // 第一次 Exec：POST 落对象后回 504（响应丢失）
		statuses:   []k8sJobStatus{{Succeeded: 1, Conditions: []k8sJobCondition{{Type: "Complete", Status: "True"}}}},
		pods:       []k8sPod{terminatedPod("pod-1", 0)},
		logs:       map[string]string{"pod-1": "out\n"},
	}
	k := newFakeK8sJob(t, f)

	_, err := k.Exec(context.Background(), execWithID())
	if !kerrors.Is(err, ErrAPICall) {
		t.Fatalf("响应丢失必须以基础设施故障呈现：%v", err)
	}
	snap1 := f.snapshot()
	if len(snap1.jobRaw) != 1 {
		t.Fatalf("首次创建必须只发一次请求：%d", len(snap1.jobRaw))
	}
	if len(snap1.jobDeletes) != 0 || len(snap1.npDeletes) != 0 {
		t.Fatalf("创建结果不明时不得删除：可能砍掉已建成作业的禁网面与作业本体（TTL 兜底）：%v %v",
			snap1.jobDeletes, snap1.npDeletes)
	}

	// 重试（同一执行标识）：对账命中既有作业 ⇒ 不再创建，返回其结果。
	res, err := k.Exec(context.Background(), execWithID())
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK || res.Stdout != "out\n" {
		t.Fatalf("重试必须返回既有作业的结果（而不是重跑）：res=%+v", res)
	}
	if detail := detailOf(t, res); detail["reconciled"] != true {
		t.Fatalf("重试必须走对账路径：%+v", detail)
	}
	snap2 := f.snapshot()
	if len(snap2.jobRaw) != 1 {
		t.Fatalf("重试后集群里只允许一份 Job（创建请求总共一次）：%d", len(snap2.jobRaw))
	}
	if !slices.Contains(snap2.getNames, k8sStableJobName(testExecID)) {
		t.Fatalf("重试必须按稳定名对账：%v", snap2.getNames)
	}
	if len(snap2.jobDeletes) != 1 || len(snap2.npDeletes) != 1 {
		t.Fatalf("成功收场的重试必须清理：%v %v", snap2.jobDeletes, snap2.npDeletes)
	}
}

// TestK8sExecExecutionIDStableName 验收标准 4（反向验证的载体）：同一执行标识
// 跨两次完整生命周期（第一次跑完正常收场后再重试）必须始终绑定同一个派生名。
// 去掉 ExecutionID 绑定后，第二次 Exec 的对账按随机名必然 404、创建落另一个
// 随机名 ⇒ 名字集合分裂 ⇒ 本用例红。「响应丢失后重试集群里只有一份」由
// TestK8sExecExecutionIDResponseLostRetry 承载（作业还在时的重试走对账）。
func TestK8sExecExecutionIDStableName(t *testing.T) {
	f := &fakeKube{
		statuses: []k8sJobStatus{{Succeeded: 1, Conditions: []k8sJobCondition{{Type: "Complete", Status: "True"}}}},
		pods:     []k8sPod{terminatedPod("pod-1", 0)},
	}
	k := newFakeK8sJob(t, f)
	for i := 0; i < 2; i++ {
		res, err := k.Exec(context.Background(), execWithID())
		if err != nil {
			t.Fatal(err)
		}
		if !res.OK {
			t.Fatalf("res=%+v", res)
		}
	}
	snap := f.snapshot()
	if len(snap.jobRaw) == 0 {
		t.Fatal("两次 Exec 至少各有一次创建请求")
	}
	stable := k8sStableJobName(testExecID)
	for i, raw := range snap.jobRaw {
		var job k8sJobObject
		if err := json.Unmarshal(raw, &job); err != nil {
			t.Fatal(err)
		}
		if job.Metadata.Name != stable {
			t.Fatalf("第 %d 次创建的名字脱离执行标识的派生（去掉绑定此断言必红）：%s ≠ %s",
				i+1, job.Metadata.Name, stable)
		}
	}
	for _, name := range snap.getNames {
		if name != stable {
			t.Fatalf("对账/轮询按随机名查询（去掉绑定此断言必红）：%s ≠ %s", name, stable)
		}
	}
}

// TestK8sExecExecutionIDLabelBinding 稳定标识绑定 Job 名与标签
// （harness.io/execution-id：Job 对象与 Pod 模板都带）。
func TestK8sExecExecutionIDLabelBinding(t *testing.T) {
	f := &fakeKube{
		statuses: []k8sJobStatus{{Succeeded: 1, Conditions: []k8sJobCondition{{Type: "Complete", Status: "True"}}}},
		pods:     []k8sPod{terminatedPod("pod-1", 0)},
	}
	k := newFakeK8sJob(t, f)
	if _, err := k.Exec(context.Background(), execWithID()); err != nil {
		t.Fatal(err)
	}
	var job k8sJobObject
	if err := json.Unmarshal(f.snapshot().jobRaw[0], &job); err != nil {
		t.Fatal(err)
	}
	if job.Metadata.Name != k8sStableJobName(testExecID) {
		t.Fatalf("Job 名未绑定执行标识：%s", job.Metadata.Name)
	}
	if job.Metadata.Labels[k8sExecutionIDLabel] != testExecID {
		t.Fatalf("Job 标签缺执行标识：%+v", job.Metadata.Labels)
	}
	if job.Spec.Template.Metadata.Labels[k8sExecutionIDLabel] != testExecID {
		t.Fatalf("Pod 模板标签缺执行标识：%+v", job.Spec.Template.Metadata.Labels)
	}
}

// TestK8sExecExecutionIDConflictRace 对账（404）与创建之间的竞态窗口：
// 另一进程同标识创建先到 ⇒ 创建回 409 ⇒ 视为「已存在」，等它的终态，
// 不产生第二份作业。
func TestK8sExecExecutionIDConflictRace(t *testing.T) {
	f := &fakeKube{
		jobCreateStatus: 409,
		statuses:        []k8sJobStatus{{Succeeded: 1, Conditions: []k8sJobCondition{{Type: "Complete", Status: "True"}}}},
		pods:            []k8sPod{terminatedPod("pod-1", 0)},
	}
	k := newFakeK8sJob(t, f)
	res, err := k.Exec(context.Background(), execWithID())
	if err != nil {
		t.Fatalf("409 在幂等路径上是对账命中，不是故障：%v", err)
	}
	if !res.OK {
		t.Fatalf("res=%+v", res)
	}
	if detail := detailOf(t, res); detail["reconciled"] != true {
		t.Fatalf("409 必须按对账命中处理：%+v", detail)
	}
	if len(f.snapshot().jobRaw) != 1 {
		t.Fatal("409 路径不得重发创建请求")
	}
}

// TestK8sExecExecutionIDForeignObjectRefused 对账命中同名对象但执行标识不符：
// 可能是别人的对象（散列碰撞/名字抢占），拒绝创建，绝不对着它等终态。
func TestK8sExecExecutionIDForeignObjectRefused(t *testing.T) {
	f := &fakeKube{
		preseeded: map[string]fakeJobSeed{
			k8sStableJobName(testExecID): {
				status: k8sJobStatus{Active: 1},
				labels: map[string]string{k8sExecutionIDLabel: "someone-elses-run"},
			},
		},
	}
	k := newFakeK8sJob(t, f)
	_, err := k.Exec(context.Background(), execWithID())
	if !kerrors.Is(err, ErrAPICall) {
		t.Fatalf("标识不符必须拒绝（fail-closed）：%v", err)
	}
	if snap := f.snapshot(); len(snap.jobRaw) != 0 || len(snap.npRaw) != 0 {
		t.Fatal("标识不符时不得创建任何对象")
	}
}

// TestK8sExecRandomNameConflictRejected 非幂等路径（空 ExecutionID）的 409
// 维持原判：随机名撞同名对象属基础设施异常，响亮失败，不冒充对账命中。
func TestK8sExecRandomNameConflictRejected(t *testing.T) {
	f := &fakeKube{jobCreateStatus: 409}
	k := newFakeK8sJob(t, f)
	if _, err := k.Exec(context.Background(), backend.Job{Image: testK8sImage}); !kerrors.Is(err, ErrAPICall) {
		t.Fatalf("随机名 409 必须是基础设施故障：%v", err)
	}
}
