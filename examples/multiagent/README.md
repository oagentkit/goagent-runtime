# Multi-Agent（多专家协作）

**Handoff** 由 dispatch.Guard 白名单守卫，专家轮转协作走 blackboard.Convene；每位专家一个会话（System/Tools 会话级覆盖）。

```bash
CGO_ENABLED=0 go run ./examples/multiagent
```

tier 5 · 依赖仓：agent（tier 3，blackboard+dispatch，均 eino-free）+ goagent-kernel（tier 4）。
