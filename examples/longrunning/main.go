// longrunning —— Long-running Agent 入口样例：长任务与中断恢复。
//
// 七词词汇侧重：Interrupt（注入的模型错误中断 Run，快照已持久化）与
// Resume（同 sessionID 再 Run 从快照恢复，已施加的副作用不重放）。
//
// 中断点的可观测：W5 之后失败会落 session_status{failed,error}——
// 两次 Run 之间用 Session.Status() 读得到（failed → 恢复后 succeeded）。
//
//	CGO_ENABLED=0 go run ./examples/longrunning
package main

import (
	"context"
	"fmt"
	"log"
	"sync"

	"github.com/yi-nology/goagent-runtime/chat"
	"github.com/yi-nology/goagent-runtime/pipeline"
	"github.com/yi-nology/goagent-runtime/runtime"
	"github.com/yi-nology/goagent-runtime/storage"
)

// scriptModel 脚本化假模型；errAt >= 0 时该次调用返回错误（注入 Interrupt）。
type scriptModel struct {
	mu     sync.Mutex
	steps  []chat.ChatResponse
	errAt  int
	called int
}

func (m *scriptModel) Chat(_ context.Context, _ chat.ChatRequest) (chat.ChatResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	i := m.called
	m.called++
	if m.errAt >= 0 && i == m.errAt {
		return chat.ChatResponse{}, errInjected
	}
	if i < len(m.steps) {
		return m.steps[i], nil
	}
	return chat.ChatResponse{Content: "（剧本耗尽）"}, nil
}

var errInjected = errString("上游模型超时（剧本注入的 Interrupt）")

type errString string

func (e errString) Error() string { return string(e) }

// counter 计数工具：施加次数即副作用次数（Resume 后不得重放）。
type counter struct {
	mu    sync.Mutex
	calls int
}

func (c *counter) handler(_ context.Context, _ []byte) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	return fmt.Sprintf("第 %d 次施加", c.calls), nil
}

func (c *counter) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

func main() {
	ctx := context.Background()
	mem := storage.NewMem() // 两个 Runtime 共享同一存储 = 跨「进程」恢复
	ctr := &counter{}
	tools := []pipeline.Tool{{Name: "report", Description: "生成报告",
		Safe: true, Handler: ctr.handler}}

	// ── Interrupt：Run 中途被模型错误中断（快照已持久化，副作用已施加 1 次）。
	fmt.Println("── Interrupt ──")
	rt1, err := runtime.NewEmbedded(runtime.Config{
		Model: &scriptModel{errAt: 1, steps: []chat.ChatResponse{
			{ToolCalls: []chat.ToolCall{{ID: "c1", Name: "report", Arguments: "{}"}}},
		}},
		Tools: tools,
	}, runtime.WithStorage(mem))
	if err != nil {
		log.Fatal(err)
	}
	s1, err := rt1.NewSession(ctx, runtime.SessionOptions{ID: "longrun"})
	if err != nil {
		log.Fatal(err)
	}
	_, runErr := s1.Run(ctx, "生成周报")
	st, _ := s1.Status(ctx)
	fmt.Printf("Run 被中断: %v\n", runErr)
	fmt.Printf("Status: exists=%v status=%s（失败原因已落事件面）\n", st.Exists, st.Status)
	fmt.Printf("副作用施加次数: %d（中断前已施加）\n", ctr.count())
	_ = rt1.Close()

	// ── Resume：同 sessionID 再 Run，从快照恢复；工具重放只判 replayed。
	fmt.Println("── Resume ──")
	rt2, err := runtime.NewEmbedded(runtime.Config{
		Model: &scriptModel{errAt: -1, steps: []chat.ChatResponse{
			{ToolCalls: []chat.ToolCall{{ID: "c1", Name: "report", Arguments: "{}"}}},
			{Content: "周报已生成（含上次已施加的报告数据）"},
		}},
		Tools: tools,
	}, runtime.WithStorage(mem))
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = rt2.Close() }()
	s2, err := rt2.NewSession(ctx, runtime.SessionOptions{ID: "longrun"})
	if err != nil {
		log.Fatal(err)
	}
	res, err := s2.Run(ctx, "继续")
	if err != nil {
		log.Fatal(err)
	}
	st2, _ := s2.Status(ctx)
	fmt.Printf("Resume 结果: status=%s\n", res.Status)
	fmt.Printf("Status: status=%s seq=%d\n", st2.Status, st2.Seq)
	fmt.Printf("副作用施加次数: %d（恢复重放不重复施加，应仍为 1）\n", ctr.count())
}
