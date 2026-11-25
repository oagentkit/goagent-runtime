// 联合提交面：快照与事件单事务落库（runtime issue #2 主体）。
//
// 病灶：checkpoint 提交与事件落库是两个独立存储面，commit 与 emit 之间无
// 事务耦合 ⇒ 三个崩溃窗口：孤儿事件（emit 落库后 commit 失败）/ 终态事件
// 丢失（commit 后 emit 失败且被吞）/ 恢复重跑重复事件（无对账面）。两脸
// 同库时由本面结构消解：事件行与快照行**同生共死**——一个事务，要么全落
// 要么全不落。
package sqliteadapter

import (
	"context"

	"github.com/oekit/pkg/errors"

	"github.com/oagentkit/goagent-kernel/checkpoint"
	"github.com/oagentkit/goagent-kernel/eventbus"
)

// CommitWithEvents 快照与事件单事务提交。快照走 SaveIfNewer（CAS 语义：
// 陈旧快照盖不掉新进度），事件走事务内插入；任一失败整批回滚。
func (s *Store) CommitWithEvents(ctx context.Context, snap checkpoint.Snapshot, events []eventbus.Event) error {
	return s.Apply(ctx, func(tx *Tx) error {
		if err := tx.Snapshots().SaveIfNewer(ctx, snap); err != nil {
			return errors.Wrap(err, "联合提交：快照落库失败")
		}
		for _, ev := range events {
			if _, err := tx.Events().PublishInTx(ctx, tx.tx, ev); err != nil {
				return errors.Wrap(err, "联合提交：事件落库失败")
			}
		}
		return nil
	})
}
