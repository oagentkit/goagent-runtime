// Package pipeline —— 工具副作用的**施加管线**。
//
// # 它在装配层补的是哪一段
//
// kernel 的 sideeffect 给「同一逻辑动作至多施加一次」的判据，但「一个
// Agent 工具调用」到「一条 sideeffect.Action」之间的映射归宿主：幂等键
// 怎么构造、资源怎么划、重试安全性怎么声明。本包把这个映射固化：
//
//   - 幂等键 = 编排侧铸造的 toolCallID（恢复重放时同一调用必须同键）；
//   - 资源 = sessionID + "/" + 工具名（同会话同工具的并发施加互斥）；
//   - RetrySafety 由工具注册时显式声明（Safe = 宿主断言下游按幂等键去重，
//     断言错了就是重复施加——只能是注册者的显式选择，不能是默认值）。
//
// # 未注册即拒
//
// 模型请求未注册的工具 ⇒ OutcomeRefused，不执行任何东西。判据与
// kernel 同方向：要求无满足者即拒绝，绝不降级成「猜一个差不多的」。
package pipeline

import (
	"context"
	"encoding/json"
	"time"

	"github.com/oekit/pkg/errors"

	"github.com/yi-nology/goagent-kernel/sideeffect"

	"github.com/yi-nology/goagent-runtime/chat"
)

// Kind 一次工具执行的结果类别（判定词表，本包自有命名）。
type Kind string

const (
	// KindApplied 本次调用获得执行权并已施加。
	KindApplied Kind = "applied"
	// KindReplayed 该幂等键已完成：直接取回既有结果，**未重新施加**。
	KindReplayed Kind = "replayed"
	// KindUncertain 结果未知：该键必须先对账裁决，禁止盲目重试。
	KindUncertain Kind = "uncertain"
	// KindRefused 拒绝执行（未注册的工具）。
	KindRefused Kind = "refused"
	// KindFailed 执行体报错且闸判为明确失败（未施加，结果可安全丢弃）。
	KindFailed Kind = "failed"
	// KindContended 资源被**他人活持有**：本次没轮上，**未施加任何副作用**。
	//
	// 它不是失败，也不是「结果未知」——闸连执行权都没给，副作用确定没有发生。
	// kernel sideeffect 把它判为 OutcomeContended 且**不带 error**（正常判据
	// 之一，见 sideeffect/runner.go：刻意不返回 error）。
	//
	// 为什么单独成类：多副本下同资源的竞争是常态（接管与旧执行者重叠），
	// 装配层必须把它落成**明确结论**——既不能当成失败（会误导宿主重试已施加
	// 的动作），也不能当成未知（会误触发对账流程）。
	KindContended Kind = "contended"
)

// contendedObservation 资源竞争的回喂文本。措辞必须让模型无法误读为
// 「已执行」：副作用**确定未施加**（连执行权都没拿到）。
const contendedObservation = "工具未执行：资源正被其他执行者占用（本次未施加任何副作用），请稍后重试或改用其他方案。"

// Handler 工具执行体。args 是模型给出的参数（JSON 文本的字节形态），
// 返回要回填给模型的文本。
type Handler func(ctx context.Context, args []byte) (string, error)

// Tool 一个可注册的 Agent 工具。
type Tool struct {
	Name        string
	Description string
	// Parameters JSON Schema 文本；nil = 不声明参数模式。
	Parameters json.RawMessage
	// Safe 重试安全性声明：true = 宿主断言该工具**幂等**（同一 toolCallID
	// 重试不会重复施加副作用）。未声明（false）= 按最弱处理：施加失败即
	// 结果未知，必须先对账。
	Safe bool
	// NeedsApproval 执行前人工审批声明（true = 该工具每次调用先过
	// agentloop.Config.ApprovalGate；拒绝 ⇒ 副作用不施加，拒绝以工具观察
	// 回喂模型，模型可自行改道）。
	NeedsApproval bool
	// IdempotencyKey 业务幂等键（**可选**）：宿主按业务语义给出「同一次
	// 业务动作」的稳定键（订单号、消息 ID、批次号……）。
	//
	// 为什么需要它：默认幂等键是编排侧铸造的 toolCallID——它在**一次
	// Run 内**稳定，但模型重放同一请求时通常生成**新的** ID（供应商不
	// 保证 tool_call_id 跨生成稳定）⇒ 闸认不出这是同一次动作，会再施加
	// 一次。集群形态里「副本崩溃 → 别的副本接管重跑」正是这个场景。
	//
	// 声明了它 ⇒ 同一业务动作无论由哪个副本、哪一次 Run 施加，键都相同
	// ⇒ 闸判 replayed ⇒ 副作用不会被第二次施加（**结构性**保证，不再
	// 依赖模型恰好复述同一个 ID）。不声明 ⇒ 沿用 toolCallID（单进程内
	// 的重复调用仍受保护，跨副本接管则依赖模型提供稳定 ID）。
	//
	// 返回空串 = 装配期用法错误 ⇒ 该次调用判失败（空键会让不同动作撞键）。
	IdempotencyKey func(ctx context.Context, args []byte) (string, error)
	// ResourceKey 资源互斥键（**可选**）：宿主按业务语义给出「真正需要互斥的
	// 业务资源」（订单号、集群名、库表……）。
	//
	// 为什么需要它：kernel 的动作身份是三元组（Key / Resource / Payload），
	// 同一 Key 的 Resource **永久不可变**——同 Key 异 Resource 判
	// ErrActionConflict（不是重放）。默认资源键含 sessionID ⇒ 宿主声明了
	// 业务幂等键（同一次业务动作跨会话重试）时，跨会话重试会因 Resource
	// 不同而被判「另一个动作」⇒ 重试被拒而不是取回既有结果。
	//
	// ⇒ 声明它 ⇒ 动作身份与互斥域各归其位：同一次业务动作跨会话重试 ⇒ 同
	// Key 同 Resource ⇒ 闸判 replayed（不重复施加）；不同业务动作碰同一
	// 资源 ⇒ 互斥。
	ResourceKey func(ctx context.Context, args []byte) (string, error)
	// Handler 执行体（nil = 装配期编程错误，注册即报错）。
	Handler Handler
}

// Outcome 一次工具调用的结果。
type Outcome struct {
	Kind    Kind
	Content string // 回填给模型的工具结果文本
}

// Pipeline 工具施加管线：注册表 + sideeffect 闸。
type Pipeline struct {
	runner *sideeffect.Runner
	tools  map[string]Tool
	// names 注册顺序（tools 是 map，遍历序随机——对外列表必须稳定）。
	names []string
	specs []chat.ToolSpec
}

// New 建管线。tools 里出现重名或 nil Handler 是装配期编程错误，fail fast。
// ttl 是每次抢副作用闸时申请的租约有效期。
func New(st sideeffect.Store, ttl time.Duration, tools []Tool) (*Pipeline, error) {
	if st == nil {
		return nil, errors.New("施加管线的副作用存储不能为空（nil Store = 闸不存在，重复施加无人拦）")
	}
	p := &Pipeline{
		runner: sideeffect.NewRunner(st, ttl),
		tools:  make(map[string]Tool, len(tools)),
	}
	for _, t := range tools {
		if t.Handler == nil {
			return nil, errors.Newf("工具缺少执行体: %s", t.Name)
		}
		if _, dup := p.tools[t.Name]; dup {
			return nil, errors.Newf("工具重名注册: %s", t.Name)
		}
		p.tools[t.Name] = t
		p.names = append(p.names, t.Name)
		p.specs = append(p.specs, chat.ToolSpec{
			Name:        t.Name,
			Description: t.Description,
			Parameters:  string(t.Parameters),
		})
	}
	return p, nil
}

// Specs 暴露给模型的工具面（注册顺序稳定，prompt 稳定性）。
func (p *Pipeline) Specs() []chat.ToolSpec { return append([]chat.ToolSpec(nil), p.specs...) }

// NeedsApproval 该名字的工具是否声明了执行前人工审批（未注册的名字恒 false）。
func (p *Pipeline) NeedsApproval(name string) bool {
	t, ok := p.tools[name]
	if !ok {
		return false
	}
	return t.NeedsApproval
}

// ApprovalRequired 列出声明了执行前人工审批的工具名（注册顺序稳定）。
//
// 装配层用它做 **fail-closed** 判定：有工具要审批却没有装配审批闸 ⇒
// 装配失败，而不是「没闸就等于放行」。审批被静默绕过是安全缺陷，不是
// 配置便利。
func (p *Pipeline) ApprovalRequired() []string {
	var out []string
	for _, name := range p.names {
		if p.tools[name].NeedsApproval {
			out = append(out, name)
		}
	}
	return out
}

// Execute 施加一次工具调用。
//
// sessionID 划分资源互斥域（同会话同工具并发施加互斥）；call.ID 是幂等键——
// 恢复重放时同一调用必须带同一 ID，否则闸把它当成另一个动作。
func (p *Pipeline) Execute(ctx context.Context, sessionID string, call chat.ToolCall) Outcome {
	t, ok := p.tools[call.Name]
	if !ok {
		return Outcome{Kind: KindRefused, Content: "工具未注册：" + call.Name}
	}
	safety := sideeffect.RetrySafetyUnknown
	if t.Safe {
		safety = sideeffect.RetrySafetySafe
	}
	key := call.ID
	if t.IdempotencyKey != nil {
		k, kerr := t.IdempotencyKey(ctx, []byte(call.Arguments))
		if kerr != nil {
			return Outcome{Kind: KindFailed, Content: "业务幂等键生成失败：" + kerr.Error()}
		}
		if k == "" {
			return Outcome{Kind: KindFailed, Content: "业务幂等键为空（空键会让不同动作撞键）"}
		}
		key = k
	}
	// 资源键（互斥域 + 动作身份的一半）：
	//
	//   - 宿主声明了 ResourceKey ⇒ 用它（业务语义优先）；
	//   - 宿主声明了 IdempotencyKey（业务幂等键）⇒ 默认资源键**不含**
	//     sessionID：动作身份必须跨会话稳定，否则同一次业务动作换个会话
	//     重试就会被判成「另一个动作」（ErrActionConflict）而不是重放；
	//   - 都没声明 ⇒ 默认 sessionID + 工具名（同会话同工具互斥）。
	resource := sessionID + "/" + call.Name
	if t.ResourceKey != nil {
		rk, rerr := t.ResourceKey(ctx, []byte(call.Arguments))
		if rerr != nil {
			return Outcome{Kind: KindFailed, Content: "资源互斥键生成失败：" + rerr.Error()}
		}
		if rk == "" {
			return Outcome{Kind: KindFailed, Content: "资源互斥键为空（空键会让不同动作不互斥）"}
		}
		resource = rk
	} else if t.IdempotencyKey != nil {
		resource = "biz:" + call.Name + "/" + key
	}
	act := sideeffect.Action{
		Key:         sideeffect.Key(key),
		Resource:    resource,
		Payload:     []byte(call.Arguments),
		RetrySafety: safety,
	}
	outcome, res, err := p.runner.Execute(ctx, act, func(ctx context.Context, lease sideeffect.Lease) (sideeffect.Result, error) {
		content, herr := t.Handler(ctx, []byte(call.Arguments))
		if herr != nil {
			return sideeffect.Result{}, herr
		}
		return sideeffect.Result{Payload: []byte(content)}, nil
	})
	switch outcome {
	case sideeffect.OutcomeApplied:
		return Outcome{Kind: KindApplied, Content: string(res.Payload)}
	case sideeffect.OutcomeReplayed:
		return Outcome{Kind: KindReplayed, Content: string(res.Payload)}
	case sideeffect.OutcomeUncertain:
		return Outcome{Kind: KindUncertain, Content: "工具执行结果未知（远端是否生效无法确认），必须先对账裁决，禁止盲目重试"}
	case sideeffect.OutcomeContended:
		// 闸判「没轮上我」。**必须分两种形态**：
		//
		//   - err == nil ⇒ 正常竞争（资源被他人活持有），副作用**确定未施加**。
		//     落成 KindContended：可重试的暂不可执行，不是失败。
		//   - err != nil ⇒ 闸本身出了状况（租约已易主 / 存储不可用等），
		//     同样未施加，落成明确失败并把原因如实回填。
		//
		// ⚠️ 绝不能在这里无条件 err.Error()：kernel 的 contended 正常路径
		// **不带 error**（sideeffect/runner.go 刻意如此），盲调即空指针 Panic
		// ——而资源竞争在多副本下是常态路径。
		if err == nil {
			return Outcome{Kind: KindContended, Content: contendedObservation}
		}
		return Outcome{Kind: KindFailed, Content: "工具执行失败（未施加任何副作用）：" + err.Error()}
	default:
		// 存储侧返回了未定义的结论：显式失败，不猜。
		//
		// ⚠️ 这里同样不能盲调 err.Error()——未定义结论可能伴随 nil error
		//（盲调即 Panic）。nil 一律落成显式失败文本。
		if err == nil {
			return Outcome{Kind: KindFailed, Content: "工具执行失败：闸返回了未定义的判定（无错误详情）"}
		}
		// 明确失败（未施加）：把错误如实回填给模型，让它自己换说法或放弃。
		return Outcome{Kind: KindFailed, Content: "工具执行失败：" + err.Error()}
	}
}
