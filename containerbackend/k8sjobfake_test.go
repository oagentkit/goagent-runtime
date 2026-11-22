// k8sjobfake_test.go —— 假 kube-apiserver：与 docker.go 的假引擎同款手法，
// 用 httptest 记录请求体与调用次数，按脚本回 Job 状态。
package containerbackend

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

const (
	testK8sNamespace     = "team-a"
	testK8sExecNamespace = "exec-iso"
	testK8sSA            = "runner-sa"
	testK8sImage         = "registry.example/app@sha256:deadbeef"

	testK8sProbeImage = "registry.example/probe@sha256:feedface"
)

var testK8sProbeCommand = []string{"/probe", "-target", "https://egress.example/ping", "-expect-blocked"}

// fakeKube 只实现 K8sJob 用到的路径；未注册路径由 mux 回 404。
type fakeKube struct {
	mu sync.Mutex

	jobRaw     [][]byte
	npRaw      [][]byte
	jobDeletes []string
	npDeletes  []string
	// jobDeleteBodies DELETE 请求体（issue #276：断言 preconditions.uid）。
	jobDeleteBodies [][]byte
	getCalls        int
	jobNS           string
	npNS            string
	podsNS          string

	// jobUID 作业对象 UID；空 = "uid-1"。jobUIDOmit 让创建应答不带 uid
	// （读不出 UID ⇒ 显式清理必须退化到 TTL，不按名字删）。
	jobUID     string
	jobUIDOmit bool

	npCreateStatus  int
	npGetStatus     int // 非 0 ⇒ 单读 NetworkPolicy 回该状态码（读回校验失败用例）
	jobCreateStatus int
	jobGetStatus    int // 非 0 ⇒ 每次 GET 都回该状态码（404 用例）
	podsStatus      int
	logStatus       int
	npListStatus    int

	statuses []k8sJobStatus // 按 GET 次序取；耗尽后重复最后一项
	pods     []k8sPod
	logs     map[string]string
	deleting bool

	foreignNPs []json.RawMessage // LIST 回给后端的「别人家」策略（叠加语义用例）

	// ── 幂等创建（issue #263）的仿真面 ──
	preseeded map[string]fakeJobSeed       // Exec 之前就存在的 Job（对账命中用例）
	created   map[string]map[string]string // POST 过的 Job（名 → 标签），含响应丢失场景
	getNames  []string                     // GET /jobs/{name} 的名字轨迹（稳定名断言）
	jobGets   int
	npCreated map[string]json.RawMessage // 已存在的 NetworkPolicy（同名重建回 409）
	// npStoreMutate 模拟 mutating admission webhook / 控制器对策略的改写：
	// POST 成功后、GET 读回前改写落库形态（读回校验用例）。
	npStoreMutate func(json.RawMessage) json.RawMessage
	createLost    bool // POST Job：先落对象再回 504（创建响应丢失）
}

// fakeJobSeed 预置 Job 的对账应答。
type fakeJobSeed struct {
	status k8sJobStatus
	labels map[string]string
}

func (f *fakeKube) handler() http.Handler {
	// 幂等仿真面（issue #263）的映射表就地初始化：测试用 &fakeKube{...} 直接
	// 构造，零值 nil map 的写入会在持锁段里 panic——锁永不释放，整站假死。
	f.mu.Lock()
	if f.created == nil {
		f.created = map[string]map[string]string{}
	}
	if f.npCreated == nil {
		f.npCreated = map[string]json.RawMessage{}
	}
	f.mu.Unlock()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /apis/networking.k8s.io/v1/namespaces/{ns}/networkpolicies", func(w http.ResponseWriter, r *http.Request) {
		raw := readBody(r)
		f.mu.Lock()
		f.npRaw = append(f.npRaw, raw)
		f.npNS = r.PathValue("ns")
		st := f.npCreateStatus
		_, exists := f.npCreated[bodyName(raw)]
		f.mu.Unlock()
		if st != 0 {
			writeStatus(w, st, "networkpolicy rejected")
			return
		}
		if exists {
			writeStatus(w, http.StatusConflict, "networkpolicy already exists")
			return
		}
		f.mu.Lock()
		mutate := f.npStoreMutate
		f.mu.Unlock()
		stored := raw
		if mutate != nil {
			stored = mutate(raw)
		}
		f.mu.Lock()
		f.npCreated[bodyName(raw)] = stored
		f.mu.Unlock()
		writeJSON(w, http.StatusCreated, map[string]any{"metadata": map[string]any{"name": bodyName(raw)}})
	})
	mux.HandleFunc("GET /apis/networking.k8s.io/v1/namespaces/{ns}/networkpolicies", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		items := append([]json.RawMessage(nil), f.foreignNPs...)
		for _, raw := range f.npCreated { // 已存在的策略同真集群一样出现在 LIST 里
			items = append(items, raw)
		}
		code := f.npListStatus
		f.mu.Unlock()
		if code != 0 {
			writeStatus(w, code, "networkpolicies rejected")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": items})
	})
	mux.HandleFunc("GET /apis/networking.k8s.io/v1/namespaces/{ns}/networkpolicies/{name}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		raw, ok := f.npCreated[r.PathValue("name")]
		code := f.npGetStatus
		f.mu.Unlock()
		if code != 0 {
			writeStatus(w, code, "networkpolicy get rejected")
			return
		}
		if !ok {
			writeStatus(w, http.StatusNotFound, "networkpolicy not found")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(raw)
	})
	mux.HandleFunc("POST /apis/batch/v1/namespaces/{ns}/jobs", func(w http.ResponseWriter, r *http.Request) {
		raw := readBody(r)
		f.mu.Lock()
		f.jobRaw = append(f.jobRaw, raw)
		f.jobNS = r.PathValue("ns")
		st := f.jobCreateStatus
		lost := f.createLost
		uid, omitUID := f.uidLocked(), f.jobUIDOmit
		f.mu.Unlock()
		if lost {
			// 服务端已受理（对象落集群），响应丢失：幂等创建的整条验收线都骑在这上面。
			f.mu.Lock()
			f.created[bodyName(raw)] = bodyLabels(raw)
			f.mu.Unlock()
			writeStatus(w, http.StatusGatewayTimeout, "job created but response lost")
			return
		}
		if st != 0 {
			if st == http.StatusConflict {
				// 409 = 同名对象已存在：对账视图里它存在（对账→创建的竞态窗口）。
				f.mu.Lock()
				f.created[bodyName(raw)] = bodyLabels(raw)
				f.mu.Unlock()
			}
			writeStatus(w, st, "job rejected")
			return
		}
		f.mu.Lock()
		f.created[bodyName(raw)] = bodyLabels(raw)
		f.mu.Unlock()
		meta := map[string]any{"name": bodyName(raw)}
		if !omitUID {
			meta["uid"] = uid
		}
		writeJSON(w, http.StatusCreated, map[string]any{"metadata": meta})
	})
	mux.HandleFunc("GET /apis/batch/v1/namespaces/{ns}/jobs/{name}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.jobGets++
		f.getNames = append(f.getNames, r.PathValue("name"))
		name := r.PathValue("name")
		code, deleting := f.jobGetStatus, f.deleting
		uid := f.uidLocked()
		seed, pre := f.preseeded[name]
		labels, created := f.created[name]
		f.mu.Unlock()
		if code != 0 {
			writeStatus(w, code, "job not found")
			return
		}
		if !created && !pre {
			// 集群里没有这个对象：对账/轮询都必须看到 404，不许拿脚本状态冒充。
			writeStatus(w, http.StatusNotFound, "job not found")
			return
		}
		var st k8sJobStatus
		if pre {
			st = seed.status
			labels = seed.labels
		} else {
			f.mu.Lock()
			f.getCalls++
			idx := f.getCalls - 1
			if len(f.statuses) > 0 {
				if idx >= len(f.statuses) {
					idx = len(f.statuses) - 1
				}
				st = f.statuses[idx]
			}
			f.mu.Unlock()
		}
		meta := map[string]any{"name": name, "uid": uid}
		if len(labels) > 0 {
			meta["labels"] = labels
		}
		if deleting {
			meta["deletionTimestamp"] = "2026-01-01T00:00:00Z"
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"apiVersion": "batch/v1", "kind": "Job", "metadata": meta, "status": st,
		})
	})
	mux.HandleFunc("DELETE /apis/batch/v1/namespaces/{ns}/jobs/{name}", func(w http.ResponseWriter, r *http.Request) {
		raw := readBody(r)
		f.mu.Lock()
		f.jobDeleteBodies = append(f.jobDeleteBodies, raw)
		uid := f.uidLocked()
		var opts k8sDeleteOptions
		_ = json.Unmarshal(raw, &opts)
		if opts.Preconditions != nil && opts.Preconditions.UID != "" && opts.Preconditions.UID != uid {
			f.mu.Unlock()
			// 真 apiserver 的 preconditions.uid 不成立时回 409，且不删对象。
			writeStatus(w, http.StatusConflict, "Precondition failed: uid mismatch")
			return
		}
		f.jobDeletes = append(f.jobDeletes, r.PathValue("name"))
		delete(f.created, r.PathValue("name")) // 集群状态如实：删了就是没了
		f.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"status": "Success"})
	})
	mux.HandleFunc("DELETE /apis/networking.k8s.io/v1/namespaces/{ns}/networkpolicies/{name}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.npDeletes = append(f.npDeletes, r.PathValue("name"))
		delete(f.npCreated, r.PathValue("name"))
		f.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"status": "Success"})
	})
	mux.HandleFunc("GET /api/v1/namespaces/{ns}/pods", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		pods := append([]k8sPod(nil), f.pods...)
		f.podsNS = r.PathValue("ns")
		code := f.podsStatus
		f.mu.Unlock()
		if code != 0 {
			writeStatus(w, code, "pods rejected")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": pods})
	})
	mux.HandleFunc("GET /api/v1/namespaces/{ns}/pods/{pod}/log", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		text, ok := f.logs[r.PathValue("pod")]
		code := f.logStatus
		f.mu.Unlock()
		if code != 0 {
			writeStatus(w, code, "logs rejected")
			return
		}
		if !ok {
			text = "log:" + r.PathValue("pod")
		}
		_, _ = w.Write([]byte(text))
	})
	return mux
}

func (f *fakeKube) uidLocked() string {
	if f.jobUID == "" {
		return "uid-1"
	}
	return f.jobUID
}

type fakeKubeSnapshot struct {
	jobRaw          [][]byte
	npRaw           [][]byte
	jobDeletes      []string
	npDeletes       []string
	jobDeleteBodies [][]byte
	getCalls        int
	jobNS           string
	npNS            string
	podsNS          string
	jobGets         int
	getNames        []string
}

func (f *fakeKube) snapshot() fakeKubeSnapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	return fakeKubeSnapshot{
		jobRaw:          append([][]byte(nil), f.jobRaw...),
		npRaw:           append([][]byte(nil), f.npRaw...),
		jobDeletes:      append([]string(nil), f.jobDeletes...),
		npDeletes:       append([]string(nil), f.npDeletes...),
		jobDeleteBodies: append([][]byte(nil), f.jobDeleteBodies...),
		getCalls:        f.getCalls,
		jobNS:           f.jobNS,
		npNS:            f.npNS,
		podsNS:          f.podsNS,
		jobGets:         f.jobGets,
		getNames:        append([]string(nil), f.getNames...),
	}
}

func newFakeK8sJob(t *testing.T, f *fakeKube, extra ...K8sJobOption) *K8sJob {
	t.Helper()
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	opts := []K8sJobOption{
		WithK8sEndpoint(srv.URL),
		WithK8sNamespace(testK8sNamespace),
		WithK8sServiceAccount(testK8sSA),
		WithK8sPollInterval(2 * time.Millisecond),
		WithK8sTTLSeconds(45),
		WithK8sMountPolicy(MountPolicy{AllowedSources: []string{"/src"}}),
		// 禁网档全链缺省装配（能力事实 + 出站探测）：既有 Exec 用例默认走
		// 完整的禁网路径；专门测「缺一半」的用例另起装配。
		WithK8sCNICapability(stubCNICapability{ok: true}),
		WithK8sEgressProbe(K8sEgressProbe{Image: testK8sProbeImage, Command: testK8sProbeCommand}),
	}
	return NewK8sJob(append(opts, extra...)...)
}

// newFakeK8sJobNoProbe 有能力事实但**没有**出站探测的装配（声明面必须降级）。
func newFakeK8sJobNoProbe(t *testing.T, f *fakeKube, extra ...K8sJobOption) *K8sJob {
	t.Helper()
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	opts := []K8sJobOption{
		WithK8sEndpoint(srv.URL),
		WithK8sNamespace(testK8sNamespace),
		WithK8sServiceAccount(testK8sSA),
		WithK8sPollInterval(2 * time.Millisecond),
		WithK8sTTLSeconds(45),
		WithK8sMountPolicy(MountPolicy{AllowedSources: []string{"/src"}}),
		WithK8sCNICapability(stubCNICapability{ok: true}),
	}
	return NewK8sJob(append(opts, extra...)...)
}

func readBody(r *http.Request) []byte {
	b, _ := io.ReadAll(r.Body)
	return b
}

func bodyName(raw []byte) string {
	var meta struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
	}
	_ = json.Unmarshal(raw, &meta)
	return meta.Metadata.Name
}

func bodyLabels(raw []byte) map[string]string {
	var meta struct {
		Metadata struct {
			Labels map[string]string `json:"labels"`
		} `json:"metadata"`
	}
	_ = json.Unmarshal(raw, &meta)
	return meta.Metadata.Labels
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	b, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(b)
}

func writeStatus(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"message": msg})
}
