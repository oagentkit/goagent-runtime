// Package sqlitestore standalone 形态的 SQLite Provider 绑定。
//
// 为什么独立成包（批668，bianque 双 driver 实弹）：本包经 sqliteadapter
// 拉入 modernc.org/sqlite 驱动（init 注册 "sqlite" driver 名）——把这条
// 驱动边留在 storage 纯化包（Provider/Mem/契约面，零驱动依赖）里，会让
// 任何只想要内存 Provider 的消费方（如 bianque，其 repo 层已用 glebarez
// 注册同名 driver）链接期撞上「Register called twice for driver sqlite」
// 启动 panic。驱动绑定归绑定包：只要不 import 本包，就零驱动边。
package sqlitestore

import (
	"context"

	"github.com/oagentkit/goagent-kernel/checkpoint"
	"github.com/oagentkit/goagent-kernel/eventbus"
	"github.com/oagentkit/goagent-kernel/sideeffect"

	"github.com/oagentkit/goagent-runtime/sqliteadapter"
	"github.com/oagentkit/goagent-runtime/storage"
)

// SQLite standalone 形态的 Provider：真 SQLite WAL 单文件库。
// 存储本体是本仓 sqliteadapter 的参考实现（BEGIN IMMEDIATE /
// WAL / busy_timeout 那套判据已用 kernel 契约套件 + 本仓 e2e 验证过），
// 本类型只做门面转发与生命周期。
type SQLite struct {
	st *sqliteadapter.Store
}

// Open 打开（或建）一个 SQLite 后端并装配 schema。
// path 为空报错（kernel 适配器 fail fast，不静默开匿名库）。
func Open(ctx context.Context, path string) (*SQLite, error) {
	st, err := sqliteadapter.Open(path)
	if err != nil {
		return nil, err
	}
	if err := st.EnsureSchema(ctx); err != nil {
		_ = st.Close()
		return nil, err
	}
	return &SQLite{st: st}, nil
}

// Checkpoints 进度快照面。
func (s *SQLite) Checkpoints() checkpoint.StoreCAS { return s.st.Snapshots() }

// Effects 副作用闸面。
func (s *SQLite) Effects() sideeffect.Store { return s.st.Effects() }

// Events 事件持久化面。
func (s *SQLite) Events() eventbus.Store { return s.st.Events() }

// Close 关库。
func (s *SQLite) Close() error { return s.st.Close() }

// AsJoint 快照+事件单事务提交面（可选能力；sqliteadapter 参考实现两脸
// 同库 ⇒ 支持联合提交。runtime 装配检测到即走 Joint 模式）。
func (s *SQLite) AsJoint() storage.JointCommitter { return s.st }

// CommitWithEvents 快照+事件单事务提交（storage.JointCommitter 的实现——
// runtime assemble 的能力断言 `a.storage.(storage.JointCommitter)` 判的是
// **本类型的方法集**，只给 AsJoint 转发器时断言恒 false ⇒ NewStandalone
// 会静默落回两步 DurablePublisher 路径（批662 遗留缺口，本包拆分时补）。
func (s *SQLite) CommitWithEvents(ctx context.Context, snap checkpoint.Snapshot, events []eventbus.Event) error {
	return s.st.CommitWithEvents(ctx, snap, events)
}
