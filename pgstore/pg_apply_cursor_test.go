// pg_apply_cursor_test.go —— 批次内**写游标**必须显式拒绝（评审 I-2）。
//
// # 为什么 PG 侧必须显式拒绝
//
// 游标面在两侧**都不在批次内**（两边 cursor.go 的包头都写明「游标不是业务
// 状态，tailer 在投递之后保存」；SQLite 侧的 `Tx` 访问器集合里也没有游标）
// ⇒ 本轮**不改语义**（不把游标改成进批次）。
//
// 但两侧的**失败形态**不同：
//
//   - SQLite 侧批内写游标会**挂到 ctx 超时**（响亮，但慢）；
//   - PG 侧批内写游标会**独立提交**（pool.Exec，另有连接）⇒ 游标越过被回滚的
//     事件 ⇒ 重启后跳读 ⇒ **静默丢帧**（比挂死更坏：没有任何报错）。
//
// ⇒ PG 侧批内写游标必须**显式拒绝**（不挂死、不静默半提交），且拒绝要**登记
// 进共享失败槽** ⇒ 调用方吞掉这个错误也照样整批作废。
//
// ⚠ 本轮**只改 PG 侧**：SQLite 侧「挂到 ctx 超时」是 sqliteadapter（v0.2.0 自 goagent-kernel 迁入）/
// apply.go 包头**明确记载的既有取舍**（不是本任务引入的）⇒ 快速失败化登记为
// deferred，留最终评审分诊。
package pgstore

import (
	"context"
	"strings"
	"testing"

	"github.com/oekit/pkg/errors"

	"github.com/yi-nology/goagent-kernel/eventbus"
	"github.com/yi-nology/goagent-kernel/tenant"
)

// assertCursorWriteRejectedInBatch 批内写游标 ⇒ 显式拒绝 + 整批回滚 + 游标没写。
//
// 三条断言各自承重：
//   - **不挂死**：ctx 未被耗尽（挂死会耗尽 ctx ⇒ 那正是 SQLite 侧的形态）；
//   - **拒绝有身份**：报错里带「批次内禁止写游标」⇒ 拒绝被登记进失败槽；
//   - **整批回滚 + 游标没写**：批内那次事件写没落库，游标行也不存在。
func assertCursorWriteRejectedInBatch(t *testing.T, s *Store, ctx context.Context,
	save func(ctx context.Context) error) {
	t.Helper()
	err := s.Apply(ctx, func(ctx context.Context) error {
		if _, ierr := s.Events().Insert(ctx, eventbus.Event{SessionID: "s", Type: "t"}); ierr != nil {
			return ierr
		}
		// 批内写游标 ⇒ 必须被拒。
		if cerr := save(ctx); cerr == nil {
			return errors.New("pgstore: 批内写游标必须被拒（独立提交 ⇒ 越过被回滚的事件 ⇒ 重启后跳读）")
		}
		return nil
	})
	if err == nil {
		t.Fatal("批内写游标被拒时 Apply 必须报错（整批已作废）")
	}
	if ctx.Err() != nil {
		t.Fatalf("批内写游标必须**显式拒绝**（不是挂到 ctx 超时），实得 ctx 已耗尽: %v", ctx.Err())
	}
	if !strings.Contains(err.Error(), "批次内禁止写游标") {
		t.Fatalf("Apply 报出的应是「批内禁止写游标」那条拒绝，实得 %v", err)
	}
	wm, werr := s.Events().CommittedWatermark(ctx)
	if werr != nil {
		t.Fatalf("读水位线失败: %v", werr)
	}
	if wm != 0 {
		t.Fatalf("批内写游标被拒 ⇒ 整批必须回滚（事件水位线应为 0），实得 %d", wm)
	}
}

// TestApplyRejectsCursorWriteInBatch 默认型游标面：批内写游标 ⇒ 显式拒绝。
func TestApplyRejectsCursorWriteInBatch(t *testing.T) {
	dsn := requireDSN(t)
	ctx := applyCtx(t)
	s := freshStore(t, dsn)

	assertCursorWriteRejectedInBatch(t, s, ctx, func(ctx context.Context) error {
		return s.TailCursors("consumer-a").SaveCursor(ctx, 7)
	})

	// 游标本身也不得留下（被拒 ⇒ 根本没写）。
	cur, found, cerr := s.TailCursors("consumer-a").LoadCursor(ctx)
	if cerr != nil {
		t.Fatalf("读游标失败: %v", cerr)
	}
	if found || cur != 0 {
		t.Fatalf("批内写游标被拒 ⇒ 游标不得留下，实得 cursor=%d found=%v", cur, found)
	}
}

// TestApplyRejectsTenantCursorWriteInBatch 租户作用域游标面：批内写游标 ⇒
// 显式拒绝（且**不是**因为缺租户——ctx 里带租户，拒的必须是「批次内写游标」）。
func TestApplyRejectsTenantCursorWriteInBatch(t *testing.T) {
	dsn := requireDSN(t)
	ctx := applyCtx(t)
	s := freshStore(t, dsn)

	assertCursorWriteRejectedInBatch(t, s, ctx, func(ctx context.Context) error {
		// 带租户上下文 ⇒ 拒的必须是「批次内写游标」，不是「缺租户」。
		tCtx := tenant.WithContext(ctx, tenant.Context{Tenant: "t-cursor"})
		return s.TenantTailCursors("consumer-b").SaveCursor(tCtx, 7)
	})

	tCtx := tenant.WithContext(ctx, tenant.Context{Tenant: "t-cursor"})
	cur, found, cerr := s.TenantTailCursors("consumer-b").LoadCursor(tCtx)
	if cerr != nil {
		t.Fatalf("读游标失败: %v", cerr)
	}
	if found || cur != 0 {
		t.Fatalf("批内写游标被拒 ⇒ 游标不得留下，实得 cursor=%d found=%v", cur, found)
	}
}
