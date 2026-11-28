// Package agentiface 三入口 → oagentkit/agent/iface 窄接口的适配器
// （agent#174：Agent 接口归属裁决的 runtime 半边）。
//
// 生命周期: Experimental——随 agent#174 波次引入；0.x 阶段按 org 通行约定演进
// （api-lifecycle 推广批次：本仓在 ADR-002 §三 第二批，清单随批次接入）。
//
// # 为什么适配器在装配层（tier 5）
//
// 接口本体在 oagentkit/agent/iface（Agent 层自我描述，见该包 docstring 与
// agent 仓 ADR-001）；而 agentloop（本仓）与 contract 边界（kernel，tier 4）
// 两套语义只有 tier 5 能同时看见——bridge/ 是既定适配层惯例
// （bridge/llmchat、bridge/einotool 同族）。agentrun 的适配器在 agent 仓
// （同仓才能写编译期断言），不重复放这里。
//
// # 事件面纪律：不转发 OnEvent
//
// 两个适配器对 Input.OnEvent 非 nil 一律**显式报错**而不是静默丢弃：
// 事件面是 kernel eventbus（runtime ADR-0010：Event 共享面 = eventbus），
// 在窄接口上再开一条回调通路 = 平行事件面，正是 ADR-0010 拒收的形态。
// 要订阅过程事件，请直接订阅 eventbus。
package agentiface
