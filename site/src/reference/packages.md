---
title: 机器事实（包 × 依赖 × 能力）
---

# 机器事实（包 × 依赖 × 能力）

::: warning 本页由 `site/scripts/gen_reference_pages.mjs` 生成，勿手改
包清单、生产行数与仓外依赖取自**磁盘上的仓**（包目录 / import / go.mod）；
职责优先取自 README「包」表，缺项回落到包文档注释。
每次 `npm run build` 自动重新生成（CI 用 `--check` 判同步）。
要改事实请改源头文件——**手抄一遍就等于多一份会静默失真的真相**。
:::

## 包清单（15 个包）

依赖列只列仓外 import；「无」表示只用标准库与本仓包。
生产行数 = 非测试 .go 文件的非空行数（含注释）。
`examples/` 下是可执行示例，不计入包清单。

| 包 | 生产文件 | 生产行数 | 仓外依赖 | 职责 | 职责来源 |
|---|---|---|---|---|---|
| `adjudicate` | 1 | 234 | `github.com/oekit/pkg`、`github.com/yi-nology/goagent-kernel` | 副作用未知态的周期裁决（证据回查 → 三态判定 → 落裁决；#1） | README 包表 |
| `agentloop` | 4 | 1377 | `github.com/oagentkit/agent`、`github.com/oekit/pkg`、`github.com/yi-nology/goagent-kernel` | 可恢复回合循环（预算守卫 + 检查点 + 事件） | README 包表 |
| `bridge/einotool` | 1 | 75 | `github.com/cloudwego/eino/components/tool` | 家族钦定适配器：eino `tool.InvokableTool` → `pipeline.Tool`（副作用闸字段不猜，宿主按业务补） | README 包表 |
| `bridge/llmchat` | 1 | 217 | `github.com/cloudwego/eino/components/model`、`github.com/cloudwego/eino/schema`、`github.com/eino-contrib/jsonschema`、`github.com/oagentkit/llm` | 家族钦定适配器：`oagentkit/llm`（eino 栈）→ `chat.Model`（**嵌套模块**——LLM SDK 边只在 bridge 内，主 module 零 LLM SDK import 的声明面不变） | README 包表 |
| `chat` | 2 | 223 | 无 | 供应商中立的模型窄接口与消息类型 | README 包表 |
| `cluster` | 2 | 704 | `github.com/jackc/pgx/v5`、`github.com/oagentkit/agent`、`github.com/oekit/concurrency`、`github.com/oekit/pkg`、`github.com/yi-nology/goagent-kernel` | 多副本模板装配（PG + worker 池：认领/租约/接管代次、跨副本事件面） | README 包表 |
| `containerbackend` | 10 | 2112 | `github.com/oagentkit/guardx`、`github.com/oekit/concurrency`、`github.com/oekit/pkg`、`github.com/yi-nology/goagent-kernel` | 容器执行后端参考实现（Docker / K8s Job）——**宿主抄它，不 import 它** | README 包表 |
| `observe` | 1 | 33 | `github.com/oekit/concurrency`、`github.com/yi-nology/goagent-kernel` | eventbus → 宿主回调（薄转发，不 Interpret 语义） | README 包表 |
| `pgstore` | 21 | 3877 | `github.com/jackc/pgx/v5`、`github.com/oekit/pkg`、`github.com/yi-nology/goagent-kernel` | PostgreSQL 参考实现：11 面统一门面 + SIGKILL 故障恢复 e2e（自 kernel 迁入） | README 包表 |
| `pipeline` | 1 | 256 | `github.com/oekit/pkg`、`github.com/yi-nology/goagent-kernel` | 工具注册与副作用施加（幂等键/资源/重试安全性映射） | README 包表 |
| `runtime` | 2 | 449 | `github.com/oagentkit/agent`、`github.com/oekit/pkg`、`github.com/yi-nology/goagent-kernel` | 模板装配（embedded / standalone） | README 包表 |
| `sqliteadapter` | 21 | 4196 | `github.com/oekit/pkg`、`github.com/yi-nology/goagent-kernel`、`modernc.org/sqlite` | SQLite（WAL）参考实现：11 面统一门面（自 kernel 迁入，判据套件同源） | README 包表 |
| `storage` | 3 | 526 | `github.com/oekit/pkg`、`github.com/yi-nology/goagent-kernel` | 存储判据面（Provider 契约 + 内存实现；**零驱动边**——纯判据包不带数据库驱动，见 #10） | README 包表 |
| `storage/sqlitestore` | 1 | 55 | `github.com/yi-nology/goagent-kernel` | standalone 形态的 SQLite Provider 绑定（**驱动边只在这一包**：modernc 驱动不进 `storage` 纯判据包） | README 包表 |
| `trace` | 1 | 391 | `github.com/yi-nology/goagent-kernel` | Trace 读模型：事件流 → Session/Run/Turn/ToolCall 层级投影 + 运营八问查询（#42） | README 包表 |

## README 包表 ⇄ 磁盘目录（对账）

两处清单若对不上，**站点上就会出现两套互相矛盾的包清单**——这正是本页存在的理由。

| 包 | 在 README「包」表 | 在磁盘 |
|---|---|---|
| `adjudicate` | ✓ | ✓ |
| `agentloop` | ✓ | ✓ |
| `bridge/einotool` | ✓ | ✓ |
| `bridge/llmchat` | ✓ | ✓ |
| `chat` | ✓ | ✓ |
| `cluster` | ✓ | ✓ |
| `containerbackend` | ✓ | ✓ |
| `observe` | ✓ | ✓ |
| `pgstore` | ✓ | ✓ |
| `pipeline` | ✓ | ✓ |
| `runtime` | ✓ | ✓ |
| `sqliteadapter` | ✓ | ✓ |
| `storage` | ✓ | ✓ |
| `storage/sqlitestore` | ✓ | ✓ |
| `trace` | ✓ | ✓ |

两处清单**完全一致**（15/15）。

## 依赖（go.mod）

直接依赖 7 个，间接依赖 15 个。

| 模块 | 版本 | 直接 / 间接 |
|---|---|---|
| `github.com/jackc/pgx/v5` | `v5.13.0` | 直接 |
| `github.com/oagentkit/agent` | `v0.16.0` | 直接 |
| `github.com/oagentkit/guardx` | `v0.30.0` | 直接 |
| `github.com/oekit/concurrency` | `v0.40.0` | 直接 |
| `github.com/oekit/pkg` | `v0.61.5` | 直接 |
| `github.com/yi-nology/goagent-kernel` | `v0.80.0` | 直接 |
| `modernc.org/sqlite` | `v1.61.0` | 直接 |
| `github.com/dustin/go-humanize` | `v1.1.0` | 间接 |
| `github.com/google/uuid` | `v1.6.0` | 间接 |
| `github.com/jackc/pgpassfile` | `v1.0.0` | 间接 |
| `github.com/jackc/pgservicefile` | `v0.0.0-20240606120523-5a60cdf6a761` | 间接 |
| `github.com/jackc/puddle/v2` | `v2.2.3` | 间接 |
| `github.com/mattn/go-isatty` | `v0.0.24` | 间接 |
| `github.com/ncruces/go-strftime` | `v1.1.0` | 间接 |
| `github.com/oekit/logx` | `v0.12.1` | 间接 |
| `github.com/remyoudompheng/bigfft` | `v0.0.0-20230129092748-24d4a6f8daec` | 间接 |
| `golang.org/x/sync` | `v0.25.0` | 间接 |
| `golang.org/x/sys` | `v0.50.0` | 间接 |
| `golang.org/x/text` | `v0.44.0` | 间接 |
| `modernc.org/libc` | `v1.79.0` | 间接 |
| `modernc.org/mathutil` | `v1.7.1` | 间接 |
| `modernc.org/memory` | `v1.12.1` | 间接 |

## 底座

| 指标 | 值 |
|---|---|
| 模块路径 | `github.com/yi-nology/goagent-runtime` |
| Go 指令 | `1.27.4` |
| SQLite 驱动 | `modernc.org/sqlite`（纯 Go）⇒ `CGO_ENABLED=0` 可编 |
| 判据归属 | 状态机 / 幂等闸 / 预算 / 恢复 / 抢占接管全部来自 `goagent-kernel`，本仓只做接线 |
