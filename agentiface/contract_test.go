package agentiface

import (
	"context"
	"errors"
	"testing"

	"github.com/oagentkit/agent/iface"
	"github.com/yi-nology/goagent-kernel/contract"
)

// fakeRC 最小宿主编排读面：记录状态迁移，不施加状态机约束（约束归宿主）。
type fakeRC struct {
	status   string
	statuses []string
}

func (f *fakeRC) ID() string     { return "s-1" }
func (f *fakeRC) CaseID() string { return "" }
func (f *fakeRC) Status() string { return f.status }
func (f *fakeRC) SetStatus(s string) error {
	f.status = s
	f.statuses = append(f.statuses, s)
	return nil
}
func (f *fakeRC) CurrentStep() string           { return "" }
func (f *fakeRC) SetCurrentStep(string) error   { return nil }
func (f *fakeRC) Field(string) (string, bool)   { return "", false }
func (f *fakeRC) SetField(string, string) error { return nil }

func TestContractSuccessClosesTerminalState(t *testing.T) {
	// Exec 只产出终答、没推状态（created 停着）⇒ 适配器补 succeeded 收口
	rc := &fakeRC{status: contract.StatusCreated}
	c := &Contract{RC: rc, Exec: func(context.Context, iface.Input) (string, error) {
		return "宿主终答", nil
	}}
	got, err := c.Run(context.Background(), iface.Input{Query: "q"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Status != contract.StatusSucceeded {
		t.Fatalf("Status = %q, want %q", got.Status, contract.StatusSucceeded)
	}
	if got.Final != "宿主终答" {
		t.Fatalf("Final = %q", got.Final)
	}
	if rc.status != contract.StatusSucceeded {
		t.Fatalf("RC 未被补到终态: %q", rc.status)
	}
}

func TestContractFailureMapsContextError(t *testing.T) {
	rc := &fakeRC{status: contract.StatusRunning}
	c := &Contract{RC: rc, Exec: func(context.Context, iface.Input) (string, error) {
		return "", context.DeadlineExceeded
	}}
	got, err := c.Run(context.Background(), iface.Input{Query: "q"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ctx 错应原样可判: %v", err)
	}
	if got.Status != contract.StatusTimedOut {
		t.Fatalf("Status = %q, want %q", got.Status, contract.StatusTimedOut)
	}
}

func TestContractHostAdvancedStatusRespected(t *testing.T) {
	// 宿主已把状态推到终态（cancelled）⇒ 适配器不覆盖状态机的裁决
	rc := &fakeRC{status: contract.StatusRunning}
	c := &Contract{RC: rc, Exec: func(context.Context, iface.Input) (string, error) {
		_ = rc.SetStatus(contract.StatusCancelled)
		return "", context.Canceled
	}}
	got, err := c.Run(context.Background(), iface.Input{Query: "q"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ctx 错应原样可判: %v", err)
	}
	if got.Status != contract.StatusCancelled {
		t.Fatalf("Status = %q, want 宿主状态机的裁决 %q", got.Status, contract.StatusCancelled)
	}
}

func TestContractRejectsOnEventAndMissingParts(t *testing.T) {
	c := &Contract{RC: &fakeRC{status: contract.StatusCreated},
		Exec: func(context.Context, iface.Input) (string, error) { return "", nil }}
	if _, err := c.Run(context.Background(), iface.Input{Query: "q",
		OnEvent: func(iface.Event) {}}); err == nil {
		t.Fatal("OnEvent 必须显式拒绝")
	}
	if _, err := (&Contract{Exec: c.Exec}).Run(context.Background(),
		iface.Input{}); err == nil {
		t.Fatal("RC 未装配必须报错")
	}
	if _, err := (&Contract{RC: c.RC}).Run(context.Background(),
		iface.Input{}); err == nil {
		t.Fatal("Exec 未装配必须报错")
	}
}

// TestContractStatusLiteralsAlignWithAgentAdapter 跨仓词表对齐链的 runtime 半边。
//
// agent/agentrun 适配器（tier 3 不能 import kernel）用**字面量**输出状态；
// 本侧钉 contract 常量 == 同一批字面量。两半合起来（本文件 +
// oagentkit/agent 的 agentrun/iface_test.go）⇒ agent 侧适配器输出 == contract
// 常量，词表分叉在两侧任一改值时都会转红（contract#354/#357 形态的防线）。
func TestContractStatusLiteralsAlignWithAgentAdapter(t *testing.T) {
	cases := []struct{ got, want string }{
		{contract.StatusSucceeded, "succeeded"},
		{contract.StatusFailed, "failed"},
		{contract.StatusCancelled, "cancelled"},
		{contract.StatusTimedOut, "timed_out"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("contract 常量 = %q，agent 适配器字面量 = %q——两侧必须同值",
				c.got, c.want)
		}
	}
}
