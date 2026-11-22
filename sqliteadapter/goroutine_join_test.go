// goroutine_join_test.go —— 「收尾助手必须等 goroutine 真退出」的语义侧回归。
//
// # 这条用例钉住什么
//
// issue #428 的纪律是「用例内起的 goroutine 必须在用例返回前被等待」。规则侧
// （archcheck）判的是**形态**（有没有等待点），判不出**语义**（那个等待点是不是
// 真的等到了退出）。后者只能靠真实并发语义来钉：收尾助手 stopTailer 若只
// cancel 不等，形态照样合规 ⇒ 规则绿着，而 goroutine 仍在用例返回后跑。
//
// # 为什么用「退出标志」而不是 sleep
//
// sleep 是**概率**：睡够多久都可能不够。标志法是**确定**的：goroutine 自己在
// 退出前把标志置上，而置位严格发生在 close(done) **之前** ⇒ stopTailer 返回
// （它等的是 done）之后标志必然为真。断言失败即说明助手没等。
package sqliteadapter

import (
	"context"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

// TestStopTailerWaitsForGoroutineExit 收尾助手必须等 goroutine 真退出。
func TestStopTailerWaitsForGoroutineExit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var exited atomic.Bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		<-ctx.Done()
		// 退出标志必须**在** close(done) 之前置位：断言方等的是 done，
		// 于是 done 关闭 ⇒ 标志已置位（顺序反了断言就没有意义）。
		exited.Store(true)
	}()

	stopTailer(t, cancel, done, "合成收尾")

	if !exited.Load() {
		t.Fatal("stopTailer 返回时 goroutine 尚未退出：" +
			"收尾助手只取消了上下文、没有等它退出")
	}
}

// TestDeferredTailerCleanupJoinsOnGoexitPath 失败路径上 deferred 收尾**仍然 join**。
//
// 「失败路径」= 用例体里 t.Fatalf 触发的 runtime.Goexit：Goexit 会执行**所有**
// defer，但**不执行**它之后的普通语句 ⇒ 收尾点若写成「用例末尾的普通语句」，
// 失败路径上那一跳根本不执行 ⇒ tailer 未被 join，而用例已经判失败
// （issue #431 的失效面，也是 archcheck 那条规则的四条已知边界之第 ④ 条）。
//
// 这里**不**真的让用例失败（那会污染套件），而是直接复现 Goexit 的语义：
// 在一个 goroutine 里注册 defer 之后调用 runtime.Goexit() ⇒ 该 defer 必须被
// 执行 ⇒ 它必须真的等到 goroutine 退出。
func TestDeferredTailerCleanupJoinsOnGoexitPath(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var exited atomic.Bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		<-ctx.Done()
		// 退出标志必须**在** close(done) 之前置位（与上面那条用例同款）。
		exited.Store(true)
	}()

	joined := make(chan bool, 1)
	go func() {
		defer func() {
			joined <- joinTailer(cancel, done, 2*time.Second)
		}()
		runtime.Goexit() // 模拟用例体中途 t.Fatalf
	}()

	select {
	case ok := <-joined:
		if !ok {
			t.Fatal("deferred 收尾在 Goexit 之后仍未等到 goroutine 退出")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Goexit 之后 deferred 收尾未执行（Goexit 必须执行所有 defer）")
	}
	if !exited.Load() {
		t.Fatal("deferred 收尾返回时 goroutine 尚未退出：" +
			"收尾助手只取消了上下文、没有等它退出")
	}
}
