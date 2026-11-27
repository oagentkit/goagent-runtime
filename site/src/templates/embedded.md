# embedded 模板

**适用**：测试、内嵌进别的程序、一次性脚本——任何「进程退出即丢弃」的场景。

## 装配

```mermaid
sequenceDiagram
    autonumber
    participant U as 宿主
    participant RT as runtime.NewEmbedded
    participant ST as 内存存储
    participant LP as agentloop
    participant OB as observe

    U->>RT: Config{Model, Tools}
    RT->>ST: 创建内存副作用闸 + 快照存取
    RT->>LP: 装配循环（Emitter → 进程内 eventbus）
    RT->>OB: Attach（可选，宿主 Sink）
    RT-->>U: Runtime
    U->>RT: NewSession(ID) → Session
    U->>LP: session.Run(ctx, 输入)
    LP-->>U: Result
```

## 特点

- **零外部依赖**：不碰磁盘、不开端口，单元测试里随便造；
- **事件走进程内 eventbus**：同进程订阅者即时收到；
- **恢复语义只在进程存活期内成立**——退出即丢，这是 by design。

## 代码

```go
rt, err := runtime.NewEmbedded(runtime.Config{Model: m, Tools: tools})
session, err := rt.NewSession(ctx, runtime.SessionOptions{ID: "session-1"})
res, err := session.Run(ctx, "查一下 A 仓库存")
```
