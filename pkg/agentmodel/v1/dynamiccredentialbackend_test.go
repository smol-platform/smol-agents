package v1

import (
	"strings"
	"testing"
)

func validBackend() DynamicCredentialBackendSpec {
	return DynamicCredentialBackendSpec{
		CredentialName: "github",
		Provider:       "githubApp",
		GitHubApp: &GitHubAppBackendSpec{
			AppID:         "123456",
			PrivateKeyRef: AuthRef{SecretName: "github-app-key", Key: "private-key.pem"},
			ScopePermissions: map[string]map[string]string{
				"github:repo:read": {"contents": "read"},
			},
		},
		Grants: []CredentialGrantSpec{
			{Principal: "spiffe://smol-agents.ai/ns/tenant-a/sa/agent", Scope: "github:repo:read", Repos: []string{"smol-platform/app"}},
		},
	}
}

// M1.20: validation rejects each malformed shape and accepts a conforming one.
func TestValidateDynamicCredentialBackend(t *testing.T) {
	if err := ValidateDynamicCredentialBackend(validBackend()); err != nil {
		t.Fatalf("valid backend rejected: %v", err)
	}

	cases := []struct {
		name string
		mut  func(*DynamicCredentialBackendSpec)
		want string
	}{
		{"empty credentialName", func(s *DynamicCredentialBackendSpec) { s.CredentialName = "" }, "credentialName is required"},
		{"empty provider", func(s *DynamicCredentialBackendSpec) { s.Provider = "" }, "provider is required"},
		{"unknown provider", func(s *DynamicCredentialBackendSpec) { s.Provider = "vault" }, "is invalid"},
		{"missing githubApp block", func(s *DynamicCredentialBackendSpec) { s.GitHubApp = nil }, "githubApp is required"},
		{"missing appID", func(s *DynamicCredentialBackendSpec) { s.GitHubApp.AppID = "" }, "appID is required"},
		{"missing privateKeyRef", func(s *DynamicCredentialBackendSpec) { s.GitHubApp.PrivateKeyRef.SecretName = "" }, "privateKeyRef.secretName is required"},
		{"unparseable principal", func(s *DynamicCredentialBackendSpec) { s.Grants[0].Principal = "tenant-a/agent" }, "not a valid SPIFFE ID"},
		{"empty scope", func(s *DynamicCredentialBackendSpec) { s.Grants[0].Scope = "" }, "scope is required"},
		// c5r.22: a grant whose scope has no githubApp.scopePermissions entry is the
		// four-string misalignment that would crash-loop the mint — rejected here.
		{"grant scope absent from scopePermissions", func(s *DynamicCredentialBackendSpec) { s.Grants[0].Scope = "github:repo:write" }, "not a key of githubApp.scopePermissions"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := validBackend()
			tc.mut(&s)
			err := ValidateDynamicCredentialBackend(s)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

// isSPIFFEID accepts well-formed IDs and rejects malformed ones.
func TestIsSPIFFEID(t *testing.T) {
	ok := []string{"spiffe://smol-agents.ai/ns/t/sa/a", "spiffe://example.org/x"}
	bad := []string{"", "spiffe://", "spiffe://td", "spiffe://td/", "https://x/y", "td/path"}
	for _, id := range ok {
		if !isSPIFFEID(id) {
			t.Errorf("%q should be valid", id)
		}
	}
	for _, id := range bad {
		if isSPIFFEID(id) {
			t.Errorf("%q should be invalid", id)
		}
	}
}
