// 绑定包（storage/sqlitestore）的判据：门面三面齐备 + fail-fast +
// 联合提交能力面成立。
//
// 这几条存在的理由：#10 把 SQLite 绑定从 storage 纯判据包拆出来之后，
// 「宿主拿到的东西是不是完整的 Provider」就成了**新形态**的问题——
// 能力面若没钉住，runtime 装配会静默落回两步提交路径（批662 的同形面）。
package sqlitestore_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/oagentkit/goagent-kernel/sidetest"

	"github.com/oagentkit/goagent-runtime/storage"
	"github.com/oagentkit/goagent-runtime/storage/sqlitestore"
)

// TestOpenProviderFaces Provider 三面齐备（runtime 装配的前提）。
func TestOpenProviderFaces(t *testing.T) {
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

// TestOpenEmptyPathFails 空路径 fail-fast：匿名库会让「历史无处可寻」，
// kernel 的适配器一律报错而不是静默开一个内存库。
func TestOpenEmptyPathFails(t *testing.T) {
	if _, err := sqlitestore.Open(context.Background(), ""); err == nil {
		t.Fatal("空路径必须报错（不得静默开匿名库）")
	}
}

// TestSatisfiesJointCommitter 联合提交能力面必须**由本类型直接实现**——
// runtime 装配的判定是类型断言 `a.storage.(storage.JointCommitter)`；
// 若本类型只给一个 AsJoint() 转发器而没实现 CommitWithEvents，断言恒 false ⇒
// 装配静默落回两步提交路径（事件与快照不再同生共死）。
func TestSatisfiesJointCommitter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "joint-cap.db")
	st, err := sqlitestore.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("sqlitestore.Open 失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	var _ storage.JointCommitter = st
	if _, ok := any(st).(storage.JointCommitter); !ok {
		t.Fatal("*sqlitestore.SQLite 必须直接实现 storage.JointCommitter（否则装配静默落回两步路径）")
	}
}

// TestSQLiteEffectsSatisfyKernelContract 同一段判据跑在真 SQLite 上——
// standalone 模板用的就是这条存储路径，契约必须在真实落库形态上成立。
// 走的是绑定包（sqlitestore.Open）而不是裸参考实现：判据要钉的是**宿主
// 实际拿到的那条路径**。
func TestSQLiteEffectsSatisfyKernelContract(t *testing.T) {
	sidetest.Run(t, func(t *testing.T, clock *sidetest.FakeClock) sidetest.ClockedStore {
		path := filepath.Join(t.TempDir(), "contract.db")
		st, err := sqlitestore.Open(context.Background(), path)
		if err != nil {
			t.Fatalf("sqlitestore.Open 失败: %v", err)
		}
		t.Cleanup(func() { _ = st.Close() })
		cs, ok := st.Effects().(sidetest.ClockedStore)
		if !ok {
			t.Fatal("Effects 面必须支持时钟注入（sidetest.ClockedStore）")
		}
		cs.UseClock(clock.Now)
		return cs
	})
}
