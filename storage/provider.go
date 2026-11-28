// Package storage —— runtime 的**存储装配面**。
//
// # 定位：判据是 kernel 的，落库形态在这里选
//
// goagent-kernel 只给存储中立契约（checkpoint.StoreCAS / sideeffect.Store /
// eventbus.Store），不给存储。本包做三件事：
//
//  1. Provider 把 runtime 需要的存储面**汇总成一个注入点**；
//  2. NewMem 给 embedded 形态（单进程，判据移植自 kernel 参考运行时）；
//  3. sqlitestore.Open 给 standalone 形态（独立子包：modernc 驱动边不进本纯化包）（真 SQLite WAL，直接复用本仓的
//     sqliteadapter 参考实现——runtime 是装配层，import 参考实现
//     正是它的职责；kernel 仓「内核包不拖驱动」的纪律管不到这里）。
//
// 生命周期: Experimental（v0.9.0 定级基线；ADR-004 波次接入 api-lifecycle 清单）
package storage

import (
	"context"
	"time"

	"github.com/yi-nology/goagent-kernel/checkpoint"
	"github.com/yi-nology/goagent-kernel/eventbus"
	"github.com/yi-nology/goagent-kernel/sideeffect"
)

// Provider runtime 需要的存储面汇总。三个面各自独立——
// 换存储形态时只需换 Provider 的实现，loop/pipeline 的代码不动。
type Provider interface {
	// Checkpoints 进度快照面（会话历史与恢复点走这里）。
	Checkpoints() checkpoint.StoreCAS
	// Effects 副作用闸面（工具副作用的幂等/栅栏/对账）。
	Effects() sideeffect.Store
	// Events 事件持久化面；nil = 只广播不持久化（embedded 形态的默认）。
	Events() eventbus.Store
}

// JointCommitter 快照+事件单事务提交面（可选能力面，issue #2 主体）：
// 事件行与快照行同生共死——孤儿事件/终态事件丢失的崩溃窗口由结构闭合。
// Provider 实现满足该接口时（如 sqlitestore.SQLite），runtime 装配自动走联合
// 提交模式；内存形态（Events()==nil）无联合意义。
type JointCommitter interface {
	CommitWithEvents(ctx context.Context, snap checkpoint.Snapshot, events []eventbus.Event) error
}

// Mem embedded 形态的内存 Provider（单进程）。
// 判据与 kernel 参考运行时同源；跨进程部署必须换 SQLite/PG。
type Mem struct {
	snaps   *memSnapStore
	effects *memEffectStore
}

// NewMem 建内存 Provider。
func NewMem() *Mem {
	return &Mem{
		snaps:   newMemSnapStore(),
		effects: newMemEffectStore(),
	}
}

// Checkpoints 进度快照面。
func (m *Mem) Checkpoints() checkpoint.StoreCAS { return m.snaps }

// Effects 副作用闸面。
func (m *Mem) Effects() sideeffect.Store { return m.effects }

// Events 内存形态不持久化事件：返回 nil（loop 判 nil 跳过持久化）。
func (m *Mem) Events() eventbus.Store { return nil }

// UseClock 注入时钟（过期场景测试用；生产不注入读 time.Now）。
func (m *Mem) UseClock(now func() time.Time) *Mem {
	m.effects.UseClock(now)
	return m
}

// 编译期断言：内存实现满足 kernel 存储契约。
var (
	_ Provider            = (*Mem)(nil)
	_ checkpoint.StoreCAS = (*memSnapStore)(nil)
	_ checkpoint.Store    = (*memSnapStore)(nil)
	_ sideeffect.Store    = (*memEffectStore)(nil)
)
