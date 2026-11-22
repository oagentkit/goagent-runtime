// option_test.go —— Open 的 opts 注入路径判据（坏实现必红）。
//
// Open(path, opts...) 是 brief 明确列出的 Produces 签名，WithClock 是导出的
// 公开装配入口。此前整条 opts 路径没有任何用例（测试只用了 UseClock）⇒
// opts 被静默丢弃时注入时钟失效，门禁与用例都不会红。

package sqliteadapter

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// TestOpenAppliesClockOption Open(path, WithClock(...)) 必须真的把时钟装上。
//
// 断言取一个与存储时钟**明显不同**的固定时刻（2000 年）：opts 被丢弃时读到的
// 是存储时钟（当前年份）⇒ 坏实现必红。
func TestOpenAppliesClockOption(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	fixed := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	s, err := Open(path, WithClock(func() time.Time { return fixed }))
	if err != nil {
		t.Fatalf("打开失败: %v", err)
	}
	defer func() { _ = s.Close() }()
	e := s.exec()
	if e.clockOverride() == nil {
		t.Fatal("Open 必须应用 opts：WithClock 装上的注入时钟不见了（opts 被丢弃）")
	}
	got, err := e.now(context.Background(), nil)
	if err != nil {
		t.Fatalf("取时刻失败: %v", err)
	}
	if !got.Equal(fixed) {
		t.Fatalf("Open 装配的注入时钟必须生效：应得 %v，实得 %v（若实得是当前年份，说明 opts 被丢弃）", fixed, got)
	}
}
