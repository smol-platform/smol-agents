package agentruntime

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	v1 "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

// otelRecorder installs an in-memory span exporter as the global tracer
// provider so StartRunSpan/StartStepSpan (which use the global tracer)
// can be observed, and returns the exporter + a restore func. This is a
// pure, local proof of R-AM-OBS-1 — no collector or cluster needed.
func otelRecorder(t *testing.T) (*tracetest.InMemoryExporter, func()) {
	t.Helper()
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	return exp, func() {
		_ = tp.Shutdown(context.Background())
		otel.SetTracerProvider(prev)
	}
}

func otelStringAttrs(kvs []attribute.KeyValue) map[string]string {
	m := map[string]string{}
	for _, kv := range kvs {
		if kv.Value.Type() == attribute.STRING {
			m[string(kv.Key)] = kv.Value.AsString()
		}
	}
	return m
}

func TestStartRunSpan_EmitsGenAIAttributes(t *testing.T) {
	exp, restore := otelRecorder(t)
	defer restore()

	agent := v1.Agent{}
	agent.Spec.Model.ProviderRef = "zai"
	agent.Spec.Model.Name = "glm-4.6"

	_, span := StartRunSpan(context.Background(), agent, "run-42")
	span.End()

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("want exactly 1 span, got %d", len(spans))
	}
	s := spans[0]
	if s.Name != "invoke_agent run-42" {
		t.Errorf("span name = %q, want %q", s.Name, "invoke_agent run-42")
	}
	got := otelStringAttrs(s.Attributes)
	want := map[string]string{
		"gen_ai.operation.name": "invoke_agent",
		"gen_ai.agent.name":     "run-42",
		"gen_ai.provider.name":  "zai",
		"gen_ai.request.model":  "glm-4.6",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("attr %s = %q, want %q", k, got[k], v)
		}
	}
}

// TestStartStepSpan_OpMappingAndParentage proves each Step kind maps to
// the right gen_ai.operation.name, that gen_ai.tool.name is set only for
// tool steps, and that every step span is a child of the run span (same
// trace, parent == run span).
func TestStartStepSpan_OpMappingAndParentage(t *testing.T) {
	exp, restore := otelRecorder(t)
	defer restore()

	ctx, run := StartRunSpan(context.Background(), v1.Agent{}, "run-1")

	cases := []struct {
		kind   v1.StepKind
		tool   string
		wantOp string
	}{
		{v1.StepPlan, "", "chat"},
		{v1.StepToolCall, "search", "tool_call"},
		{v1.StepObservation, "search", "tool_call"},
		{v1.StepFinal, "", "finalize"},
	}
	for _, c := range cases {
		_, sp := StartStepSpan(ctx, c.kind, c.tool)
		sp.End()
	}
	run.End()

	spans := exp.GetSpans()
	if len(spans) != len(cases)+1 {
		t.Fatalf("want %d spans, got %d", len(cases)+1, len(spans))
	}

	var runTraceID, runSpanID string
	for _, s := range spans {
		if s.Name == "invoke_agent run-1" {
			runTraceID = s.SpanContext.TraceID().String()
			runSpanID = s.SpanContext.SpanID().String()
		}
	}
	if runSpanID == "" {
		t.Fatal("run span not recorded")
	}

	// Index step spans by their agent.step.kind attribute.
	byKind := map[string]tracetest.SpanStub{}
	for _, s := range spans {
		if s.Name == "invoke_agent run-1" {
			continue
		}
		attrs := otelStringAttrs(s.Attributes)
		byKind[attrs["agent.step.kind"]] = s
		if s.SpanContext.TraceID().String() != runTraceID {
			t.Errorf("step %q is not in the run trace", s.Name)
		}
		if s.Parent.SpanID().String() != runSpanID {
			t.Errorf("step %q parent=%s, want run span %s", s.Name, s.Parent.SpanID(), runSpanID)
		}
	}

	for _, c := range cases {
		s, ok := byKind[string(c.kind)]
		if !ok {
			t.Errorf("no span recorded for step kind %s", c.kind)
			continue
		}
		attrs := otelStringAttrs(s.Attributes)
		if attrs["gen_ai.operation.name"] != c.wantOp {
			t.Errorf("kind %s: op = %q, want %q", c.kind, attrs["gen_ai.operation.name"], c.wantOp)
		}
		if s.Name != c.wantOp {
			t.Errorf("kind %s: span name = %q, want %q", c.kind, s.Name, c.wantOp)
		}
		toolName, hasTool := attrs["gen_ai.tool.name"]
		if c.tool == "" && hasTool {
			t.Errorf("kind %s: unexpected gen_ai.tool.name=%q", c.kind, toolName)
		}
		if c.tool != "" && toolName != c.tool {
			t.Errorf("kind %s: tool = %q, want %q", c.kind, toolName, c.tool)
		}
	}
}
