// Command manager is the operator entrypoint.
package main

import (
	"bytes"
	"flag"
	"os"
	"strconv"
	"strings"
	"time"

	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"k8s.io/apimachinery/pkg/runtime"

	amv1 "github.com/smol-platform/smol-agents/operator/api/agentmodel/v1"
	v1 "github.com/smol-platform/smol-agents/operator/api/v1"
	"github.com/smol-platform/smol-agents/operator/internal/controllers"
	"github.com/smol-platform/smol-agents/operator/internal/controllers/agentmodel"
	memoryctrl "github.com/smol-platform/smol-agents/operator/internal/controllers/memory"
	"github.com/smol-platform/smol-agents/operator/internal/webhooks"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	_ = clientgoscheme.AddToScheme(scheme)
	_ = v1.AddToScheme(scheme)
	_ = amv1.AddToScheme(scheme)
}

func main() {
	var metricsAddr, probeAddr string
	var enableLeaderElection bool
	var defaultRunRuntimeClass string
	var allowHostRuntime bool
	var sessionNATSURL string
	var natsAccountSeedFile string
	var a2aMaxDepth int
	var maxConcurrentReconciles int
	var runDeadlineMultiplier float64
	var defaultApprovalTimeout time.Duration
	var defaultNamespaceRunConcurrency int
	var enableAdmissionQueue bool
	var maxRunPriority int
	var allowedStdioMCP string
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8443", "metrics endpoint")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "health/readiness probe address")
	flag.BoolVar(&enableLeaderElection, "leader-elect", true, "enable leader election")
	flag.StringVar(&defaultRunRuntimeClass, "default-run-runtime-class", "kata-fc",
		"sandbox RuntimeClass applied to AgentRun pods when the Agent doesn't override it (empty = kata-fc)")
	flag.BoolVar(&allowHostRuntime, "allow-host-runtime", false,
		"permit runc (shared host kernel) for AgentRun pods on clusters with no sandbox runtime; otherwise runc is a fail-closed R-SBX-1 violation")
	flag.StringVar(&sessionNATSURL, "session-nats-url", os.Getenv("SESSION_NATS_URL"),
		"NATS JetStream URL for AgentSession turn delivery (the gateway path); empty leaves session workers on the on-disk inbox")
	flag.StringVar(&natsAccountSeedFile, "nats-account-seed-file", os.Getenv("NATS_ACCOUNT_SEED_FILE"),
		"path to the NATS account signing seed (mounted Secret) used to mint per-namespace worker credentials (M2.20); empty leaves session workers connecting unauthenticated")
	flag.IntVar(&a2aMaxDepth, "a2a-max-depth", 4,
		"A2A delegation recursion ceiling injected into loop run pods (M3.5); a child at this depth may not spawn further children")
	flag.IntVar(&maxConcurrentReconciles, "max-concurrent-reconciles", 4,
		"max parallel reconciles per agent-model controller (AgentRun/AgentSession)")
	flag.Float64Var(&runDeadlineMultiplier, "run-deadline-multiplier", 1.5,
		"scales an AgentRun's wall-clock budget into the pod ActiveDeadlineSeconds hard backstop (M1.10)")
	flag.DurationVar(&defaultApprovalTimeout, "default-approval-timeout", time.Hour,
		"expiry for an un-decided pre-run approval when the Agent sets none (M5)")
	flag.IntVar(&defaultNamespaceRunConcurrency, "default-namespace-run-concurrency", 0,
		"per-namespace cap on Running AgentRuns when no AgentRunQuota sets one (0 = unlimited, M1.12)")
	flag.BoolVar(&enableAdmissionQueue, "enable-admission-queue", false,
		"per-namespace priority ordering of queued runs at the concurrency cap (M1.13; off = M1.12 behavior)")
	flag.IntVar(&maxRunPriority, "max-run-priority", 1000, "clamp for AgentRun.spec.priority (M1.13)")
	flag.StringVar(&allowedStdioMCP, "allowed-stdio-mcp", "",
		"comma-separated allow-list of approved stdio MCP server URLs (M2.15; empty = deny all stdio MCP)")
	opts := zap.Options{Development: false}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	// The A2A recursion ceiling reaches the run-pod builder via env (M3.5).
	if a2aMaxDepth > 0 {
		_ = os.Setenv("SMOL_AGENTS_A2A_MAX_DEPTH", strconv.Itoa(a2aMaxDepth))
	}

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	// Load the NATS account signing seed for per-namespace worker credentials
	// (M2.20). Mounted from a Secret; absent leaves session workers unauthenticated.
	var natsAccountSeed []byte
	if natsAccountSeedFile != "" {
		b, rerr := os.ReadFile(natsAccountSeedFile)
		if rerr != nil {
			setupLog.Error(rerr, "unable to read --nats-account-seed-file", "path", natsAccountSeedFile)
			os.Exit(1)
		}
		natsAccountSeed = bytes.TrimSpace(b)
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                  scheme,
		Metrics:                 metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress:  probeAddr,
		LeaderElection:          enableLeaderElection,
		LeaderElectionID:        "smol-agents-operator.smol-agents.ai",
		LeaderElectionNamespace: "smol-agents-system",
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	r := &controllers.SmolAgentReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}
	if err := r.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to register SmolAgent controller")
		os.Exit(1)
	}
	pr := &controllers.SmolAgentPlatformReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}
	if err := pr.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to register SmolAgentPlatform controller")
		os.Exit(1)
	}
	if err := (&controllers.AgentNodePoolReconciler{
		Client: mgr.GetClient(), Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to register AgentNodePool controller")
		os.Exit(1)
	}

	// runtime.agents.smol-agents.ai/v1 — agent-model CRDs.
	if err := (&agentmodel.AgentReconciler{
		Client: mgr.GetClient(), Scheme: mgr.GetScheme(),
		AllowedStdioMCP: parseAllowList(allowedStdioMCP),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to register Agent controller")
		os.Exit(1)
	}
	if err := (&agentmodel.AgentRunReconciler{
		Client: mgr.GetClient(), Scheme: mgr.GetScheme(),
		DefaultRunRuntimeClass:         defaultRunRuntimeClass,
		AllowHostRuntime:               allowHostRuntime,
		MaxConcurrentReconciles:        maxConcurrentReconciles,
		RunDeadlineMultiplier:          runDeadlineMultiplier,
		DefaultApprovalTimeout:         defaultApprovalTimeout,
		DefaultNamespaceRunConcurrency: int32(defaultNamespaceRunConcurrency),
		EnableAdmissionQueue:           enableAdmissionQueue,
		MaxPriority:                    int32(maxRunPriority),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to register AgentRun controller")
		os.Exit(1)
	}
	if err := (&agentmodel.AgentSessionReconciler{
		Client: mgr.GetClient(), Scheme: mgr.GetScheme(),
		DefaultRunRuntimeClass:  defaultRunRuntimeClass,
		AllowHostRuntime:        allowHostRuntime,
		NATSURL:                 sessionNATSURL,
		NATSAccountSeed:         natsAccountSeed,
		MaxConcurrentReconciles: maxConcurrentReconciles,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to register AgentSession controller")
		os.Exit(1)
	}
	if err := (&agentmodel.AgentNetworkReconciler{
		Client: mgr.GetClient(), Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to register AgentNetwork controller")
		os.Exit(1)
	}
	if err := (&agentmodel.DynamicCredentialBackendReconciler{
		Client: mgr.GetClient(), Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to register DynamicCredentialBackend controller")
		os.Exit(1)
	}
	if err := (&agentmodel.AgentTeamReconciler{
		Client: mgr.GetClient(), Scheme: mgr.GetScheme(),
		MaxConcurrentReconciles: maxConcurrentReconciles,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to register AgentTeam controller")
		os.Exit(1)
	}
	if err := (&agentmodel.AgentWorkflowReconciler{
		Client: mgr.GetClient(), Scheme: mgr.GetScheme(),
		MaxConcurrentReconciles: maxConcurrentReconciles,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to register AgentWorkflow controller")
		os.Exit(1)
	}

	// runtime.agents.smol-agents.ai/v1 — memory CRDs.
	if err := (&memoryctrl.MemoryRetrieverReconciler{
		Client: mgr.GetClient(), Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to register MemoryRetriever controller")
		os.Exit(1)
	}

	// Admission webhooks. R-OP-WH-1, R-OP-WH-2, R-AN-API-1.
	if os.Getenv("ENABLE_WEBHOOKS") != "false" {
		if err := webhooks.SetupAgentWebhook(mgr, "default"); err != nil {
			setupLog.Error(err, "unable to register SmolAgent webhook")
			os.Exit(1)
		}
		if err := webhooks.SetupPlatformWebhook(mgr, "default"); err != nil {
			setupLog.Error(err, "unable to register SmolAgentPlatform webhook")
			os.Exit(1)
		}
		if err := webhooks.SetupAgentNetworkWebhook(mgr); err != nil {
			setupLog.Error(err, "unable to register AgentNetwork webhook")
			os.Exit(1)
		}
		if err := webhooks.SetupAgentPolicyGateWebhook(mgr); err != nil {
			setupLog.Error(err, "unable to register AgentPolicy gate webhook")
			os.Exit(1)
		}
		if err := webhooks.SetupAgentPolicySelfWebhook(mgr); err != nil {
			setupLog.Error(err, "unable to register AgentPolicy self-validation webhook")
			os.Exit(1)
		}
		if err := webhooks.SetupAgentSessionWebhook(mgr); err != nil {
			setupLog.Error(err, "unable to register AgentSession webhook")
			os.Exit(1)
		}
		if err := webhooks.SetupAgentTeamWebhook(mgr); err != nil {
			setupLog.Error(err, "unable to register AgentTeam webhook")
			os.Exit(1)
		}
		if err := webhooks.SetupToolWebhook(mgr); err != nil {
			setupLog.Error(err, "unable to register Tool webhook")
			os.Exit(1)
		}
		if err := webhooks.SetupAgentWorkflowWebhook(mgr); err != nil {
			setupLog.Error(err, "unable to register AgentWorkflow webhook")
			os.Exit(1)
		}
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "manager exit")
		os.Exit(1)
	}
}

// parseAllowList turns a comma-separated flag value into a set, trimming blanks.
func parseAllowList(csv string) map[string]bool {
	out := map[string]bool{}
	for _, e := range strings.Split(csv, ",") {
		if e = strings.TrimSpace(e); e != "" {
			out[e] = true
		}
	}
	return out
}
