// finish.go —— 一次执行的**原子收尾**（结果事件 + 任务终态，同一事务）。
//
// # 为什么需要它
//
// 结果事件与任务终态是同一件事的两面。分开写（先 Insert 再 Complete）会
// 出现两类半截状态：
//
//   - 事件写失败、任务仍被完成 ⇒ 任务已是终态，等待方**永远等不到结果**；
//   - 事件写成功、任务完成失败（租约已被接管）⇒ **旧执行者的结果对外可见**，
//     等待方可能拿到过期执行者的结论。
//
// ⇒ Finish 把两者收进**同一事务**：任一失败整批回滚——任务保持可恢复状态
// （租约到期后由别的副本接管重跑），结果事件不外泄。
//
// # 判据归属
//
// 租约资格判据仍归 kernel（taskboard：令牌 + 持有者 + 状态），本包不另写；
// 本文件只提供**跨面同事务**的落点（kernel 的 taskboard 与 eventbus 是两个
// 独立存储面，跨面原子性由本参考实现在同一 PG 事务里接线）。
package pgstore

import (
	"context"

	"github.com/oekit/pkg/errors"

	"github.com/yi-nology/goagent-kernel/eventbus"
	"github.com/yi-nology/goagent-kernel/taskboard"
)

// FinishRequest 一次执行的收尾请求。
type FinishRequest struct {
	// TaskID 被收尾的任务。
	TaskID taskboard.TaskID
	// Token 认领令牌（凭它判定收尾资格——与 taskboard.Complete 同口径）。
	Token string
	// Event 结果事件（与任务终态同事务落库）。
	Event eventbus.Event
}

// Finish 一次执行的收尾：写结果事件 + 把任务标记完成，**同一事务**。
//
// 失败语义：任一环节失败 ⇒ 整批回滚（事件不落库、任务不变）⇒ 任务保持
// 可恢复状态，**不留半截状态**。
func (s *Store) Finish(ctx context.Context, req FinishRequest) error {
	if s == nil {
		return errors.New("pgstore: 收尾面未装配（nil Store）")
	}
	if req.TaskID == "" {
		return errors.New("pgstore: 收尾缺任务 ID")
	}
	if req.Token == "" {
		return errors.New("pgstore: 收尾缺令牌（无凭据 = 资格不可证）")
	}
	return s.Apply(ctx, func(ctx context.Context) error {
		// 两个面都从 ctx 取本批事务句柄 ⇒ 同一事务（面不自开事务）。
		if _, err := s.Events().Insert(ctx, req.Event); err != nil {
			return errors.Wrap(err, "pgstore: 写结果事件失败", "task", string(req.TaskID))
		}
		if err := s.Board().Complete(ctx, req.TaskID, req.Token); err != nil {
			return errors.Wrap(err, "pgstore: 完成任务失败", "task", string(req.TaskID))
		}
		return nil
	})
}
