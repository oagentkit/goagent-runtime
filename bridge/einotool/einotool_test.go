package einotool

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	ejsonschema "github.com/eino-contrib/jsonschema"
)

// fakeTool 手写 eino InvokableTool 最小实现。
type fakeTool struct {
	name        string
	desc        string
	paramsJSON  string
	invokedArgs string
}

func (f *fakeTool) Info(ctx context.Context) (*schema.ToolInfo, error) {
	info := &schema.ToolInfo{Name: f.name, Desc: f.desc}
	if f.paramsJSON != "" {
		var sch ejsonschema.Schema
		if err := json.Unmarshal([]byte(f.paramsJSON), &sch); err != nil {
			return nil, err
		}
		info.ParamsOneOf = schema.NewParamsOneOfByJSONSchema(&sch)
	}
	return info, nil
}

func (f *fakeTool) InvokableRun(ctx context.Context, args string, opts ...tool.Option) (string, error) {
	f.invokedArgs = args
	return "ok:" + args, nil
}

func TestAdaptConvertsShape(t *testing.T) {
	f := &fakeTool{
		name:       "查库存",
		desc:       "按 SKU 查库存",
		paramsJSON: `{"type":"object","properties":{"sku":{"type":"string"}}}`,
	}
	tl, err := Adapt(context.Background(), f)
	if err != nil {
		t.Fatalf("Adapt: %v", err)
	}
	if tl.Name != "查库存" || tl.Description != "按 SKU 查库存" {
		t.Fatalf("形状未转换: %+v", tl)
	}
	if len(tl.Parameters) == 0 || !strings.Contains(string(tl.Parameters), "sku") {
		t.Fatalf("参数模式缺失: %s", tl.Parameters)
	}
	// 副作用闸字段不猜：全零值，语义宿主补。
	if tl.Safe || tl.NeedsApproval || tl.IdempotencyKey != nil || tl.ResourceKey != nil {
		t.Fatalf("副作用闸字段应为零值: %+v", tl)
	}
	// Handler 走 InvokableRun（JSON 文本透传）。
	out, err := tl.Handler(context.Background(), []byte(`{"sku":"A"}`))
	if err != nil || out != "ok:"+`{"sku":"A"}` || f.invokedArgs != `{"sku":"A"}` {
		t.Fatalf("Handler 未桥到 InvokableRun: out=%q err=%v invoked=%q", out, err, f.invokedArgs)
	}
}

func TestAdaptWithoutParams(t *testing.T) {
	tl, err := Adapt(context.Background(), &fakeTool{name: "ping", desc: "探活"})
	if err != nil {
		t.Fatalf("Adapt: %v", err)
	}
	if tl.Parameters != nil {
		t.Fatalf("无参数模式应为 nil: %s", tl.Parameters)
	}
}

func TestAdaptAllFailClosed(t *testing.T) {
	okTool := &fakeTool{name: "ping"}
	_, err := AdaptAll(context.Background(), []tool.InvokableTool{okTool, &nilInfoTool{}})
	if err == nil || !strings.Contains(err.Error(), "Info 返回 nil") {
		t.Fatalf("期望整体失败，got %v", err)
	}
}

type nilInfoTool struct{ fakeTool }

func (n *nilInfoTool) Info(ctx context.Context) (*schema.ToolInfo, error) { return nil, nil }
