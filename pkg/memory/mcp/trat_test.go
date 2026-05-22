package mcp_test

// Tests for R-MEM-AUTH-3: TraT-gated mutations.
//
// Convention under test: when a MemoryRetriever has MutationsTraT=true,
// the caller must pass the compact TraT JWT in the "trat" field of the
// tool arguments for write_memory and delete_memory.
//
// The gateway:
//  1. Rejects mutations with no "trat" field → PermissionDenied.
//  2. Rejects mutations with an invalid/expired TraT → PermissionDenied.
//  3. Rejects mutations where the TraT subject != caller SPIFFE ID → PermissionDenied.
//  4. Accepts mutations with a valid TraT whose subject matches the caller.
//  5. When TratVerifier is nil + MutationsTraT=true → fail-closed PermissionDenied.

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	v1 "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
	"github.com/smol-platform/smol-agents/pkg/memory"
	"github.com/smol-platform/smol-agents/pkg/memory/api"
	"github.com/smol-platform/smol-agents/pkg/memory/audit"
	"github.com/smol-platform/smol-agents/pkg/memory/mcp"
	"github.com/smol-platform/smol-agents/pkg/memory/quota"
	"github.com/smol-platform/smol-agents/pkg/memory/store"
	"github.com/smol-platform/smol-agents/pkg/trat"
)

// ── Fake TraT verifier ─────────────────────────────────────────────────────

// fakeTratVerifier is a controllable trat.Verifier for tests.
type fakeTratVerifier struct {
	claims *trat.Claims
	err    error
}

func (f *fakeTratVerifier) Verify(_ context.Context, compact string) (*trat.Claims, error) {
	if f.err != nil {
		return nil, f.err
	}
	c := *f.claims // copy
	c.Compact = compact
	return &c, nil
}

var _ trat.Verifier = (*fakeTratVerifier)(nil)

// ── Constants ─────────────────────────────────────────────────────────────

const (
	tratCallerID    = "spiffe://smol-agents.ai/ns/team/sa/writer"
	tratRetriever   = "ns/test-retriever"
	tratFakeCompact = "fake.trat.compact"
)

// ── Helpers ────────────────────────────────────────────────────────────────

// tratWriterSpec builds a MemoryRetrieverSpec with MutationsTraT=true and
// write+delete grants for tratCallerID.
func tratWriterSpec(ops ...v1.MemoryOperation) v1.MemoryRetrieverSpec {
	if len(ops) == 0 {
		ops = []v1.MemoryOperation{v1.MemoryOpWrite, v1.MemoryOpDelete}
	}
	return v1.MemoryRetrieverSpec{
		Stores:        []string{"s"},
		TopK:          10,
		MutationsTraT: true,
		Policy: []v1.MemoryGrant{{
			Identity:   tratCallerID,
			Operations: ops,
			Namespaces: []string{"*"},
		}},
	}
}

// newTratServer builds a Gateway with MutationsTraT=true, the given verifier,
// and a fake worker. Returns the httptest.Server and audit collector.
func newTratServer(t *testing.T, verifier trat.Verifier, spec v1.MemoryRetrieverSpec, ref string) (*httptest.Server, *audit.CollectorLogger) {
	t.Helper()
	rs := store.NewFakeStore()
	rs.Add(ref, store.RetrieverInfo{Spec: spec, WorkerURL: "http://fake-worker"})

	col := &audit.CollectorLogger{}
	gw := &mcp.Gateway{
		Auth:         mcp.AuthConfig{},
		Retrievers:   rs,
		Quota:        quota.NewEnforcer(),
		AuditLog:     col,
		TratVerifier: verifier,
		WorkerFactory: func(_ string) api.RetrievalService {
			return &fakeWorker{
				writeResp: &api.WriteResponse{Result: memory.WriteResult{ID: "doc-1"}},
			}
		},
	}
	srv := httptest.NewServer(mcp.NewDispatcher(gw))
	t.Cleanup(srv.Close)
	return srv, col
}

// assertRPCError validates the JSON-RPC error code and logs a context message.
func assertRPCError(t *testing.T, resp map[string]any, wantCode int, desc string) {
	t.Helper()
	rpcErr, ok := resp["error"].(map[string]any)
	if !ok {
		t.Fatalf("%s: expected error response, got result: %v", desc, resp["result"])
	}
	code := int(rpcErr["code"].(float64))
	if code != wantCode {
		t.Fatalf("%s: want RPC code %d, got %d (msg=%v)", desc, wantCode, code, rpcErr["message"])
	}
}

// ── Tests ─────────────────────────────────────────────────────────────────

// TestTraT_MissingField verifies that omitting "trat" when MutationsTraT=true
// returns PermissionDenied.
func TestTraT_MissingField(t *testing.T) {
	validVerifier := &fakeTratVerifier{
		claims: &trat.Claims{Subject: tratCallerID, Expiry: time.Now().Add(time.Hour)},
	}
	srv, col := newTratServer(t, validVerifier, tratWriterSpec(), tratRetriever)

	resp := callTool(t, srv, tratCallerID, "write_memory", map[string]any{
		"content":      "hello",
		"retrieverRef": tratRetriever,
		// No "trat" field.
	})

	assertRPCError(t, resp, mcp.CodePermissionDenied, "missing trat field")

	if len(col.Records) == 0 || col.Records[0].Decision != audit.DecisionDeny {
		t.Error("missing TraT must be audited as deny")
	}
}

// TestTraT_InvalidToken verifies that a token rejected by the verifier
// returns PermissionDenied.
func TestTraT_InvalidToken(t *testing.T) {
	verifyErr := errors.New("trat: verification failed: expired")
	srv, col := newTratServer(t, &fakeTratVerifier{err: verifyErr}, tratWriterSpec(), tratRetriever)

	resp := callTool(t, srv, tratCallerID, "write_memory", map[string]any{
		"content":      "hello",
		"retrieverRef": tratRetriever,
		"trat":         "bad.token",
	})

	assertRPCError(t, resp, mcp.CodePermissionDenied, "invalid TraT")

	if len(col.Records) == 0 || col.Records[0].Decision != audit.DecisionDeny {
		t.Error("invalid TraT must be audited as deny")
	}
}

// TestTraT_SubjectMismatch verifies that a TraT whose subject differs from the
// caller is rejected (prevents replay by a different identity).
func TestTraT_SubjectMismatch(t *testing.T) {
	otherIdentity := "spiffe://smol-agents.ai/ns/team/sa/other"
	srv, col := newTratServer(t, &fakeTratVerifier{
		claims: &trat.Claims{
			Subject: otherIdentity, // different from tratCallerID
			Expiry:  time.Now().Add(time.Hour),
		},
	}, tratWriterSpec(), tratRetriever)

	resp := callTool(t, srv, tratCallerID, "write_memory", map[string]any{
		"content":      "hello",
		"retrieverRef": tratRetriever,
		"trat":         tratFakeCompact,
	})

	assertRPCError(t, resp, mcp.CodePermissionDenied, "TraT subject mismatch")

	if len(col.Records) == 0 || col.Records[0].Decision != audit.DecisionDeny {
		t.Error("subject mismatch must be audited as deny")
	}
}

// TestTraT_Allow verifies that a valid TraT with matching subject allows the mutation.
func TestTraT_Allow(t *testing.T) {
	srv, col := newTratServer(t, &fakeTratVerifier{
		claims: &trat.Claims{
			Subject: tratCallerID,
			Expiry:  time.Now().Add(time.Hour),
		},
	}, tratWriterSpec(), tratRetriever)

	resp := callTool(t, srv, tratCallerID, "write_memory", map[string]any{
		"content":      "hello",
		"retrieverRef": tratRetriever,
		"trat":         tratFakeCompact,
	})

	if resp["error"] != nil {
		t.Fatalf("expected allowed write with valid TraT, got error: %v", resp["error"])
	}
	if len(col.Records) == 0 || col.Records[0].Decision != audit.DecisionAllow {
		t.Error("valid TraT write must be audited as allow")
	}
}

// TestTraT_AllowDelete verifies TraT enforcement works for delete_memory too.
func TestTraT_AllowDelete(t *testing.T) {
	spec := tratWriterSpec(v1.MemoryOpDelete)
	srv, _ := newTratServer(t, &fakeTratVerifier{
		claims: &trat.Claims{
			Subject: tratCallerID,
			Expiry:  time.Now().Add(time.Hour),
		},
	}, spec, tratRetriever)

	resp := callTool(t, srv, tratCallerID, "delete_memory", map[string]any{
		"id":           "doc-1",
		"retrieverRef": tratRetriever,
		"trat":         tratFakeCompact,
	})

	if resp["error"] != nil {
		t.Fatalf("expected allowed delete with valid TraT, got error: %v", resp["error"])
	}
}

// TestTraT_NilVerifierFailClosed verifies that when TratVerifier is nil and
// MutationsTraT=true, mutations are rejected (fail-closed). R-MEM-SEC-1.
func TestTraT_NilVerifierFailClosed(t *testing.T) {
	srv, col := newTratServer(t, nil /* no verifier */, tratWriterSpec(), tratRetriever)

	resp := callTool(t, srv, tratCallerID, "write_memory", map[string]any{
		"content":      "hello",
		"retrieverRef": tratRetriever,
		"trat":         tratFakeCompact,
	})

	assertRPCError(t, resp, mcp.CodePermissionDenied, "nil verifier must fail-closed")

	if len(col.Records) == 0 || col.Records[0].Decision != audit.DecisionDeny {
		t.Error("nil verifier deny must be audited")
	}
}

// TestTraT_NotRequired verifies that when MutationsTraT=false, no TraT is
// needed and omitting it does not cause a denial.
func TestTraT_NotRequired(t *testing.T) {
	const noTratRef = "ns/no-trat"
	rs := store.NewFakeStore()
	rs.Add(noTratRef, store.RetrieverInfo{
		Spec: v1.MemoryRetrieverSpec{
			Stores:        []string{"s"},
			TopK:          10,
			MutationsTraT: false,
			Policy: []v1.MemoryGrant{{
				Identity:   tratCallerID,
				Operations: []v1.MemoryOperation{v1.MemoryOpWrite},
				Namespaces: []string{"*"},
			}},
		},
		WorkerURL: "http://fake-worker",
	})

	gw := &mcp.Gateway{
		Auth:       mcp.AuthConfig{},
		Retrievers: rs,
		Quota:      quota.NewEnforcer(),
		// TratVerifier nil — must not be called when MutationsTraT=false.
		WorkerFactory: func(_ string) api.RetrievalService {
			return &fakeWorker{
				writeResp: &api.WriteResponse{Result: memory.WriteResult{ID: "doc-1"}},
			}
		},
	}
	srv := httptest.NewServer(mcp.NewDispatcher(gw))
	t.Cleanup(srv.Close)

	resp := callTool(t, srv, tratCallerID, "write_memory", map[string]any{
		"content":      "hello",
		"retrieverRef": noTratRef,
		// No "trat" field — and none required.
	})

	if resp["error"] != nil {
		t.Fatalf("write without TraT should be allowed when MutationsTraT=false: %v", resp["error"])
	}
}
