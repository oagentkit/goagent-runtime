# ReAct Agent（自主工具循环）

模型自主决策：观察 → 再决策，一个 **Run** 含多个 **Turn**、每 Turn 可含 **ToolCall**。

```bash
CGO_ENABLED=0 go run ./examples/react
```

tier 5（goagent-runtime）· 依赖仓：goagent-kernel（tier 4）· 零外部服务。
