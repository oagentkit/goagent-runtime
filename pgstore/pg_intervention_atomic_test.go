// pg_intervention_atomic_test.go —— ForceReleaseAtomic 参考实现的同事务判据
// （P0-A）：「释放 + 介入留痕」必须同一事务——任一失败 ⇒ 两者都不生效。
//
// 跳过与硬失败的边界与 pg_test.go 同口径：场景硬失败；唯一的 skip 在环境
// 入口（GAK_PGREF_DSN 未设置）。
package pgstore

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/oagentkit/goagent-kernel/controlplane"
	"github.com/oagentkit/goagent-kernel/taskboard"
)

// auditRowCount 留痕账行数（本测试的裁决尺子之一）。
func auditRowCount(t *testing.T, tx pgx.Tx) int64 {
	t.Helper()
	var n int64
	if err := tx.QueryRow(context.Background(),
		`SELECT count(*) FROM pgref_control_audit`).Scan(&n); err != nil {
		t.Fatalf("数留痕账失败: %v", err)
	}
	return n
}

// TestForceReleaseWithAuditAtomicCommit 成功路径：释放与留痕同一事务落库——
// 任务回 open、留痕账恰有一行，两者一个不缺（缺任何一个都是窗口的证据）。
func TestForceReleaseWithAuditAtomicCommit(t *testing.T) {
	_, board, _, pool := freshControlFixture(t)
	ctx := context.Background()
	board.Seed(taskboard.Task{ID: "t1", State: taskboard.StateOpen})
	task, err := board.Claim(ctx, "t1", "w-a", taskboard.ClaimRequest{TTL: 60})
	if err != nil {
		t.Fatal(err)
	}

	entry := controlplane.AuditEntry{
		Operator: "op", Action: controlplane.ActionForceRelease,
		Target: "t1", Basis: "PG 同事务判据",
	}
	if err := board.ForceReleaseWithAudit(ctx, "t1", task.Lease.Token, entry); err != nil {
		t.Fatalf("原子强释失败: %v", err)
	}

	tasks, err := board.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if tasks[0].State != taskboard.StateOpen {
		t.Fatalf("强释后任务应回 open，got %s", tasks[0].State)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if n := auditRowCount(t, tx); n != 1 {
		t.Fatalf("留痕账应恰有一行，got %d", n)
	}
}

// TestForceReleaseWithAuditAuditFailureRollsBack 窗口闭合判据：留痕写失败
// （operator 空串违反 NOT NULL）⇒ 事务回滚 ⇒ 释放一并撤销：任务仍 claimed、
// 留痕账零行。「介入返回 error ⇒ 可观测状态未变」在真 PG 上成立。
func TestForceReleaseWithAuditAuditFailureRollsBack(t *testing.T) {
	_, board, _, pool := freshControlFixture(t)
	ctx := context.Background()
	board.Seed(taskboard.Task{ID: "t1", State: taskboard.StateOpen})
	task, err := board.Claim(ctx, "t1", "w-a", taskboard.ClaimRequest{TTL: 60})
	if err != nil {
		t.Fatal(err)
	}

	entry := controlplane.AuditEntry{
		Operator: "", // 违反 NOT NULL ⇒ 留痕写必失败
		Action:   controlplane.ActionForceRelease,
		Target:   "t1", Basis: "PG 回滚判据",
	}
	err = board.ForceReleaseWithAudit(ctx, "t1", task.Lease.Token, entry)
	if err == nil {
		t.Fatal("留痕写失败必须上抛，不得静默成功")
	}

	tasks, lerr := board.List(ctx)
	if lerr != nil {
		t.Fatal(lerr)
	}
	if tasks[0].State != taskboard.StateClaimed {
		t.Fatalf("留痕失败 ⇒ 释放必须回滚：任务仍应 claimed，got %s", tasks[0].State)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if n := auditRowCount(t, tx); n != 0 {
		t.Fatalf("回滚后留痕账应零行，got %d", n)
	}
}

// TestForceReleaseWithAuditRejectLeavesNoAudit 拒绝路径（令牌不匹配）
// 不产生审计行：拒绝不是介入，不留痕。
func TestForceReleaseWithAuditRejectLeavesNoAudit(t *testing.T) {
	_, board, _, pool := freshControlFixture(t)
	ctx := context.Background()
	board.Seed(taskboard.Task{ID: "t1", State: taskboard.StateOpen})
	task, err := board.Claim(ctx, "t1", "w-a", taskboard.ClaimRequest{TTL: 60})
	if err != nil {
		t.Fatal(err)
	}

	entry := controlplane.AuditEntry{
		Operator: "op", Action: controlplane.ActionForceRelease,
		Target: "t1", Basis: "PG 拒绝判据",
	}
	err = board.ForceReleaseWithAudit(ctx, "t1", task.Lease.Token+"-wrong", entry)
	if err == nil {
		t.Fatal("错误令牌必须被拒")
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if n := auditRowCount(t, tx); n != 0 {
		t.Fatalf("拒绝路径不得留痕：留痕账应零行，got %d", n)
	}
}
