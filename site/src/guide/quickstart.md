# 五分钟上手

## 前置

- Go 1.27+
- 不需要任何外部服务（embedded 模板全内存；standalone 模板只用一个 SQLite 文件）

## 第一步：跑 embedded 演示

```bash
git clone https://github.com/yi-nology/goagent-runtime.git
cd goagent-runtime
CGO_ENABLED=0 go run ./examples/embedded-demo
```

演示内容是：**让 Agent 调用一个会「随机失败」的工具，并在执行过程中模拟进程被杀掉、再恢复**。你会看到输出里明确写着副作用只被施加了一次——这就是「恢复不重放」。

## 第二步：跑 standalone 演示（跨进程恢复）

```bash
CGO_ENABLED=0 go run ./examples/standalone-demo
```

这个演示把会话历史落进一个 SQLite 文件（WAL 模式）。进程重启后从文件里续跑，已执行过的工具调用不会被重新执行。

## 第三步：三行代码嵌进你自己的程序

```go
rt, err := runtime.NewEmbedded(runtime.Config{
    Model: myModel,          // 实现 chat.Model 接口（一个 Chat 方法）
    Tools: []pipeline.Tool{{
        Name: "查库存", Description: "查询仓库库存",
        Handler: func(ctx context.Context, args []byte) (string, error) {
            // 你的业务逻辑
            return "A 仓：42 件", nil
        },
    }},
})
if err != nil { ... }
defer rt.Close()

session, err := rt.NewSession(ctx, runtime.SessionOptions{ID: "session-1"})
if err != nil { ... }

res, err := session.Run(ctx, "帮我查一下 A 仓库存")
// res.Status / res.Final / res.Rounds / res.ToolCalls / res.Uncertain
```

- `session-1` 是**恢复锚点**：同一 ID 的多次 `Run` 共享历史，进程重启后同 ID 续跑即从快照恢复（详见 [中断恢复](/goagent-runtime/advanced/recovery.html)）。
- 会话级配置覆盖：`SessionOptions{System / Budget / Tools / Observer}` 只作用于本会话，不改 Runtime 默认、不串到其他会话（nil = 继承）。
- 老调用方零改动：`rt.Run(ctx, sessionID, user)` 仍在（薄委托到 Session）。

`NewStandalone(cfg, "/path/to/x.db")` 把内存存储换成 SQLite 单文件，其余代码**一行不改**。

## 模型从哪来？

`chat.Model` 是供应商中立窄接口——任何模型 SDK（OpenAI / Anthropic / 自研网关 / eino）包一层适配即可接入：

```go
type Model interface {
    Chat(ctx context.Context, req chat.ChatRequest) (chat.ChatResponse, error)
}
```

本仓不 import 任何具体供应商，适配层是宿主的三五行胶水代码。

## 遇到问题？

- 工具被 `Refused`：模型请求了一个**没注册**的工具——这是 fail-closed 设计，注册上就好（见 [pipeline](/goagent-runtime/packages/pipeline.html)）。
- 结果带 `Uncertain`：远端副作用是否生效无法确认，**禁止盲目重试**（见 [中断恢复](/goagent-runtime/advanced/recovery.html)）。
