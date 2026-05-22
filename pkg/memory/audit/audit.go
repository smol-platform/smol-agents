// Package audit provides structured per-call audit logging for the memory-mcp
// gateway. Every call — allowed or denied — is recorded with identity,
// operation metadata, and outcome. Content, embeddings, and credentials are
// never included (R-MEM-AUDIT-1, R-MEM-SEC-1).
package audit

import (
	"context"
	"log/slog"
	"time"
)

// Decision records the authorization outcome for a memory call.
type Decision string

const (
	DecisionAllow Decision = "allow"
	DecisionDeny  Decision = "deny"
)

// Record is a single immutable audit event. Every field that could expose
// content, embeddings, or credentials is deliberately absent.
// R-MEM-AUDIT-1: caller SPIFFE id, tenant, retrieverRef, op, namespace,
// filter summary, result count, decision, latency.
type Record struct {
	// CallerSPIFFEID is the full SPIFFE URI of the authenticated caller.
	// Empty only when the call was unauthenticated (decision=deny).
	CallerSPIFFEID string

	// Tenant is the tenant derived from the caller's SPIFFE identity.
	Tenant string

	// RetrieverRef is the namespace-qualified MemoryRetriever name.
	RetrieverRef string

	// Op is the MCP operation name (e.g. "retrieve_memory", "write_memory").
	Op string

	// Namespace is the memory namespace targeted by the call.
	Namespace string

	// FilterSummary is a non-sensitive description of predicates (e.g.
	// "metadata_keys=[env,region]"). Never contains filter values that
	// might encode content.
	FilterSummary string

	// ResultCount is the number of results returned (0 on deny/error).
	ResultCount int

	// Decision is the authorization outcome.
	Decision Decision

	// ErrorKind is the memory.Kind string on error, empty on success.
	ErrorKind string

	// LatencyMs is the wall-clock time from request receipt to response.
	LatencyMs int64

	// Timestamp is when the record was emitted.
	Timestamp time.Time
}

// Logger is the audit sink interface. Implementations must be safe for
// concurrent use. The slog-backed implementation is the default; tests
// can inject a collector.
type Logger interface {
	// Log records a completed call. It must not block the caller significantly.
	Log(ctx context.Context, r Record)
}

// SlogLogger is the production Logger backed by log/slog.
// It emits one structured log line per call at INFO level.
// The log line intentionally omits any content, embedding, or credential field.
type SlogLogger struct {
	Logger *slog.Logger
}

// Log implements Logger.
func (l *SlogLogger) Log(_ context.Context, r Record) {
	lg := l.Logger
	if lg == nil {
		lg = slog.Default()
	}
	lg.Info("memory.audit",
		slog.String("caller", r.CallerSPIFFEID),
		slog.String("tenant", r.Tenant),
		slog.String("retrieverRef", r.RetrieverRef),
		slog.String("op", r.Op),
		slog.String("namespace", r.Namespace),
		slog.String("filterSummary", r.FilterSummary),
		slog.Int("resultCount", r.ResultCount),
		slog.String("decision", string(r.Decision)),
		slog.String("errorKind", r.ErrorKind),
		slog.Int64("latencyMs", r.LatencyMs),
		slog.Time("ts", r.Timestamp),
	)
}

// CollectorLogger captures records for tests. Not for production use.
type CollectorLogger struct {
	Records []Record
}

// Log implements Logger.
func (c *CollectorLogger) Log(_ context.Context, r Record) {
	c.Records = append(c.Records, r)
}
