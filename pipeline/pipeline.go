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
)

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
	specs  []chat.ToolSpec
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
	act := sideeffect.Action{
		Key:         sideeffect.Key(key),
		Resource:    sessionID + "/" + call.Name,
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
	default:
		// 明确失败（未施加）：把错误如实回填给模型，让它自己换说法或放弃。
		return Outcome{Kind: KindFailed, Content: "工具执行失败：" + err.Error()}
	}
}
