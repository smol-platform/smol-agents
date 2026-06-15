//go:build envtest

package webhooks

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	amv1 "github.com/smol-platform/smol-agents/operator/api/agentmodel/v1"
	pure "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

// warningRecorder captures admission warnings surfaced by the apiserver on
// the client's responses — the same channel kubectl prints them on.
type warningRecorder struct {
	mu       sync.Mutex
	warnings []string
}

func (w *warningRecorder) HandleWarningHeader(_ int, _ string, msg string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.warnings = append(w.warnings, msg)
}

func (w *warningRecorder) all() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.warnings...)
}

// projectRootWebhooks walks up from the test working dir to the go.mod root.
func projectRootWebhooks(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above test dir")
		}
		dir = parent
	}
}

// TestEnvtest_ClaudeWriteRuntimeWarning boots a real apiserver + the real
// Agent gate webhook server and asserts the rv1.3 claude-write warning is
// delivered end-to-end on create (live verification without a cluster).
func TestEnvtest_ClaudeWriteRuntimeWarning(t *testing.T) {
	log.SetLogger(zap.New(zap.UseDevMode(true)))
	root := projectRootWebhooks(t)

	failPolicy := admissionv1.Fail
	sideEffects := admissionv1.SideEffectClassNone
	path := "/validate-runtime-agents-smol-agents-ai-v1-agent"
	vwc := &admissionv1.ValidatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "agent-gate-test"},
		Webhooks: []admissionv1.ValidatingWebhook{{
			Name:                    "vagent.test.smol-agents.ai",
			AdmissionReviewVersions: []string{"v1"},
			SideEffects:             &sideEffects,
			FailurePolicy:           &failPolicy,
			ClientConfig: admissionv1.WebhookClientConfig{
				Service: &admissionv1.ServiceReference{Name: "noop", Namespace: "default", Path: &path},
			},
			Rules: []admissionv1.RuleWithOperations{{
				Operations: []admissionv1.OperationType{admissionv1.Create, admissionv1.Update},
				Rule: admissionv1.Rule{
					APIGroups:   []string{"runtime.agents.smol-agents.ai"},
					APIVersions: []string{"v1"},
					Resources:   []string{"agents"},
				},
			}},
		}},
	}

	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join(root, "operator", "config", "crd")},
		ErrorIfCRDPathMissing: true,
		WebhookInstallOptions: envtest.WebhookInstallOptions{
			ValidatingWebhooks: []*admissionv1.ValidatingWebhookConfiguration{vwc},
		},
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("envtest Start: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := amv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	opts := env.WebhookInstallOptions
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme: scheme,
		WebhookServer: webhook.NewServer(webhook.Options{
			Host: opts.LocalServingHost, Port: opts.LocalServingPort, CertDir: opts.LocalServingCertDir,
		}),
		Metrics: metricsserver.Options{BindAddress: "0"},
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	// runc default mirrors a kataless dev cluster — the posture the warning exists for.
	if err := SetupAgentPolicyGateWebhook(mgr, "runc"); err != nil {
		t.Fatalf("SetupAgentPolicyGateWebhook: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = mgr.Start(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	// Client that records apiserver warning headers.
	rec := &warningRecorder{}
	wcfg := rest.CopyConfig(cfg)
	wcfg.WarningHandler = rec
	cli, err := client.New(wcfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}

	// Wait for the webhook server to come up (dial via a probe create that
	// would 500 while the endpoint is down because failurePolicy=Fail).
	agent := &amv1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "claude-writer", Namespace: "default"}}
	agent.Spec.Mode = pure.ModeHarness
	agent.Spec.Instructions = "write a file"
	agent.Spec.Budget = pure.Budget{MaxSteps: 1, MaxTokens: 1000, MaxWallClockSeconds: 60}
	agent.Spec.Harness = &pure.HarnessSpec{
		Kind: pure.HarnessClaudeCode,
		CLI:  &pure.HarnessCLISpec{AllowedTools: []string{"Write"}},
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		err = cli.Create(ctx, agent)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("create Agent: %v", err)
		}
		time.Sleep(250 * time.Millisecond)
	}

	var hit bool
	for _, w := range rec.all() {
		if strings.Contains(w, "--dangerously-skip-permissions") && strings.Contains(w, `runtimeClass runc`) {
			hit = true
		}
	}
	if !hit {
		t.Fatalf("expected the claude-write warning on create, got warnings: %v", rec.all())
	}

	// Control: a kata Agent must create with no claude-write warning.
	rec2 := &warningRecorder{}
	wcfg2 := rest.CopyConfig(cfg)
	wcfg2.WarningHandler = rec2
	cli2, err := client.New(wcfg2, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}
	kata := agent.DeepCopy()
	kata.ResourceVersion = ""
	kata.Name = "claude-writer-kata"
	kata.Spec.Sandbox = pure.SandboxSpec{RuntimeClass: "kata-fc"}
	if err := cli2.Create(ctx, kata); err != nil {
		t.Fatalf("create kata Agent: %v", err)
	}
	for _, w := range rec2.all() {
		if strings.Contains(w, "--dangerously-skip-permissions") {
			t.Fatalf("kata Agent must not warn, got: %v", rec2.all())
		}
	}
}
