// control_authority_test.go —— 权威接线的判据（controlplane.NewAuthoritativeStorePlane
// + SQLite 的控制面 / 任务板）。
//
// # 为什么必须有这条
//
// 控制面此前在 SQLite 侧**装不进**权威 plane：内核的同源校验（checkTaskSource，
// controlplane/authority.go）要求「Store 与 TaskAuthority 双方都声明来源标识
// 且相等」⇒ 只实现一方 ⇒ 装配期直接 ErrAuthorityMismatch。
//
// Task 7 补上了 TaskAuthority 那一半（BoardStore.TaskSourceID）⇒ **能装上了**
// ⇒ 但「能装上」与「装上之后强释真经任务板同一份状态」是两件事：后者零判据
// ⇒ 改签名 / 改语义 / 改来源标识的派生方式都不会红 ⇒ 与本仓评审史（Task 2 /
// Task 3 两轮因「判据缺失 ⇒ 坏实现也能过」判 FAIL）同型。
//
// # 判据（逐条对位 pgstore（v0.2.0 自 goagent-kernel 迁入）/pg_control_test.go 的同名用例）
//
//   - 同源装配通过（同一数据文件 ⇒ 来源标识一致）；
//   - 强释经任务板同一份状态：旧令牌当场作废（ErrLeaseLost）、任务回到 open、
//     任务行上的令牌被清掉；
//   - 强释留痕落地一条（权威路径的留痕走 plane.audit，不经 Store.Commit*）；
//   - 不同数据文件 ⇒ 装配期 ErrAuthorityMismatch（不是运行期才炸）。
package sqliteadapter

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/oekit/pkg/errors"

	"github.com/yi-nology/goagent-kernel/controlplane"
	"github.com/yi-nology/goagent-kernel/taskboard"
)

// openOtherStore 开一份**另一个数据文件**的后端（同源判据的反例用）。
//
// 为什么不用 openStore：那条 helper 每次都开在 t.TempDir() 下的同一个文件名
// ⇒ 两份「不同的库」必须是两个不同的路径（否则同源判据根本无从触发）。
func openOtherStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "other.db"))
	if err != nil {
		t.Fatalf("打开另一份后端失败: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.EnsureSchema(ctx); err != nil {
		t.Fatalf("建表失败: %v", err)
	}
	return s
}

// TestControlPlaneAuthoritativeWiring 权威接线：同源装配通过 + 强释经任务板
// 同一份状态（旧令牌当场作废）。
//
// 覆盖源码行：sqliteadapter（v0.2.0 自 goagent-kernel 迁入）/control.go 的 Store.Control（控制面入口）
// + BoardStore.TaskSourceID（来源标识）+ sqliteadapter（v0.2.0 自 goagent-kernel 迁入）/board.go 的
// List / sqliteadapter（v0.2.0 自 goagent-kernel 迁入）/board_lease.go 的 Release（权威任务面）。
func TestControlPlaneAuthoritativeWiring(t *testing.T) {
	ctx := tenCtx(t, "t-auth") // 介入是写路径 ⇒ 租户 fail-closed（#197）
	s := openStore(t)
	aud := &controlplane.MemAuditor{}
	ctrl := s.Control(aud)
	board := s.Board()

	// 装配：同一数据文件 ⇒ 来源标识一致 ⇒ 通过。
	plane, err := controlplane.NewAuthoritativeStorePlane(ctrl, aud, board, nil)
	if err != nil {
		t.Fatalf("同源装配必须通过: %v", err)
	}

	// 任务板认领 ⇒ 拿到令牌。
	s.Board().Seed(taskboard.Task{ID: "t1", State: taskboard.StateOpen})
	claimed, err := board.Claim(ctx, "t1", "worker-a", taskboard.ClaimRequest{TTL: time.Minute})
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}

	// 强释：权威模式直接复用运行时的任务板（**不调** Store.CommitTask）。
	if err := plane.ForceRelease(ctx, controlplane.ForceRelease{
		TaskID: "t1", Operator: "op-1", Basis: "synthetic basis",
	}); err != nil {
		t.Fatalf("ForceRelease: %v", err)
	}

	// 旧令牌当场作废（同一张表判定）。
	if err := board.Complete(ctx, "t1", claimed.Lease.Token); !errors.Is(err, taskboard.ErrLeaseLost) {
		t.Fatalf("强释后旧令牌完成必须 ErrLeaseLost（同一张表判定），实得 %v", err)
	}
	tasks, err := board.List(ctx)
	if err != nil {
		t.Fatalf("Board.List: %v", err)
	}
	if len(tasks) != 1 || tasks[0].State != taskboard.StateOpen {
		t.Fatalf("强释后任务必须回到 open: %+v", tasks)
	}
	if tasks[0].Lease.Token != "" {
		t.Fatalf("强释后旧令牌必须从任务行上消失: %+v", tasks[0].Lease)
	}

	// 强释留痕落地一条（权威路径的留痕走 plane.audit，不经 Store.Commit*）。
	entries, err := aud.Entries(ctx, "t1")
	if err != nil {
		t.Fatalf("读留痕失败: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("强释必须留痕一条，实得 %d 条", len(entries))
	}
	if entries[0].Action != controlplane.ActionForceRelease {
		t.Fatalf("留痕动作必须是强释，实得 %q", entries[0].Action)
	}
}

// TestControlPlaneAuthorityRejectsDifferentStore 不同数据文件 ⇒ 装配期
// ErrAuthorityMismatch（不是运行期才炸）。
//
// 覆盖源码行：sqliteadapter（v0.2.0 自 goagent-kernel 迁入）/store.go 的 Store.sourceID（来源标识的
// 派生）+ sqliteadapter（v0.2.0 自 goagent-kernel 迁入）/control.go 的 ControlStore.TaskSourceID。
func TestControlPlaneAuthorityRejectsDifferentStore(t *testing.T) {
	s := openStore(t)
	other := openOtherStore(t)
	aud := &controlplane.MemAuditor{}

	// 同源：控制面与任务板同一数据文件 ⇒ 装配通过。
	if _, err := controlplane.NewAuthoritativeStorePlane(
		s.Control(aud), aud, s.Board(), nil); err != nil {
		t.Fatalf("同源装配必须通过: %v", err)
	}
	// 不同源：控制面在另一个数据文件 ⇒ 装配期拒绝。
	_, err := controlplane.NewAuthoritativeStorePlane(other.Control(aud), aud, s.Board(), nil)
	if !errors.Is(err, controlplane.ErrAuthorityMismatch) {
		t.Fatalf("不同数据文件的控制面与任务板必须 ErrAuthorityMismatch，实得 %v", err)
	}
}
