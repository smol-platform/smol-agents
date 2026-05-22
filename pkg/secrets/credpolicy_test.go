package secrets

import (
	"errors"
	"testing"
)

func TestStaticCredentialPolicy(t *testing.T) {
	p := NewStaticCredentialPolicy()
	p.Grant(idA, "github:repo:read", "github", "smol-platform/app")

	base := func(repo string) CredentialRequest {
		return CredentialRequest{
			Name: "github", Principal: idA, Scope: "github:repo:read",
			ReqCtx: map[string]any{"repo": repo},
		}
	}

	if _, err := p.AuthorizeMint(base("smol-platform/app")); err != nil {
		t.Errorf("allow-listed repo should pass: %v", err)
	}

	cases := []struct {
		name string
		req  CredentialRequest
	}{
		{"repo not allow-listed", base("evil/exfil")},
		{"ungranted principal", CredentialRequest{Name: "github", Principal: idB, Scope: "github:repo:read", ReqCtx: map[string]any{"repo": "smol-platform/app"}}},
		{"ungranted scope", CredentialRequest{Name: "github", Principal: idA, Scope: "github:repo:write", ReqCtx: map[string]any{"repo": "smol-platform/app"}}},
		{"wrong credential name", CredentialRequest{Name: "gitlab", Principal: idA, Scope: "github:repo:read", ReqCtx: map[string]any{"repo": "smol-platform/app"}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := p.AuthorizeMint(c.req); !errors.Is(err, ErrUnauthorized) {
				t.Errorf("want ErrUnauthorized, got %v", err)
			}
		})
	}
}
