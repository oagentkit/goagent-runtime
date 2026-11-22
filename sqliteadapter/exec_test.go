// exec_test.go —— 时钟优先级的判据（坏实现必红）。
//
// 契约：注入时钟**优先**于存储时钟，与「有没有 tx」无关。
// 此前两条注入时钟的用例都传 nil tx，唯一测「有 tx」的用例又用存储时钟 ⇒
// 「tx 与注入时钟同时存在」这一格是空的 ⇒ 把优先级改成「tx 优先」不会让任何
// 用例变红 ⇒ Task 3+ 的契约套件会在事务内静默读到存储时钟（时钟口径漂移）。

package sqliteadapter

import (
	"context"
	"testing"
	"time"
)

// TestExecNowPrefersInjectedClockInsideTx 有 tx 时，注入时钟仍须优先。
//
// 断言取一个与存储时钟**明显不同**的固定时刻（2000 年）：存储时钟绝不可能
// 返回它 ⇒ 退化成存储时钟的实现必红。
func TestExecNowPrefersInjectedClockInsideTx(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	fixed := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	tx, err := s.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("开事务失败: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	// tx 非 nil **且**注入了时钟 ⇒ 判据落在这两者的交叉格上。
	e := exec{db: s.DB(), tx: tx, clock: func() time.Time { return fixed }}
	got, err := e.now(ctx, tx)
	if err != nil {
		t.Fatalf("取时刻失败: %v", err)
	}
	if !got.Equal(fixed) {
		t.Fatalf("有 tx 时注入时钟仍须优先：应得注入时刻 %v，实得 %v（若实得是当前年份，说明退回了存储时钟）", fixed, got)
	}
}
