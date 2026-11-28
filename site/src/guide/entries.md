# 五种入口形态

按**开发形态**选入口，而不是按部署形态——每种形态一个最小可跑样例（`go run` 即通），组合成本一眼可见。部署形态（embedded / standalone / cluster）见[模板选型](/goagent-runtime/templates/)，API 形态见[五分钟上手](/goagent-runtime/guide/quickstart.html)。

| 形态 | 适用 | 样例 | 七词侧重 |
|---|---|---|---|
| **Simple** | 最简单的模型 + 工具调用 | [`examples/simple`](https://github.com/yi-nology/goagent-runtime/tree/main/examples/simple) | Run |
| **ReAct** | 自主工具循环（模型决定下一步） | [`examples/react`](https://github.com/yi-nology/goagent-runtime/tree/main/examples/react) | Run / Turn / ToolCall |
| **Workflow** | 固定步骤、明确依赖、确定性流程 | [`examples/workflow`](https://github.com/yi-nology/goagent-runtime/tree/main/examples/workflow) | Step |
| **Multi-Agent** | 多专家协作（黑板轮转 + 派发守卫） | [`examples/multiagent`](https://github.com/yi-nology/goagent-runtime/tree/main/examples/multiagent) | Handoff |
| **Long-running** | 长任务与中断恢复 | [`examples/longrunning`](https://github.com/yi-nology/goagent-runtime/tree/main/examples/longrunning) | Interrupt / Resume |

## 跑一遍

```bash
git clone https://github.com/yi-nology/goagent-runtime.git && cd goagent-runtime
CGO_ENABLED=0 go run ./examples/simple        # 最简闭环
CGO_ENABLED=0 go run ./examples/react         # 自主多 Turn 循环
CGO_ENABLED=0 go run ./examples/workflow      # 固定 Step 拼装
CGO_ENABLED=0 go run ./examples/multiagent    # 多专家协作
CGO_ENABLED=0 go run ./examples/longrunning   # 中断 → 恢复，副作用不重放
```

## 组合成本（tier 与依赖仓）

| 形态 | tier | 上游依赖仓 | 备注 |
|---|---|---|---|
| Simple / ReAct / Workflow / Long-running | 5（本仓） | goagent-kernel（tier 4，判据） | 零外部服务、零 LLM SDK |
| Multi-Agent | 5（本仓） | + agent（tier 3：`blackboard` + `dispatch`，均 eino-free） | 主模块零 LLM SDK 边界不破 |

## 两条边界结论（成文不悬置）

- **Workflow 缺口**：样例只做**样例级拼装**（固定 Step 是普通 Go 函数顺序调用，语言步骤嵌一个 Run）。真正的定步执行器（依赖图 / 恢复语义 / 与 Loop 的组合面）正主在 [runtime#39](https://github.com/yi-nology/goagent-runtime/issues/39)（条件立项），推导见 [ADR-0010](https://github.com/yi-nology/goagent-runtime/blob/main/docs/adr/ADR-0010-执行接口两模型分立共享语义不成模型.md)——本样例不抢跑、不自造执行器。
- **Multi-Agent 装配选择**：上游 `agent/router` 是 LLM 意图分类原语（eino 耦合），装配它会把 eino 拖进主模块依赖面（破坏「主 module 零 LLM SDK」声明面）——故装配 eino-free 的 `blackboard`（共享黑板轮转协作）+ `dispatch`（Handoff 白名单守卫）。

## 关联

- 场景选型表（按场景直查）：oagentkit/matrix-tools#258（同波推进，将与本页互链）
- 七词执行语义词汇表：oagentkit/agent#175（本页样例叙事的用词基准）
