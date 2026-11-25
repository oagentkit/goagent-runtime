// 联合提交面：快照与事件单事务（issue #9——#2 主体「同生共死」的 PG 形态）。
//
// 与 sqliteadapter/joint.go 同款语义：checkpoint 提交与事件落库无事务耦合
// 的崩溃窗口（孤儿事件/终态事件丢失/恢复重复）在两脸同库时由本面结构闭合。
//
// # 实现形态（与 SQLite 形态的差异）
//
// pgstore 的批作用域=Store.Apply（ctx 携带 tx+共享失败槽）：批内 SaveIfNewer
// 与 Insert 都经 runTx 认到本批 tx ⇒ **直接调用两脸方法即可**，跨面同生共
// 死由 runTx 的失败槽保证（任一面失败登记 ⇒ 整批作废）。批内禁止面自开
// 事务（独立提交破坏原子性，runTx 显式拒绝）。
package pgstore

import (
	"context"

	"github.com/oagentkit/goagent-kernel/checkpoint"
	"github.com/oagentkit/goagent-kernel/eventbus"
)

// CommitWithEvents 快照与事件单事务提交。快照走 SaveIfNewer（CAS：陈旧写
// ErrStaleSeq 且什么都没写），事件走 Insert；批内任一面失败 ⇒ 登记失败槽
// ⇒ 整批回滚。
func (s *Store) CommitWithEvents(ctx context.Context, snap checkpoint.Snapshot, events []eventbus.Event) error {
	return s.Apply(ctx, func(ctx context.Context) error {
		if err := s.Snapshots().SaveIfNewer(ctx, snap); err != nil {
			return err
		}
		es := s.Events()
		for _, ev := range events {
			if _, err := es.Insert(ctx, ev); err != nil {
				return err
			}
		}
		return nil
	})
}
