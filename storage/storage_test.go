package storage_test

import (
	"testing"

	"github.com/yi-nology/goagent-kernel/sidetest"

	"github.com/yi-nology/goagent-runtime/storage"
)

// TestMemEffectsSatisfyKernelContract 用 kernel 的 sidetest 共享契约套件
// （33 个子测试）钉内存实现：runtime 不另写一套判据，同一段语义。
func TestMemEffectsSatisfyKernelContract(t *testing.T) {
	sidetest.Run(t, func(t *testing.T, clock *sidetest.FakeClock) sidetest.ClockedStore {
		m := storage.NewMem()
		m.UseClock(clock.Now)
		return m.Effects().(sidetest.ClockedStore)
	})
}

// TestMemEventsNilByDesign 内存形态不持久化事件是**有意默认**：nil 让
// loop 判 nil 跳过持久化，而不是拿到一个假装落盘的哑实现。
func TestMemEventsNilByDesign(t *testing.T) {
	if storage.NewMem().Events() != nil {
		t.Fatal("内存 Provider 的 Events 应返回 nil")
	}
}
