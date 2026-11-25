// control_audit.go —— 权威强释的原子接缝实现（ForceReleaseAtomic，P0-A）。
//
// BoardStore 同时满足 controlplane.TaskAuthority 与本接口：StorePlane 探测
// 到后，「凭令牌强释 + 介入留痕」收敛进**同一事务**——两步形态「释放成功、
// 留痕失败 ⇒ 租约已放而无成功审计」的窗口在此闭合。
package pgstore

import (
	"context"
	"github.com/jackc/pgx/v5"
	"github.com/oekit/pkg/errors"

	"github.com/oagentkit/goagent-kernel/controlplane"
	"github.com/oagentkit/goagent-kernel/taskboard"
)

// 编译期断言：BoardStore 满足原子强释接缝（TaskAuthority 面由 board.go 兑现）。
var _ controlplane.ForceReleaseAtomic = (*BoardStore)(nil)

// ForceReleaseWithAudit 在同一事务里：凭令牌校验并强制释放任务 + 追加
// 介入留痕行。任一失败 ⇒ 两者都不生效（释放随审计写失败一并回滚）。
//
// 拒绝路径（令牌不匹配 / 任务不在认领态）不产生审计行：拒绝不是介入，
// 不留痕——与 controlplane 非权威路径「被拒路径留痕直接追加」区分。
func (b *BoardStore) ForceReleaseWithAudit(ctx context.Context, id taskboard.TaskID, token string, entry controlplane.AuditEntry) error {
	_, err := withinTx(ctx, b.pool, func(tx pgx.Tx) (bool, error) {
		if err := releaseOrComplete(ctx, tx, id, token, string(taskboard.StateOpen)); err != nil {
			return false, err
		}
		if err := insertControlAudit(ctx, tx, entry); err != nil {
			// 审计写失败 ⇒ 返回错误 ⇒ runTx 回滚 ⇒ 上面的释放一并撤销。
			return false, errors.Wrap(err, "pgstore: 介入留痕写失败（释放已随事务回滚）", "task", string(id))
		}
		return false, nil
	})
	return err
}

// insertControlAudit 介入留痕行写入（在调用方的事务内执行）。
// 留痕时刻一律取数据库时钟 clock_timestamp()——介入方的进程时钟不可信
// （AuditEntry 契约），且租约/留痕是跨节点共享的事实，时间源必须与
// 任务板同口径（pgstore 的时钟纪律见 doc.go「时钟」段）；entry.At 忽略。
func insertControlAudit(ctx context.Context, tx pgx.Tx, entry controlplane.AuditEntry) error {
	tag, err := tx.Exec(ctx,
		`INSERT INTO pgref_control_audit (at, operator, action, target, basis, detail)
		    VALUES (clock_timestamp(), $1, $2, $3, $4, $5)`,
		entry.Operator, string(entry.Action), entry.Target, entry.Basis, entry.Detail)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return errors.Newf("pgstore: 介入留痕未落行: %s", entry.Target)
	}
	return nil
}
