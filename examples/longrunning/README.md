# Long-running Agent（长任务与中断恢复）

**Interrupt** 注入中断（快照已持久化）→ 同 sessionID **Resume**，副作用不重放；中途可用 `Session.Status()` 读状态。

```bash
CGO_ENABLED=0 go run ./examples/longrunning
```

tier 5 · 依赖仓：goagent-kernel（tier 4，checkpoint 判据）· 零外部服务。
