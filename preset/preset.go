// Package preset —— 五形态标准装配（matrix-tools ADR-004 (a) 路径的落地面）。
//
// 生命周期: Experimental（随 ADR-004 preset 波次引入）
//
// # 定位：真装配，不是转发
//
// 把 NewEmbedded + NewSession + Run + Close 的装配生命周期收成一次调用，
// 并给每个形态**自己的默认口径**——这是与转发门面的分界（ADR-004 §三：
// 转发不带口径，装配带）。模型、工具、系统提示、审批闸仍全由宿主注入，
// 本包不含任何领域概念（runtime 包定位不变）。
//
// # 五形态与互为锚的样例
//
//   - Simple     一次性执行（装配→会话→Run→Close 一体）——examples/simple
//   - ReAct      多轮工具循环：零值 Budget 补默认回合上限——examples/react
//   - Workflow   固定流程的「语言步骤」= Simple（定步执行器归 runtime#39，
//     本包刻意不做——流程骨架是宿主的代码，不是平台的）——examples/workflow
//   - MultiAgent 共享黑板多专家轮转：专家会话装配 + 贡献闭包 + Convene
//     ——examples/multiagent
//   - Longrunning 可恢复执行：同一 store + 同一 sessionID 再调即从快照恢复
//     ——examples/longrunning
//
// # conformance（ADR-004 §四）
//
// 五形态样例 `go run` 进 CI（零外网、脚本化模型）——样例与本包互为锚：
// preset 改坏 ⇒ 样例执行红。换模型/工具实现的扩展点套件复用
// oagentkit/llm#241 的 suite 模式（登记于 yi-nology/goagent-runtime#55，
// 套件落地时与本包双向引用）。
package preset

import (
	"context"
	"fmt"
	"strings"

	"github.com/oagentkit/agent/blackboard"

	"github.com/yi-nology/goagent-runtime/agentloop"
	"github.com/yi-nology/goagent-runtime/pipeline"
	"github.com/yi-nology/goagent-runtime/runtime"
	"github.com/yi-nology/goagent-runtime/storage"
)

// DefaultReActMaxRounds ReAct 形态零值 Budget 时补的默认回合上限。
//
// 为什么 ReAct 单独有默认而 Simple 没有：ReAct 的形态语义就是「模型自主
// 循环直到收口」——没有上限的自主循环在生产上不可验收（ADR-004 的口径
// 差异面）。宿主给了显式 Budget 则完全尊重（只填零值）。
const DefaultReActMaxRounds = 16

// DefaultMultiAgentRounds MultiAgent 形态的默认召集轮次（与样例既有口径一致）。
const DefaultMultiAgentRounds = 2

// Simple 一次性执行：装配→会话→Run→Close 一体（simple / workflow 形态的锚）。
//
// sessionID 是会话锚点（空 = 一次性执行语义由实现方裁）；Close 在返回前
// 必然发生（成功失败都关）——调用方不持有 Runtime。
func Simple(ctx context.Context, cfg runtime.Config, sessionID, user string,
	opts ...runtime.Option) (*agentloop.Result, error) {
	return run(ctx, cfg, sessionID, user, opts...)
}

// ReAct 多轮工具循环形态：与 Simple 同装配，差异只在额度口径——
// 零值 Budget 补 DefaultReActMaxRounds（宿主显式声明的上限一律尊重）。
func ReAct(ctx context.Context, cfg runtime.Config, sessionID, user string,
	opts ...runtime.Option) (*agentloop.Result, error) {
	if cfg.Budget.MaxRounds == 0 {
		cfg.Budget.MaxRounds = DefaultReActMaxRounds
	}
	return run(ctx, cfg, sessionID, user, opts...)
}

// Longrunning 可恢复执行形态：把调用方持有的存储接进装配。
//
// 恢复口径（跨「进程」恢复的装配事实）：**同一 store + 同一 sessionID**
// 再次调用即从快照恢复执行；store 的生命周期归调用方（跨多次调用传入），
// Runtime 仍是一次性的（每次调用装配、收尾即关）。
//
// 返回三元组而非二元组：本形态的状态面（中断后 exists/status/seq）必须在
// Close **之前**取——Session.Status 走 ensureOpen，关后即失效；而「中断时
// 恰恰最要看状态」是本形态的验收叙事，所以状态随结果一起带回。
// Run 出错时状态照返（中断错误 + 状态并存是本形态的常态，不是错误叠加）。
func Longrunning(ctx context.Context, cfg runtime.Config, store storage.Provider,
	sessionID, user string, opts ...runtime.Option) (*agentloop.Result, runtime.SessionStatus, error) {
	rt, err := runtime.NewEmbedded(cfg, append(opts, runtime.WithStorage(store))...)
	if err != nil {
		return nil, runtime.SessionStatus{}, err
	}
	defer func() { _ = rt.Close() }()
	session, err := rt.NewSession(ctx, runtime.SessionOptions{ID: sessionID})
	if err != nil {
		return nil, runtime.SessionStatus{}, err
	}
	res, runErr := session.Run(ctx, user)
	st, stErr := session.Status(ctx)
	if runErr != nil {
		return res, st, runErr // 状态照返：中断时状态正是要看的
	}
	if stErr != nil {
		return nil, st, stErr
	}
	return res, st, nil
}

// Simple/ReAct/Longrunning 的共同内核：装配→会话→Run→Close。
func run(ctx context.Context, cfg runtime.Config, sessionID, user string,
	opts ...runtime.Option) (*agentloop.Result, error) {
	rt, err := runtime.NewEmbedded(cfg, opts...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rt.Close() }()
	session, err := rt.NewSession(ctx, runtime.SessionOptions{ID: sessionID})
	if err != nil {
		return nil, err
	}
	return session.Run(ctx, user)
}

// Expert 多专家的一份装配：会话级 System/工具面各自一套，互不泄漏
// （W4 配置分层的直接消费）。
type Expert struct {
	// ID 会话锚点（恢复/审计按它定位该专家的执行史）。
	ID string
	// Name 黑板上的贡献者名（写入条目的 Author）。
	Name string
	// System 该专家的系统提示（会话级覆盖）。
	System string
	// Tools 该专家的工具面（会话级覆盖；nil = 不带工具）。
	Tools []pipeline.Tool
	// Filter 可选的内容谓词：Run 终稿不通过则不写入黑板（词表是宿主的，
	// preset 不解释语义——「无补充」这类判断必须由宿主注入）。nil = 总是写入。
	Filter func(final string) bool
}

// MultiAgent 共享黑板多专家轮转形态：
// 装配专家会话 → 贡献闭包（读增量 → 该专家 Run → 按 Filter 写发现）→ Convene 轮转。
//
// 拓扑守卫（dispatch.Guard）与黑板种子（Board.Seed）仍是宿主的编译期/领域
// 事实——本包只收「会话装配 + 贡献闭包 + 轮转」这段样板。
// opts 为 nil 时用 DefaultMultiAgentRounds。
func MultiAgent(ctx context.Context, cfg runtime.Config, experts []Expert,
	board *blackboard.Board, opts *blackboard.ConveneOptions) (*blackboard.ConveneResult, error) {
	if len(experts) == 0 {
		return nil, fmt.Errorf("preset: MultiAgent 至少要有一位专家")
	}
	if opts == nil {
		opts = &blackboard.ConveneOptions{MaxRounds: DefaultMultiAgentRounds}
	}
	rt, err := runtime.NewEmbedded(cfg)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rt.Close() }()

	specialists := make([]blackboard.Specialist, 0, len(experts))
	for _, e := range experts {
		session, err := rt.NewSession(ctx, runtime.SessionOptions{
			ID:     e.ID,
			System: e.System,
			Tools:  e.Tools,
		})
		if err != nil {
			return nil, err
		}
		specialists = append(specialists, blackboard.Specialist{
			Name: e.Name,
			Act: func(ctx context.Context, b *blackboard.Board, since int64) (bool, error) {
				return contribute(ctx, b, since, session, e)
			},
		})
	}
	return blackboard.Convene(ctx, board, specialists, opts)
}

// contribute 专家贡献一次：读增量 → 跑会话 Run → 按 Filter 写发现。
func contribute(ctx context.Context, b *blackboard.Board, since int64,
	session *runtime.Session, e Expert) (bool, error) {
	news := b.EntriesAfter(since)
	if len(news) == 0 {
		return false, nil // 无增量：本专家本轮无事可做
	}
	var sb strings.Builder
	for _, ent := range news {
		fmt.Fprintf(&sb, "- [%s/%s] %s\n", ent.Author, ent.Kind, ent.Content)
	}
	res, err := session.Run(ctx, "观察以下黑板增量，给出你的发现：\n"+sb.String())
	if err != nil {
		return false, err
	}
	if e.Filter != nil && !e.Filter(res.Final) {
		return false, nil
	}
	b.Write(e.Name, "finding", res.Final)
	return true, nil
}
