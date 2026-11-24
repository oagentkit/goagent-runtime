---
title: 版本记录
---

# 版本记录

::: warning 本页由 `site/scripts/gen_reference_pages.mjs` 从仓内 `CHANGELOG.md` 投影生成，勿手改
版本 / 日期 / 主题投影自仓内 `CHANGELOG.md`（该文件由家族工具 `matrix-tools/scripts/gen_changelogs.py` 从 git 历史生成）；
逐版完整变更看仓内 `CHANGELOG.md`。
:::

| 版本 | 日期 | 主题 |
|---|---|---|
| `v0.6.3` | 2026-11-24 | 新增(agentloop)：工具事件拆成执行前请求+执行后收口（批669 面2 事件映射的上游前置） |
| `v0.6.2` | 2026-11-24 | 新增(agentloop)：工具事件拆成执行前请求+执行后收口（批669 面2 事件映射的上游前置） |
| `v0.6.1` | 2026-11-23 | 重构(storage)：storage 纯化拆分——sqlite.go 移 storage/sqlitestore（modernc 驱动边不进纯化包；bianque 双 driver 注册 panic 实弹）+ *SQLite 补 CommitWithEvents 转发（批662 assemble 能力断言恒 false 缺口）+ 检测回归钉 |
| `v0.6.0` | 2026-11-23 | 新增(chat)：流式增量回调 OnDelta + agentloop content_delta 事件 |
| `v0.5.1` | 2026-11-23 | 新增(adjudicate)：逐项观测钩子 Observer + ErrNotUncertain 并发分类 |
| `v0.5.0` | 2026-11-23 | 新增(adjudicate)：副作用未知态周期裁决框架（#1） |
| `v0.4.2` | 2026-11-23 | 格式(site)：架构图按 goagent-kernel 站视觉口径重绘（分层卡片 + 单向箭头 + 图注） |
| `v0.4.1` | 2026-11-23 | 修复(site)：正文链接补 base 前缀（子路径部署下的 404）+ 内部链接门禁 |
| `v0.4.0` | 2026-11-23 | feat(cluster)：多副本高可用模板（PG + worker 池）+ 业务幂等键接缝 |
| `v0.3.0` | 2026-11-22 | feat：收编三个演示与容器后端参考实现（kernel v0.68.0 迁出） |
| `v0.2.0` | 2026-11-22 | feat：收编 pgstore 与 sqliteadapter 参考实现（自 goagent-kernel 迁入） |
| `v0.1.0` | 2026-11-21 | goagent-runtime v0.1.0：goagent-kernel 的 Agent 运行时装配层 |

> 完整变更：仓内 [CHANGELOG.md](https://github.com/yi-nology/goagent-runtime/blob/main/CHANGELOG.md)。
