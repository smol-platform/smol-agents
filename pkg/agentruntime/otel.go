package agentruntime

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	v1 "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

// otelTracer returns the package's tracer.
func otelTracer() trace.Tracer {
	return otel.Tracer("github.com/smol-platform/smol-agents/pkg/agentruntime")
}

// StartRunSpan opens the parent span for an AgentRun and sets the
// canonical OTel GenAI attributes (R-AM-OBS-1).
func StartRunSpan(ctx context.Context, agent v1.Agent, runName string) (context.Context, trace.Span) {
	return otelTracer().Start(ctx, "invoke_agent "+runName,
		trace.WithSpanKind(trace.SpanKindServer),
		trace.WithAttributes(
			attribute.String("gen_ai.operation.name", "invoke_agent"),
			attribute.String("gen_ai.agent.name", runName),
			attribute.String("gen_ai.provider.name", agent.Spec.Model.ProviderRef),
			attribute.String("gen_ai.request.model", agent.Spec.Model.Name),
		),
	)
}

// StartStepSpan opens a child span for a single Step.
func StartStepSpan(ctx context.Context, kind v1.StepKind, toolName string) (context.Context, trace.Span) {
	op := "chat"
	switch kind {
	case v1.StepToolCall, v1.StepObservation, v1.StepToolCallRejected, v1.StepObservationRejected:
		op = "tool_call"
	case v1.StepFinal:
		op = "finalize"
	}
	attrs := []attribute.KeyValue{
		attribute.String("gen_ai.operation.name", op),
		attribute.String("agent.step.kind", string(kind)),
	}
	if toolName != "" {
		attrs = append(attrs, attribute.String("gen_ai.tool.name", toolName))
	}
	return otelTracer().Start(ctx, op,
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(attrs...),
	)
}
