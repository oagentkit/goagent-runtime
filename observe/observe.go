// Package observe —— 事件观测的**薄装配**：eventbus → 宿主回调。
//
// kernel 的 eventbus 给广播与持久化判据；本包只做一件事：订阅并把事件
// 转发给宿主的 Sink（日志/指标/审计各自实现 Sink 即可）。不做过滤、
// 不做采样、不 Interpret 事件语义——那是宿主的事。
package observe

import (
	"context"

	"github.com/oekit/concurrency/async"

	"github.com/yi-nology/goagent-kernel/eventbus"
)

// Sink 宿主的事件消费面。
type Sink func(eventbus.Event)

// Attach 订阅 bus 并把事件转发给 sink，返回 detach 函数。
// sink 为 nil 是 no-op（返回空 detach）。
func Attach(ctx context.Context, bus *eventbus.Bus, sink Sink) (detach func()) {
	if sink == nil || bus == nil {
		return func() {}
	}
	ch, unsubscribe := bus.Subscribe(ctx)
	done := make(chan struct{})
	// 走 async.GoSafe：goroutine 内 panic 必须被拦（裸 go func 杀进程）。
	async.GoSafe(func() {
		defer close(done)
		for ev := range ch {
			sink(ev)
		}
	})
	return func() {
		unsubscribe()
		<-done // join：转发 goroutine 收尾后才算 detach 完成
	}
}
