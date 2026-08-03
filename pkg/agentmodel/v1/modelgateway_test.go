package v1

import (
	"strings"
	"testing"
)

func TestValidateModelGateway(t *testing.T) {
	ok := ModelGatewaySpec{Provider: "hermes", Image: "nousresearch/hermes-agent:latest"}
	if err := ValidateModelGateway(ok); err != nil {
		t.Errorf("valid hermes gateway rejected: %v", err)
	}

	cases := []struct {
		name string
		s    ModelGatewaySpec
		want string
	}{
		{"missing provider", ModelGatewaySpec{Image: "x"}, "provider is required"},
		{"bad provider", ModelGatewaySpec{Provider: "vllm", Image: "x"}, "is invalid"},
		{"missing image", ModelGatewaySpec{Provider: "hermes"}, "image is required"},
		{"env without name", ModelGatewaySpec{Provider: "hermes", Image: "x", Env: []HarnessEnvVar{{Value: "v"}}}, "env[0].name is required"},
		{"env secretRef without secretName", ModelGatewaySpec{Provider: "hermes", Image: "x", Env: []HarnessEnvVar{{Name: "K", SecretRef: &AuthRef{}}}}, "secretRef.secretName is required"},
		{"ui sharedSecret without secretRef", ModelGatewaySpec{Provider: "hermes", Image: "x", UI: &GatewayUISpec{Expose: true, Auth: GatewayUIAuth{Mode: "sharedSecret"}}}, "ui.auth.secretRef.secretName is required"},
		{"ui oidcProxy without oidc", ModelGatewaySpec{Provider: "hermes", Image: "x", UI: &GatewayUISpec{Expose: true, Auth: GatewayUIAuth{Mode: "oidcProxy"}}}, "ui.auth.oidc is required"},
		{"ui oidcProxy missing fields", ModelGatewaySpec{Provider: "hermes", Image: "x", UI: &GatewayUISpec{Expose: true, Auth: GatewayUIAuth{Mode: "oidcProxy", OIDC: &GatewayUIOIDC{Issuer: "https://i"}}}}, "clientID is required"},
		{"ui oidcProxy partial pinned urls", ModelGatewaySpec{Provider: "hermes", Image: "x", UI: &GatewayUISpec{Expose: true, Auth: GatewayUIAuth{Mode: "oidcProxy", OIDC: &GatewayUIOIDC{Issuer: "https://i", ClientID: "c", RedirectURL: "https://r/cb", SecretRef: &AuthRef{SecretName: "s"}, LoginURL: "https://i/auth"}}}}, "must be set together"},
		{"ui oidcNative without oidc", ModelGatewaySpec{Provider: "hermes", Image: "x", UI: &GatewayUISpec{Expose: true, Auth: GatewayUIAuth{Mode: "oidcNative"}}}, "ui.auth.oidc is required for mode=oidcNative"},
		{"ui oidcNative missing redirectURL", ModelGatewaySpec{Provider: "hermes", Image: "x", UI: &GatewayUISpec{Expose: true, Auth: GatewayUIAuth{Mode: "oidcNative", OIDC: &GatewayUIOIDC{Issuer: "https://i", ClientID: "c"}}}}, "redirectURL is required"},
		{"ui missing mode", ModelGatewaySpec{Provider: "hermes", Image: "x", UI: &GatewayUISpec{Expose: true}}, "ui.auth.mode is required"},
		{"ui bad mode", ModelGatewaySpec{Provider: "hermes", Image: "x", UI: &GatewayUISpec{Expose: true, Auth: GatewayUIAuth{Mode: "basic"}}}, "is invalid"},
		{"ui port clashes with gateway port", ModelGatewaySpec{Provider: "hermes", Image: "x", Port: 8643, UI: &GatewayUISpec{Expose: true, Port: 8643, Auth: GatewayUIAuth{Mode: "sharedSecret", SecretRef: &AuthRef{SecretName: "s"}}}}, "ui.port must differ"},
	}
	for _, tc := range cases {
		err := ValidateModelGateway(tc.s)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want error containing %q", tc.name, err, tc.want)
		}
	}
}

func TestValidateModelGateway_RejectsSecretShapedConfig(t *testing.T) {
	const fakeKey = "sk-FAKEabcdEFGH12345678"
	s := ModelGatewaySpec{Provider: "hermes", Image: "x", Config: "model: glm-4.6\nport: 8642\napi_key: " + fakeKey + "\n"}
	err := ValidateModelGateway(s)
	if err == nil {
		t.Fatalf("secret-shaped config accepted, want rejection")
	}
	// The point of this test: the error must never echo the matched secret
	// value, since it is written verbatim into ModelGatewayStatus.Message.
	if strings.Contains(err.Error(), fakeKey) {
		t.Fatalf("validation error leaks the secret value: %v", err)
	}
	if !strings.Contains(err.Error(), "config line 3") {
		t.Errorf("error does not name the offending line: %v", err)
	}
}

func TestValidateModelGateway_ConfigNoFalsePositives(t *testing.T) {
	s := ModelGatewaySpec{Provider: "hermes", Image: "x", Config: `model: glm-4.6
server:
  host: 0.0.0.0
  port: 8642
dashboard:
  enabled: true
  url: https://hermes.example.com
`}
	if err := ValidateModelGateway(s); err != nil {
		t.Errorf("ordinary config rejected: %v", err)
	}
}

func TestValidateModelGateway_EmptyConfigPasses(t *testing.T) {
	s := ModelGatewaySpec{Provider: "hermes", Image: "x", Config: ""}
	if err := ValidateModelGateway(s); err != nil {
		t.Errorf("empty config rejected: %v", err)
	}
}

func TestModelGatewaySpec_EffectivePort(t *testing.T) {
	if got := (ModelGatewaySpec{Provider: "hermes"}).EffectivePort(); got != HermesDefaultPort {
		t.Errorf("hermes default port = %d, want %d", got, HermesDefaultPort)
	}
	if got := (ModelGatewaySpec{Provider: "hermes", Port: 9000}).EffectivePort(); got != 9000 {
		t.Errorf("explicit port = %d, want 9000", got)
	}
}

func TestModelGatewaySpec_EffectiveUIPort(t *testing.T) {
	if got := (ModelGatewaySpec{}).EffectiveUIPort(); got != GatewayUIDefaultPort {
		t.Errorf("default UI port = %d, want %d", got, GatewayUIDefaultPort)
	}
	if got := (ModelGatewaySpec{UI: &GatewayUISpec{Port: 9100}}).EffectiveUIPort(); got != 9100 {
		t.Errorf("explicit UI port = %d, want 9100", got)
	}
	// A valid sharedSecret UI passes validation.
	if err := ValidateModelGateway(ModelGatewaySpec{Provider: "hermes", Image: "x",
		UI: &GatewayUISpec{Expose: true, Auth: GatewayUIAuth{Mode: "sharedSecret", SecretRef: &AuthRef{SecretName: "ui-htpasswd"}}}}); err != nil {
		t.Errorf("valid sharedSecret UI rejected: %v", err)
	}
	// A valid oidcProxy UI (with pinned back-channel) passes validation.
	if err := ValidateModelGateway(ModelGatewaySpec{Provider: "hermes", Image: "x",
		UI: &GatewayUISpec{Expose: true, Auth: GatewayUIAuth{Mode: "oidcProxy", OIDC: &GatewayUIOIDC{
			Issuer: "https://dex.example.com", ClientID: "hermes-dashboard", RedirectURL: "https://h.example.com/oauth2/callback",
			SecretRef: &AuthRef{SecretName: "oidc"}, LoginURL: "https://dex.example.com/auth", RedeemURL: "http://dex.dex.svc:5556/token", JWKSURL: "http://dex.dex.svc:5556/keys",
		}}}}); err != nil {
		t.Errorf("valid oidcProxy UI rejected: %v", err)
	}
	// A valid oidcNative UI (public PKCE, no secret) passes validation.
	if err := ValidateModelGateway(ModelGatewaySpec{Provider: "hermes", Image: "x",
		UI: &GatewayUISpec{Expose: true, Auth: GatewayUIAuth{Mode: "oidcNative", OIDC: &GatewayUIOIDC{
			Issuer: "https://dex.example.com", ClientID: "hermes-dashboard", RedirectURL: "https://h.example.com/auth/callback",
		}}}}); err != nil {
		t.Errorf("valid oidcNative UI rejected: %v", err)
	}
}
