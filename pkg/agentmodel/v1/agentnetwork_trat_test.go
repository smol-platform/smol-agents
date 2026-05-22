package v1

import (
	"strings"
	"testing"
)

func httpRes() ResourceTarget {
	return ResourceTarget{Name: "gh", Kind: "http", Gateway: "https://api.github.com",
		JWTAudience: "spiffe://smol-agents.ai/gh", LocalPort: 8080}
}

func ipSpec(p IdentityProxySpec) AgentNetworkSpec {
	return AgentNetworkSpec{Kind: NetworkIdentityProxy, IdentityProxy: &p}
}

func TestValidate_TraT_OK(t *testing.T) {
	r := httpRes()
	r.TraT = &TraTInjection{Scope: "github:repo:read"}
	s := ipSpec(IdentityProxySpec{Resources: []ResourceTarget{r}, TTS: &TTSRef{URL: "https://tts/token"}})
	if err := ValidateAgentNetwork(s); err != nil {
		t.Fatalf("valid trat resource rejected: %v", err)
	}
}

func TestValidate_Credential_OK(t *testing.T) {
	r := httpRes()
	r.Credential = &CredentialInjection{Name: "github", Scope: "github:repo:read"}
	s := ipSpec(IdentityProxySpec{
		Resources: []ResourceTarget{r},
		TTS:       &TTSRef{URL: "https://tts/token", JWKSURL: "https://tts/jwks"},
	})
	if err := ValidateAgentNetwork(s); err != nil {
		t.Fatalf("valid credential resource rejected: %v", err)
	}
}

func TestValidate_TraT_RejectsTCP(t *testing.T) {
	r := ResourceTarget{Name: "db", Kind: "tcp", Gateway: "10.0.0.5:5432",
		LocalAddr: "127.0.0.1:5432", Authorize: []string{"spiffe://smol-agents.ai/db"}}
	r.TraT = &TraTInjection{Scope: "x"}
	s := ipSpec(IdentityProxySpec{Resources: []ResourceTarget{r}, TTS: &TTSRef{URL: "https://tts/token"}})
	mustErr(t, ValidateAgentNetwork(s), "requires kind=http")
}

func TestValidate_TraT_RequiresScope(t *testing.T) {
	r := httpRes()
	r.TraT = &TraTInjection{}
	s := ipSpec(IdentityProxySpec{Resources: []ResourceTarget{r}, TTS: &TTSRef{URL: "https://tts/token"}})
	mustErr(t, ValidateAgentNetwork(s), "trat.scope is required")
}

func TestValidate_RequiresTTS(t *testing.T) {
	r := httpRes()
	r.TraT = &TraTInjection{Scope: "s"}
	s := ipSpec(IdentityProxySpec{Resources: []ResourceTarget{r}}) // no TTS
	mustErr(t, ValidateAgentNetwork(s), "tts.url is required")
}

func TestValidate_Credential_RequiresJWKS(t *testing.T) {
	r := httpRes()
	r.Credential = &CredentialInjection{Name: "github", Scope: "github:repo:read"}
	s := ipSpec(IdentityProxySpec{Resources: []ResourceTarget{r}, TTS: &TTSRef{URL: "https://tts/token"}}) // no JWKSURL
	mustErr(t, ValidateAgentNetwork(s), "tts.jwksUrl is required")
}

func mustErr(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error containing %q, got nil", want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("error %q does not contain %q", err.Error(), want)
	}
}
