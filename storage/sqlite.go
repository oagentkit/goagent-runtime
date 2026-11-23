package storage

import (
	"context"

	"github.com/yi-nology/goagent-kernel/checkpoint"
	"github.com/yi-nology/goagent-kernel/eventbus"
	"github.com/yi-nology/goagent-runtime/sqliteadapter"
	"github.com/yi-nology/goagent-kernel/sideeffect"
)

// SQLite standalone 形态的 Provider：真 SQLite WAL 单文件库。
// 存储本体是本仓 sqliteadapter 的参考实现（BEGIN IMMEDIATE /
// WAL / busy_timeout 那套判据已用 kernel 契约套件 + 本仓 e2e 验证过），
// 本类型只做门面转发与生命周期。
type SQLite struct {
	st *sqliteadapter.Store
}

// OpenSQLite 打开（或建）一个 SQLite 后端并装配 schema。
// path 为空报错（kernel 适配器 fail fast，不静默开匿名库）。
func OpenSQLite(ctx context.Context, path string) (*SQLite, error) {
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
func (s *SQLite) AsJoint() JointCommitter { return s.st }
