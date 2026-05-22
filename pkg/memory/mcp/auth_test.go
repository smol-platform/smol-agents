package mcp_test

import (
	"net/http"
	"testing"

	"github.com/smol-platform/smol-agents/pkg/memory/mcp"
)

// tenantFromPath exercises the exported auth logic indirectly through
// the insecureTestIdentity helper (package-internal). We test
// the SPIFFE-path-to-tenant derivation via ExtractIdentity with a
// synthesized unsigned JWT, using the insecure (no BundleSource) code path.

func TestAuthConfig_ExtractIdentity_MissingAuthHeader(t *testing.T) {
	cfg := mcp.AuthConfig{}
	req := fakeHTTPRequest(t, "")
	_, err := cfg.ExtractIdentity(req)
	if err == nil {
		t.Fatal("expected error for missing Authorization header")
	}
}

func TestAuthConfig_ExtractIdentity_MalformedBearer(t *testing.T) {
	cfg := mcp.AuthConfig{}
	req := fakeHTTPRequestWithAuth(t, "Basic abc123")
	_, err := cfg.ExtractIdentity(req)
	if err == nil {
		t.Fatal("expected error for non-Bearer Authorization header")
	}
}

func TestAuthConfig_ExtractIdentity_InvalidJWT(t *testing.T) {
	cfg := mcp.AuthConfig{}
	req := fakeHTTPRequestWithAuth(t, "Bearer notajwt")
	_, err := cfg.ExtractIdentity(req)
	if err == nil {
		t.Fatal("expected error for malformed JWT")
	}
}

func TestAuthConfig_ExtractIdentity_TenantFromPath(t *testing.T) {
	cases := []struct {
		spiffeID   string
		wantTenant string
		wantErr    bool
	}{
		{
			spiffeID:   "spiffe://smol-agents.ai/ns/team-alpha/sa/coder",
			wantTenant: "team-alpha",
		},
		{
			spiffeID:   "spiffe://smol-agents.ai/ns/default",
			wantTenant: "default",
		},
		{
			spiffeID:   "spiffe://smol-agents.ai/ns/team-beta/sa/agent/run/abc",
			wantTenant: "team-beta",
		},
		{
			// No /ns/ segment → error
			spiffeID: "spiffe://smol-agents.ai/workload/foo",
			wantErr:  true,
		},
	}

	cfg := mcp.AuthConfig{} // insecure mode
	for _, tc := range cases {
		t.Run(tc.spiffeID, func(t *testing.T) {
			token := buildJWT(tc.spiffeID, "memory-mcp")
			req := fakeHTTPRequestWithAuth(t, "Bearer "+token)
			identity, err := cfg.ExtractIdentity(req)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q", tc.spiffeID)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if identity.Tenant != tc.wantTenant {
				t.Fatalf("tenant = %q, want %q", identity.Tenant, tc.wantTenant)
			}
			if identity.SPIFFEID != tc.spiffeID {
				t.Fatalf("SPIFFEID = %q, want %q", identity.SPIFFEID, tc.spiffeID)
			}
		})
	}
}

// fakeHTTPRequest builds a minimal *http.Request for auth testing.
func fakeHTTPRequest(t *testing.T, auth string) *http.Request {
	t.Helper()
	return fakeHTTPRequestWithAuth(t, auth)
}

func fakeHTTPRequestWithAuth(t *testing.T, auth string) *http.Request {
	t.Helper()
	req, err := http.NewRequest("POST", "/mcp", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	return req
}
