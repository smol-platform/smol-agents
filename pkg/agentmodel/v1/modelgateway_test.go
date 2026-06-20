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
	}
	for _, tc := range cases {
		err := ValidateModelGateway(tc.s)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want error containing %q", tc.name, err, tc.want)
		}
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
