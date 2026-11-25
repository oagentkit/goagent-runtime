// board_deps_test.go —— 依赖判定的边界格：依赖**根本不存在**。
//
// # 与契约套件那条用例的区别（别混）
//
//   - 套件（boardtest「依赖未完成不得认领」）：依赖**存在**，但状态不是 done
//     ⇒ ErrBlocked。
//   - 本文件这条：依赖**根本不存在**（依赖行被删 / 拓扑未装载）⇒ ErrBlocked。
//
// # 为什么必须单列一条
//
// 「不存在即视为已完成」是一次真实的重构诱惑：depsDone 的 sql.ErrNoRows 分支
// 改成 return nil（变异注入 MUT-D2）⇒ 全套用例仍然全绿 ⇒ 依赖未装载的任务
// 被放行认领 ⇒ **两段依赖工作并行施加**，且这类失效在报告里完全看不出来。
// 套件那条够不着这一格（它造的是「存在但未完成」的依赖）。
package sqliteadapter

import (
	"context"
	"testing"
	"time"

	"github.com/oekit/pkg/errors"

	"github.com/oagentkit/goagent-kernel/taskboard"
)

// TestBoardClaimBlockedWhenDependencyMissing 依赖不存在 ⇒ 认领必须被拒。
//
// 判据形态：只摆 r1（依赖 r2），r2 **不存在** ⇒ Claim(r1) 必须报 ErrBlocked。
func TestBoardClaimBlockedWhenDependencyMissing(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s := openStore(t)
	b := s.Board()

	// r1 依赖 r2，而 r2 根本不存在（只摆 r1）。
	b.Seed(taskboard.Task{
		ID:    "r1",
		State: taskboard.StateOpen,
		Deps:  []taskboard.TaskID{"r2"},
	})

	_, err := b.Claim(ctx, "r1", "a1", taskboard.ClaimRequest{TTL: time.Minute})
	if err == nil {
		t.Fatalf("依赖不存在时认领必须被拒（实得无错）——「不存在即视为已完成」会让依赖未装载的任务被放行认领 ⇒ 两段依赖工作并行施加")
	}
	if !errors.Is(err, taskboard.ErrBlocked) {
		t.Fatalf("依赖不存在时认领必须报 ErrBlocked（实得 %v）", err)
	}
}
