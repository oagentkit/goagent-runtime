// k8sjobegress_readback_test.go —— issue #262 的「创建后读回」判据（从
// k8sjobegress_test.go 拆出：该文件已顶到 max_file_lines 门禁）。
//
// 判据本体一字未改：策略建出来不算数，必须 GET 读回并断言它就是本作业的
// Egress 全拒策略；策略缺失 / 不可达 / 被改写 / 同名他人 ⇒ 拒绝启动。
package containerbackend

import (
	"context"
	"encoding/json"
	"testing"

	kerrors "github.com/oekit/pkg/errors"

	"github.com/yi-nology/goagent-kernel/backend"
)

// TestK8sExecEgressPolicyReadBack 创建时实测校验（issue #262）：策略建出来
// 不算数，必须 GET 读回并断言它就是本作业的 Egress 全拒策略。
//
//   - 策略缺失（读回 404 / GET 失败）⇒ 拒绝启动（fail-closed）；
//   - 策略存在但被改写（mutating webhook 加放行规则）⇒ 拒绝启动；
//   - 同名策略是别人的对象 ⇒ 拒绝复用且**不得删除**别人的对象；
//   - 策略存在且形态正确（409 重试路径）⇒ 读回通过，作业照常创建。
func TestK8sExecEgressPolicyReadBack(t *testing.T) {
	okStatus := []k8sJobStatus{{Succeeded: 1, Conditions: []k8sJobCondition{{Type: "Complete", Status: "True"}}}}
	okPods := []k8sPod{probedPod("pod-1", 0, 0)}

	t.Run("既有本作业全拒策略（409）⇒ 读回通过，作业创建", func(t *testing.T) {
		name := k8sStableJobName("exec-existing")
		ours, err := json.Marshal(buildK8sNetworkPolicy(name, testK8sNamespace))
		if err != nil {
			t.Fatal(err)
		}
		f := &fakeKube{
			npCreated: map[string]json.RawMessage{name: ours},
			statuses:  okStatus,
			pods:      okPods,
		}
		k := newFakeK8sJob(t, f)
		res, err := k.Exec(context.Background(), backend.Job{Image: testK8sImage, ExecutionID: "exec-existing"})
		if err != nil {
			t.Fatalf("既有全拒策略读回通过后不得拒绝：%v", err)
		}
		if !res.OK {
			t.Fatalf("res=%+v", res)
		}
		if snap := f.snapshot(); len(snap.jobRaw) != 1 {
			t.Fatalf("读回通过后必须创建 Job：%d", len(snap.jobRaw))
		}
	})

	t.Run("创建后被改写为允许出站（webhook）⇒ 拒绝且回收本次对象", func(t *testing.T) {
		f := &fakeKube{
			// 模拟 mutating admission：落库策略被追加一条 allow-all egress。
			npStoreMutate: func(raw json.RawMessage) json.RawMessage {
				var p map[string]any
				if err := json.Unmarshal(raw, &p); err != nil {
					t.Fatal(err)
				}
				spec, _ := p["spec"].(map[string]any)
				spec["egress"] = []any{map[string]any{}}
				out, err := json.Marshal(p)
				if err != nil {
					t.Fatal(err)
				}
				return out
			},
		}
		k := newFakeK8sJob(t, f)
		_, err := k.Exec(context.Background(), backend.Job{Image: testK8sImage})
		if !kerrors.Is(err, ErrNetworkPolicy) {
			t.Fatalf("读回形态不是全拒策略必须拒绝创建作业：%v", err)
		}
		snap := f.snapshot()
		if len(snap.jobRaw) != 0 {
			t.Fatalf("读回校验失败时不得创建 Job：%d", len(snap.jobRaw))
		}
		if len(snap.npDeletes) != 1 {
			t.Fatalf("本次创建的对象必须回收：%v", snap.npDeletes)
		}
	})

	t.Run("读回失败（GET 500）⇒ 拒绝启动（fail-closed）", func(t *testing.T) {
		f := &fakeKube{npGetStatus: 500}
		k := newFakeK8sJob(t, f)
		_, err := k.Exec(context.Background(), backend.Job{Image: testK8sImage})
		if !kerrors.Is(err, ErrNetworkPolicy) {
			t.Fatalf("策略读回不可达必须拒绝启动：%v", err)
		}
		if snap := f.snapshot(); len(snap.jobRaw) != 0 {
			t.Fatalf("读回不可达时不得创建 Job：%d", len(snap.jobRaw))
		}
	})

	t.Run("读回对象身份不符（被改名）⇒ 拒绝启动（fail-closed）", func(t *testing.T) {
		// 落库对象被控制器改名：本作业名下读回的对象不是本作业的策略。
		f := &fakeKube{
			npStoreMutate: func(raw json.RawMessage) json.RawMessage {
				var p map[string]any
				if err := json.Unmarshal(raw, &p); err != nil {
					t.Fatal(err)
				}
				meta, _ := p["metadata"].(map[string]any)
				meta["name"] = "renamed-by-controller"
				out, err := json.Marshal(p)
				if err != nil {
					t.Fatal(err)
				}
				return out
			},
		}
		k := newFakeK8sJob(t, f)
		if _, err := k.Exec(context.Background(), backend.Job{Image: testK8sImage}); !kerrors.Is(err, ErrNetworkPolicy) {
			t.Fatalf("读回对象身份不符必须拒绝启动：%v", err)
		}
		if snap := f.snapshot(); len(snap.jobRaw) != 0 {
			t.Fatalf("读回对象身份不符时不得创建 Job：%d", len(snap.jobRaw))
		}
	})

	t.Run("读回不存在（404）⇒ 拒绝启动（fail-closed）", func(t *testing.T) {
		f := &fakeKube{npGetStatus: 404}
		k := newFakeK8sJob(t, f)
		if _, err := k.Exec(context.Background(), backend.Job{Image: testK8sImage}); !kerrors.Is(err, ErrNetworkPolicy) {
			t.Fatalf("读回不存在必须拒绝启动：%v", err)
		}
		if snap := f.snapshot(); len(snap.jobRaw) != 0 {
			t.Fatalf("读回不存在时不得创建 Job：%d", len(snap.jobRaw))
		}
	})

	t.Run("同名策略是别人的（允许出站）⇒ 拒绝复用且不删别人的对象", func(t *testing.T) {
		name := k8sStableJobName("exec-foreign")
		foreign := foreignNetworkPolicy(name, map[string]string{}, false, true)
		f := &fakeKube{npCreated: map[string]json.RawMessage{name: foreign}}
		k := newFakeK8sJob(t, f)
		_, err := k.Exec(context.Background(), backend.Job{Image: testK8sImage, ExecutionID: "exec-foreign"})
		if !kerrors.Is(err, ErrNetworkPolicy) {
			t.Fatalf("同名非本人策略必须拒绝复用：%v", err)
		}
		snap := f.snapshot()
		if len(snap.jobRaw) != 0 {
			t.Fatalf("拒绝复用时不得创建 Job：%d", len(snap.jobRaw))
		}
		if len(snap.npDeletes) != 0 {
			t.Fatalf("不得删除别人的 NetworkPolicy：%v", snap.npDeletes)
		}
	})
}
