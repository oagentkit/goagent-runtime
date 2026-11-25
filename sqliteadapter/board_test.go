// board_test.go —— 任务面（taskboard.Board / taskboard.RenewingBoard）的契约判据。
//
// # 主判据是共享契约套件
//
// boardtest 全套（含 4 / 10 / 100 并发认领档、令牌铸造、过期接管、续期、
// 词表外状态拒绝、快照隔离）——与 pgstore 跑的是**同一段判据**，
// 实现各写一份、判据只有一份。
//
// # 本文件另补三条套件覆盖不到的落库形态判据
//
//   - **独立读路径走只读句柄**：写事务占住写句柄的唯一连接期间，独立读路径
//     仍要能查（走写句柄会挂到 ctx 超时——实测 2s deadline exceeded）。
//   - **只读路径读到的是已提交快照**：看不到未提交数据（与 PG 的 MVCC 快照
//     语义一致，这才是真正要对齐的东西）。
//   - **接管代次是持久化水位**：重开库后下一次认领的代次仍须大于历史最大代次。
//
// 所有新用例的 ctx 都带超时（单连接池下回归的表现是「挂死」而不是干净的
// 断言失败⇒带超时才能快速红、CI 上好定位）。
package sqliteadapter

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/oagentkit/goagent-kernel/taskboard"
	"github.com/oagentkit/goagent-kernel/taskboard/boardtest"
)

// boardFactory 契约套件的工厂：每个子测试调一次，返回全新实现。
func boardFactory(t *testing.T, _ *boardtest.FakeClock) boardtest.ClockedBoard {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("打开失败: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.EnsureSchema(t.Context()); err != nil {
		t.Fatalf("建表失败: %v", err)
	}
	return s.Board()
}

// TestBoardContract 跑任务板的共享契约套件（boardtest 全套，含 10 与 100
// 并发认领档）。
func TestBoardContract(t *testing.T) {
	boardtest.Run(t, boardFactory)
}

// TestBoardSeedPersistsAcrossReopen 摆进去的任务在重开后仍在（持久化判据）。
func TestBoardSeedPersistsAcrossReopen(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	path := filepath.Join(t.TempDir(), "state.db")

	s, err := Open(path)
	if err != nil {
		t.Fatalf("打开失败: %v", err)
	}
	if err := s.EnsureSchema(ctx); err != nil {
		t.Fatalf("建表失败: %v", err)
	}
	s.Board().Seed(taskboard.Task{ID: "t1", State: taskboard.StateOpen})
	if err := s.Close(); err != nil {
		t.Fatalf("关闭失败: %v", err)
	}

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("重开失败: %v", err)
	}
	defer func() { _ = s2.Close() }()
	tasks, err := s2.Board().List(ctx)
	if err != nil {
		t.Fatalf("读任务失败: %v", err)
	}
	if len(tasks) != 1 || tasks[0].ID != "t1" {
		t.Fatalf("重开后任务丢失: %+v", tasks)
	}
}

// TestBoardSeedSeqIncrementsAndKeepsFence 同 id 重摆：序号**递增**、代次不动。
//
// 两条都是「静默失效」形态，写错了没有任何信号：
//   - seq 覆盖 ⇒ 同一个序号反复出现 ⇒ 「这次任务有没有被改动」判不出来；
//   - fence 被 Seed 清零 ⇒ 代次不再是单调水位 ⇒ 迟到持有者的栅栏失效。
func TestBoardSeedSeqIncrementsAndKeepsFence(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s := openStore(t)
	b := s.Board()

	b.Seed(taskboard.Task{ID: "t1", State: taskboard.StateOpen})
	first, err := b.List(ctx)
	if err != nil {
		t.Fatalf("读任务失败: %v", err)
	}
	if len(first) != 1 || first[0].Seq == 0 {
		t.Fatalf("装载后的任务应有非零序号（与 pgstore 同款：新插入 seq=1），实得 %+v", first)
	}

	// 认领一次把代次顶上去，再重摆：代次必须**保留**（Seed 是装载入口，不是接管）。
	claimed, err := b.Claim(ctx, "t1", "a1", taskboard.ClaimRequest{TTL: time.Minute})
	if err != nil {
		t.Fatalf("认领失败: %v", err)
	}
	b.Seed(taskboard.Task{ID: "t1", State: taskboard.StateOpen})

	after, err := b.List(ctx)
	if err != nil {
		t.Fatalf("读任务失败: %v", err)
	}
	if len(after) != 1 {
		t.Fatalf("重摆后应仍只有一条任务，实得 %d 条", len(after))
	}
	if after[0].Seq <= first[0].Seq {
		t.Fatalf("重摆的序号必须递增（覆盖会让「任务有没有被改动」判不出来）：旧 %d，新 %d",
			first[0].Seq, after[0].Seq)
	}
	// 代次保留只能从**下一次认领**上看（租约被清空后快照不暴露代次——
	// 与 pgstore 同款：无令牌即无租约）。代次被 Seed 清零 ⇒ 新认领的代次会
	// 回到 1 ⇒ 与上一次持平 ⇒ 这条断言必红。
	reclaimed, err := b.Claim(ctx, "t1", "a2", taskboard.ClaimRequest{TTL: time.Minute})
	if err != nil {
		t.Fatalf("重摆后应当可再认领: %v", err)
	}
	if reclaimed.Lease.Fence <= claimed.Lease.Fence {
		t.Fatalf("重摆不得清零接管代次（代次是持久化水位）：旧 %d，新 %d",
			claimed.Lease.Fence, reclaimed.Lease.Fence)
	}
}

// TestBoardListReadsThroughIndependentHandle 独立读路径走**只读句柄**。
//
// 判据形态：写事务占住写句柄的唯一连接（MaxOpenConns(1)）并持着写锁时，
// 独立读路径仍要能查——走写句柄的实现会挂到 ctx 超时（实测 2s deadline
// exceeded）⇒ 这条用例对「List 误用写句柄」是**必红**的。
//
// 同时钉住快照语义：读到的是**已提交**快照（看不到未提交数据）。
func TestBoardListReadsThroughIndependentHandle(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s := openStore(t)
	b := s.Board()
	b.Seed(taskboard.Task{ID: "t1", State: taskboard.StateOpen})

	// 占住写句柄的唯一连接并持写锁（BEGIN IMMEDIATE）：写事务未提交。
	tx, err := s.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("开写事务失败: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx,
		`UPDATE gak_board_task SET title = ? WHERE id = ?`, "未提交", "t1"); err != nil {
		t.Fatalf("写事务内更新失败: %v", err)
	}

	// 写句柄的唯一连接正被事务占着 ⇒ 从它取连接会一直等。
	readCtx, readCancel := context.WithTimeout(ctx, 5*time.Second)
	defer readCancel()
	tasks, err := b.List(readCtx)
	if err != nil {
		t.Fatalf("写事务占用写句柄期间 List 必须仍能查（独立只读句柄）：%v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("快照应有一条任务，实得 %d 条", len(tasks))
	}
	if tasks[0].Title == "未提交" {
		t.Fatalf("独立读路径须读到已提交快照（看不到未提交数据），实得 %+v", tasks[0])
	}
}

// TestBoardFencePersistsAcrossReopen 接管代次是**持久化水位**，不是进程内计数。
//
// 释放后重新认领、以及**重开库**后的认领，代次都必须继续递增——代次跨重启
// 不递增 ⇒ 下游凭代次拒收迟到写入的栅栏就失效了。
func TestBoardFencePersistsAcrossReopen(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	path := filepath.Join(t.TempDir(), "state.db")

	s, err := Open(path)
	if err != nil {
		t.Fatalf("打开失败: %v", err)
	}
	if err := s.EnsureSchema(ctx); err != nil {
		t.Fatalf("建表失败: %v", err)
	}
	b := s.Board()
	// 注入一个固定的过去时刻：第一次认领的租约立刻算过期（便于重开后接管）。
	b.UseClock(func() time.Time { return time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC) })
	b.Seed(taskboard.Task{ID: "t1", State: taskboard.StateOpen})
	first, err := b.Claim(ctx, "t1", "a1", taskboard.ClaimRequest{TTL: time.Minute})
	if err != nil {
		t.Fatalf("首次认领失败: %v", err)
	}
	if first.Lease.Fence == 0 {
		t.Fatalf("首次认领必须铸出非零接管代次，实得 %+v", first.Lease)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("关闭失败: %v", err)
	}

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("重开失败: %v", err)
	}
	defer func() { _ = s2.Close() }()
	second, err := s2.Board().Claim(ctx, "t1", "a2", taskboard.ClaimRequest{TTL: time.Minute})
	if err != nil {
		t.Fatalf("重开库后过期租约必须可被接管: %v", err)
	}
	if second.Lease.Fence <= first.Lease.Fence {
		t.Fatalf("代次是持久化水位：重开库后的认领代次必须大于历史最大代次（旧 %d，新 %d）",
			first.Lease.Fence, second.Lease.Fence)
	}
}
