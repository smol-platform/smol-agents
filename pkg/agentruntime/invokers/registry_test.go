package invokers

import (
	"testing"

	v1 "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

func TestDefault_WiresHTTP(t *testing.T) {
	reg := Default(fakeLeaser{val: "t"}, nil)
	inv, ok := reg[v1.ToolHTTP]
	if !ok || inv == nil {
		t.Fatalf("Default must wire an HTTP invoker, got %v", reg)
	}
	if _, isHTTP := inv.(*HTTPInvoker); !isHTTP {
		t.Errorf("ToolHTTP must map to *HTTPInvoker, got %T", inv)
	}
}
