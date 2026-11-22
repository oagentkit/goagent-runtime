// k8sjobtransport.go —— K8s Job 的传输管道：apiserver 请求骨架、状态码归一与路径。
//
// 拆分只为满足单文件 400 行阈值：载荷构造在 k8sjobwire.go，语义在 k8sjob.go。
// 命名空间一律按调用传入（禁网路径可落独立执行命名空间，issue #262）。
package containerbackend

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/oekit/pkg/errors"
)

type k8sResponse struct {
	status int
	body   []byte
}

func (k *K8sJob) call(ctx context.Context, method, path string, body any, q url.Values) (k8sResponse, error) {
	u := k.endpoint + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	var rd io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return k8sResponse{}, errors.Wrap(ErrAPICall, "containerbackend: 编码 apiserver 请求失败: "+err.Error())
		}
		rd = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return k8sResponse{}, errors.Wrap(ErrAPICall, "containerbackend: 构造 apiserver 请求失败: "+err.Error())
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	token, err := k8sBearerToken(k)
	if err != nil {
		return k8sResponse{}, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := k.client.Do(req)
	if err != nil {
		return k8sResponse{}, errors.Wrap(ErrAPICall, "containerbackend: apiserver 不可达: "+err.Error())
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return k8sResponse{}, errors.Wrap(ErrAPICall, "containerbackend: 读取 apiserver 应答失败: "+err.Error())
	}
	return k8sResponse{status: resp.StatusCode, body: raw}, nil
}

// k8sBearerToken 每请求重读 token（轮换生效）；缺省文件不存在 = 无认证。
func k8sBearerToken(k *K8sJob) (string, error) {
	if k.tokenFile == "" {
		return "", nil
	}
	b, err := os.ReadFile(k.tokenFile)
	if err != nil {
		if os.IsNotExist(err) && !k.tokenExplicit {
			return "", nil
		}
		return "", errors.Wrap(ErrAPICall, "containerbackend: 读取 service account token 失败: "+err.Error())
	}
	return strings.TrimSpace(string(b)), nil
}

// k8sAPIError 非 2xx 即基础设施故障（错误体带 apiserver 自己的解释）。
func k8sAPIError(op string, resp k8sResponse) error {
	if resp.status >= 200 && resp.status < 300 {
		return nil
	}
	msg := strings.TrimSpace(string(resp.body))
	if len(msg) > 512 {
		msg = msg[:512]
	}
	return errors.Wrap(ErrAPICall, fmt.Sprintf("containerbackend: %s 返回 %d: %s", op, resp.status, msg))
}

// createJob 在 k8sjobidem.go（创建落点分级：created/conflict/rejected/uncertain）。

func (k *K8sJob) getJob(ctx context.Context, ns, name string) (k8sJobResponse, bool, error) {
	resp, err := k.call(ctx, http.MethodGet, k8sJobsPath(ns, name), nil, nil)
	if err != nil {
		return k8sJobResponse{}, false, err
	}
	if resp.status == http.StatusNotFound {
		return k8sJobResponse{}, false, nil
	}
	if err := k8sAPIError("查询 Job", resp); err != nil {
		return k8sJobResponse{}, false, err
	}
	var out k8sJobResponse
	if err := json.Unmarshal(resp.body, &out); err != nil {
		return k8sJobResponse{}, false, errors.Wrap(ErrAPICall, "containerbackend: 解析 Job 应答失败: "+err.Error())
	}
	return out, true, nil
}

func (k *K8sJob) listPods(ctx context.Context, ns, name string) ([]k8sPod, error) {
	q := url.Values{"labelSelector": {k8sJobLabel + "=" + name}}
	resp, err := k.call(ctx, http.MethodGet, k8sPodsPath(ns), nil, q)
	if err != nil {
		return nil, err
	}
	if err := k8sAPIError("查询 Pod", resp); err != nil {
		return nil, err
	}
	var out k8sPodList
	if err := json.Unmarshal(resp.body, &out); err != nil {
		return nil, errors.Wrap(ErrAPICall, "containerbackend: 解析 Pod 应答失败: "+err.Error())
	}
	return out.Items, nil
}

func (k *K8sJob) podLogs(ctx context.Context, ns, pod string) (string, error) {
	q := url.Values{"container": {k8sJobContainerName}}
	resp, err := k.call(ctx, http.MethodGet, k8sPodsPath(ns)+"/"+url.PathEscape(pod)+"/log", nil, q)
	if err != nil {
		return "", err
	}
	if err := k8sAPIError("读取 Pod 日志", resp); err != nil {
		return "", err
	}
	return string(resp.body), nil
}

// createNetworkPolicy 建 Egress 全拒策略；同名已存在（重试路径）回 conflict=true，
// 由调用方对账验证后复用。
func (k *K8sJob) createNetworkPolicy(ctx context.Context, ns, name string) (conflict bool, err error) {
	resp, err := k.call(ctx, http.MethodPost, k8sNetworkPoliciesPath(ns, ""), buildK8sNetworkPolicy(name, ns), nil)
	if err != nil {
		return false, err // 传输失败：结果不明，调用方按 fail-closed 拒绝
	}
	if resp.status == http.StatusConflict {
		return true, k8sAPIError("创建 NetworkPolicy", resp)
	}
	return false, k8sAPIError("创建 NetworkPolicy", resp)
}

// getNetworkPolicy 读单个策略（重试路径上同名重建遇 409 后的复用校验）。
func (k *K8sJob) getNetworkPolicy(ctx context.Context, ns, name string) (k8sNetworkPolicy, bool, error) {
	resp, err := k.call(ctx, http.MethodGet, k8sNetworkPoliciesPath(ns, name), nil, nil)
	if err != nil {
		return k8sNetworkPolicy{}, false, err
	}
	if resp.status == http.StatusNotFound {
		return k8sNetworkPolicy{}, false, nil
	}
	if err := k8sAPIError("查询 NetworkPolicy", resp); err != nil {
		return k8sNetworkPolicy{}, false, err
	}
	var out k8sNetworkPolicy
	if err := json.Unmarshal(resp.body, &out); err != nil {
		return k8sNetworkPolicy{}, false, errors.Wrap(ErrAPICall, "containerbackend: 解析 NetworkPolicy 应答失败: "+err.Error())
	}
	return out, true, nil
}

// listNetworkPolicies 执行命名空间的策略面全量（严格策略装配的对账入口）。
func (k *K8sJob) listNetworkPolicies(ctx context.Context, ns string) ([]k8sNetworkPolicy, error) {
	resp, err := k.call(ctx, http.MethodGet, k8sNetworkPoliciesPath(ns, ""), nil, nil)
	if err != nil {
		return nil, err
	}
	if err := k8sAPIError("查询 NetworkPolicy", resp); err != nil {
		return nil, err
	}
	var out k8sNetworkPolicyList
	if err := json.Unmarshal(resp.body, &out); err != nil {
		return nil, errors.Wrap(ErrAPICall, "containerbackend: 解析 NetworkPolicy 应答失败: "+err.Error())
	}
	return out.Items, nil
}

// k8sDeleteOptions DELETE 请求体：清理 Job 时带 UID 前置条件（issue #276）。
//
// 用 K8s 原生 `preconditions.uid` 而不是「先查后删」的两步：两步之间对象
// 仍可被替换，前置条件是服务端在同一次删除里校验身份。
type k8sDeleteOptions struct {
	PropagationPolicy string                  `json:"propagationPolicy,omitempty"`
	Preconditions     *k8sDeletePreconditions `json:"preconditions,omitempty"`
}

type k8sDeletePreconditions struct {
	UID string `json:"uid"`
}

// deleteJobWithUID 删除本次创建的 Job，UID 不符时由 API 拒绝（409）。
//
// 404 = 已被 TTL / 控制面清掉：清理幂等；409 = 同名对象已不是本次创建的那一份
// （UID 前置条件不成立）：**不删**，这是本函数存在的全部理由。
func (k *K8sJob) deleteJobWithUID(ctx context.Context, ns, name, uid string) error {
	body := k8sDeleteOptions{
		PropagationPolicy: "Background",
		Preconditions:     &k8sDeletePreconditions{UID: uid},
	}
	resp, err := k.call(ctx, http.MethodDelete, k8sJobsPath(ns, name), body, nil)
	if err != nil {
		return err
	}
	switch resp.status {
	case http.StatusNotFound:
		return nil // 已被 TTL / 控制面清掉：清理是幂等的
	case http.StatusConflict:
		return nil // UID 不符：同名对象是后来者，拒绝删除即正确结果
	}
	return k8sAPIError("删除 Job", resp)
}

func (k *K8sJob) deleteNetworkPolicy(ctx context.Context, ns, name string) error {
	resp, err := k.call(ctx, http.MethodDelete, k8sNetworkPoliciesPath(ns, name), nil, nil)
	if err != nil {
		return err
	}
	if resp.status == http.StatusNotFound {
		return nil
	}
	return k8sAPIError("删除 NetworkPolicy", resp)
}

func k8sJobsPath(ns, name string) string {
	p := "/apis/batch/v1/namespaces/" + url.PathEscape(ns) + "/jobs"
	if name != "" {
		p += "/" + url.PathEscape(name)
	}
	return p
}

func k8sNetworkPoliciesPath(ns, name string) string {
	p := "/apis/networking.k8s.io/v1/namespaces/" + url.PathEscape(ns) + "/networkpolicies"
	if name != "" {
		p += "/" + url.PathEscape(name)
	}
	return p
}

func k8sPodsPath(ns string) string {
	return "/api/v1/namespaces/" + url.PathEscape(ns) + "/pods"
}
