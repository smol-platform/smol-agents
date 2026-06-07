//go:build e2e_l2

package l2

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// setupLiveLLMSecrets injects real provider API keys into the cluster so the
// claude-code / codex live-harness scenarios can hit api.z.ai / api.openai.com.
// It is a no-op (returning an empty cleanup) unless L2_LIVE_LLM=1.
//
// Keys are read from the DRIVER's env (L2_ANTHROPIC_API_KEY, L2_OPENAI_API_KEY)
// and ferried to the node LEAK-FREE: the value goes to the existing artifact S3
// bucket (the instance role already has S3 read; no IAM change, no SSM Parameter
// Store) over STDIN, then the node fetches it and creates the Secret in one shell
// step — the value never appears in argv, in an SSM SendCommand parameter, or in
// any test log. The returned cleanup best-effort deletes the S3 objects.
//
// If a key is missing the corresponding secret is skipped (a warning is logged,
// never the value); the scenario then fail-skips on the absent secret, which is
// acceptable.
func setupLiveLLMSecrets(ctx context.Context, t *testing.T, e *l2Env, runID string) (cleanup func()) {
	t.Helper()
	noop := func() {}
	if os.Getenv("L2_LIVE_LLM") != "1" {
		return noop
	}

	bucket := os.Getenv("L2_ARTIFACT_BUCKET")
	region := envOrDefault("AWS_REGION", "us-east-2")
	if bucket == "" {
		t.Logf("L2_LIVE_LLM=1 but L2_ARTIFACT_BUCKET is empty — cannot ferry keys leak-free; live-LLM scenarios will fail-skip on the missing secret")
		return noop
	}

	anthropicKey := os.Getenv("L2_ANTHROPIC_API_KEY")
	openaiKey := os.Getenv("L2_OPENAI_API_KEY")

	// Pre-create the namespaces the scenarios use (idempotent).
	for _, ns := range []string{"e2e-live-claude", "e2e-live-codex"} {
		mk := fmt.Sprintf(
			"k0s kubectl create namespace %s --dry-run=client -o yaml | k0s kubectl apply -f -", ns)
		if _, err := e.runSSM(ctx, mk, 60*time.Second); err != nil {
			t.Logf("create namespace %s: %v (live-LLM scenarios may fail-skip)", ns, err)
		}
	}

	var s3keys []string
	put := func(s3key, value, ns, secretName, dataKey, missingLabel string) {
		if value == "" {
			t.Logf("%s is empty — skipping secret %s/%s; the live-LLM scenario will fail-skip on the missing secret", missingLabel, ns, secretName)
			return
		}
		if err := injectLiveKeyViaS3(ctx, e, bucket, region, s3key, value, ns, secretName, dataKey); err != nil {
			t.Logf("inject live key into %s/%s failed: %v (scenario will fail-skip)", ns, secretName, err)
			return
		}
		s3keys = append(s3keys, s3key)
	}

	put(fmt.Sprintf("live-keys/%s/anthropic", runID), anthropicKey,
		"e2e-live-claude", "zai-anthropic-key", "ANTHROPIC_API_KEY", "L2_ANTHROPIC_API_KEY")
	put(fmt.Sprintf("live-keys/%s/openai", runID), openaiKey,
		"e2e-live-codex", "openai-codex-key", "CODEX_API_KEY", "L2_OPENAI_API_KEY")

	return func() {
		for _, s3key := range s3keys {
			c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			cmd := exec.CommandContext(c, "aws", "s3", "rm",
				"s3://"+bucket+"/"+s3key, "--region", region)
			_ = cmd.Run() // best-effort
			cancel()
		}
	}
}

// injectLiveKeyViaS3 ferries `value` to the node without it ever touching argv,
// an SSM parameter, or a log:
//
//  1. PUT the value to S3 with the value on STDIN (encrypted at rest, SSE-AES256).
//  2. On the node, fetch the value and create the Secret in one shell step so the
//     value is only ever materialized on the node, inside a shell variable.
func injectLiveKeyViaS3(ctx context.Context, e *l2Env, bucket, region, s3key, value, ns, secretName, dataKey string) error {
	cmd := exec.CommandContext(ctx, "aws", "s3", "cp", "-",
		"s3://"+bucket+"/"+s3key, "--sse", "AES256", "--region", region)
	cmd.Stdin = strings.NewReader(value)
	if out, err := cmd.CombinedOutput(); err != nil {
		// Never include `value`; `out` is aws-cli's own diagnostic.
		return fmt.Errorf("s3 cp to %s: %w: %s", s3key, err, string(out))
	}

	script := fmt.Sprintf(`set -euo pipefail
V=$(aws s3 cp s3://%s/%s - --region %s)
k0s kubectl -n %s create secret generic %s --from-literal=%s="$V" --dry-run=client -o yaml | k0s kubectl apply -f -`,
		bucket, s3key, region, ns, secretName, dataKey)
	if out, err := e.runSSM(ctx, script, 60*time.Second); err != nil {
		return fmt.Errorf("on-node fetch+create secret %s/%s: %w: %s", ns, secretName, err, string(out))
	}
	return nil
}
