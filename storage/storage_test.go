package storage_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/yi-nology/goagent-kernel/sidetest"
	"github.com/yi-nology/goagent-runtime/sqliteadapter"

	"github.com/yi-nology/goagent-runtime/storage"
	"github.com/yi-nology/goagent-runtime/storage/sqlitestore"
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

// TestSQLiteEffectsSatisfyKernelContract 同一段判据跑在真 SQLite 上——
// standalone 模板用的就是这条存储路径，契约必须在真实落库形态上成立。
func TestSQLiteEffectsSatisfyKernelContract(t *testing.T) {
	sidetest.Run(t, func(t *testing.T, clock *sidetest.FakeClock) sidetest.ClockedStore {
		path := filepath.Join(t.TempDir(), "contract.db")
		st, err := sqliteadapter.Open(path, sqliteadapter.WithClock(clock.Now))
		if err != nil {
			t.Fatalf("开库失败: %v", err)
		}
		t.Cleanup(func() { _ = st.Close() })
		if err := st.EnsureSchema(context.Background()); err != nil {
			t.Fatalf("建 schema 失败: %v", err)
		}
		// 具体类型隐式满足 ClockedStore（*EffectStore 自带 UseClock 接缝）。
		return st.Effects()
	})
}

// TestSQLiteProviderFaces SQLite Provider 三面齐备（runtime 装配的前提）。
func TestSQLiteProviderFaces(t *testing.T) {
	path := filepath.Join(t.TempDir(), "faces.db")
	st, err := sqlitestore.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("sqlitestore.Open 失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	if st.Checkpoints() == nil {
		t.Fatal("Checkpoints 面为 nil")
	}
	if st.Effects() == nil {
		t.Fatal("Effects 面为 nil")
	}
	if st.Events() == nil {
		t.Fatal("Events 面为 nil（standalone 事件要落库）")
	}
}

// TestMemEventsNilByDesign 内存形态不持久化事件是**有意默认**：nil 让
// loop 判 nil 跳过持久化，而不是拿到一个假装落盘的哑实现。
func TestMemEventsNilByDesign(t *testing.T) {
	if storage.NewMem().Events() != nil {
		t.Fatal("内存 Provider 的 Events 应返回 nil")
	}
}
