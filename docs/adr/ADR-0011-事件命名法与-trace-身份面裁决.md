# ADR-0011 · 事件命名法与 Trace 身份面裁决

| 项 | 内容 |
|---|---|
| **状态** | 已采纳（Accepted） |
| **日期** | 2026-11-28（W5 #42 修订输入收口：命名法裁决 + 投影表） |
| **背景** | 全栈演进总纲（2026-11-27）提议一套点号式事件词表（`session.created` / `run.started` …）；核对实况发现**三种命名形态已经并存**：① kernel eventbus **~40 个 snake_case 常量**（`session_created` / `turn_started` / `tool_call` / `llm_usage` / `approval_requested` …，机制层）；② 本仓 agentloop **6 个 `agent.*` 点号常量**（`agent.round_start` / `agent.content_delta` / `agent.tool_call` / `agent.tool_result` / `agent.round_end` / `agent.session_status`，执行层）；③ 总纲提议的点号词表（尚未落地）。W5 落 Trace 时必须裁决：新词怎么起、单源在哪、run/worker 身份进不进事件面——否则第四种形态必然长出来。 |
| **决策** | ① **全栈命名法 = snake_case**（kernel 词表风格为法）；点号式不再扩张——总纲词表作为**语义清单**吸收（它回答「该有哪些语义」，不回答「字符串长什么样」）。② **分层单源**：机制层词表（session/turn/tool/llm/approval 族）单源在 kernel eventbus，**不改名**（~40 常量破坏性 rename + 消费方连坐，拒绝）；执行层 `agent.*` 6 词是本仓既有事实面，**冻结不再新增**（新词一律 snake_case；远期与 kernel 对齐是独立的破坏性变更议题，不在本单范围）。③ **Run 身份走载荷键 `run_id`，不新造 `run_started` 事件族**——run 边界已由 round 序列 + session_status 表达，新族是有词表无信息增量；载荷键对既有事件是纯加法（「一轮四事件」判据不动、旧消费者不破）。④ **Worker 身份不进事件面**：Owner / Fence 在 kernel taskboard（判据面），投影指路 `cluster.Board().List`——观测面不复制判据面。 |
| **后果** | ① kernel 零改动、事件**类型**零新增（W5 只加载荷键）；② 未来真需要 `run_*` / `worker_*` 事件语义时，按 snake_case 落在本仓（单源 agentloop / cluster），**不进 kernel**、不建第三份词表文件；③ trace 投影是两族的**唯一合成点**（机制层词表 + 执行层词表 + 载荷键 → 统一层级视图）；④ `agent.*` 6 词与 kernel 同义词（如 `agent.tool_call` vs `tool_call`）在并存期各自成文——对齐是后续独立议题，本 ADR 不假装它们已经是同一个词。 |
| **依据** | kernel `eventbus/eventbus.go:74-176` 词表核对（~40 常量 snake_case）；本仓 `agentloop/loop.go:63-68`（6 常量 `agent.*`）；W5 实现 PR #50（run_id 载荷键 + trace 投影）；runtime#42 修订输入评论（2026-11-27，命名法裁决 + 投影表验收增补）；ADR-0002（判据不另写一套——词表同理：不建第三份）。 |

> **一句话结论**：事件命名法 snake_case 为法、`agent.*` 冻结不扩张、run 身份走载荷键不新造事件族、worker 身份留在任务板——trace 投影是词表的唯一合成点，不建第三份词表。

---

## 三种形态的实况（裁决对象）

| 形态 | 位置 | 例 | 裁决后 |
|---|---|---|---|
| 机制层 snake_case | kernel eventbus（~40 常量） | `session_created` / `turn_started` / `tool_call` / `llm_usage` / `approval_requested` | **单源不动、不改名** |
| 执行层 `agent.*` 点号 | 本仓 agentloop（6 常量） | `agent.round_start` / `agent.tool_call` / `agent.session_status` | **冻结**（不再新增；新词走 snake_case） |
| 总纲点号词表 | 未落地 | `session.created` / `run.started` | **作语义清单吸收**，不落地为命名 |

## 投影表：词汇 → 统一层级视图（验收件）

| 族 / 单源 | 词 | Trace 层级去向 |
|---|---|---|
| 执行层 / agentloop | `agent.round_start` | `Turn`（Round 序号 + 到达时刻；载荷 `usage` 进 Run 聚合） |
| 执行层 / agentloop | `agent.tool_call` | `ToolCall.StartedAt`（`call_id` / `tool`） |
| 执行层 / agentloop | `agent.tool_result` | `ToolCall` 收口（`outcome` / `bytes`；按 `call_id` 配对计时） |
| 执行层 / agentloop | `agent.session_status` | `Run.Status` / `Error` / `PendingCallID` / `EndedAt`（失败轮次 = 已观测 Turn 数 + 1） |
| 执行层 / agentloop | `agent.content_delta` | **不入层级**（观测旁路，瞬时碎片） |
| 载荷键 / agentloop | `run_id` | **切分 `Run`**（每次 Run/Approve 铸造；`Result.RunID` 同源对账） |
| 载荷键 / agentloop | `round` / `usage` / `pending_call_id` / `error` / `outcome` / `bytes` | 层级节点字段（见上） |
| 判据面 / kernel taskboard | `Owner` / `Fence` / `Token` | 问 5（哪个 Worker 接管）→ `cluster.Board().List`——**非事件** |
| 机制层 / kernel eventbus | `session_created` / `turn_started` / … | 语义清单预留（本仓 trace 暂不消费；并存期各自成文） |

## 为什么不新造 `run_started` 事件族

- **信息增量为零**：一次 Run 的开始/结束已经由「round_start 序列的起点 + session_status 收口」表达，`run_started` 只是把同一事实写第二遍；
- **破坏面真实存在**：新事件类型进 Joint 提交面要过「一轮四事件」等既有判据的审视，而载荷键是纯加法；
- **跨副本回传已走结果载荷**：cluster 的 `session.result` 载荷带 `run_id`（W5），不需要新族。

## 与「事件是事实流不是状态面」的关系

（#42 修订输入第 3 条）Trace 投影不得把事件总线当可查询状态面——八问查询全部走本 ADR 定义的读模型（`trace.Collector`），事件只进投影、不被当库查。这与 #38 的「读模型不另造状态面」是同一条纪律的两面。
