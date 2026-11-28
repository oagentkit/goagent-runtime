# Workflow Agent（固定流程）

固定 **Step** 顺序拼装（提取 → 校验 → 汇总），只在语言步骤嵌 **Run**；流程由代码说了算。

```bash
CGO_ENABLED=0 go run ./examples/workflow
```

tier 5 · 依赖仓：goagent-kernel（tier 4）· 定步执行器正主 = runtime#39（条件立项，本样例不抢跑，见 ADR-0010）。
