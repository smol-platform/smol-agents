// Command manager is the operator entrypoint.
package main

import (
	"flag"
	"os"

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
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8443", "metrics endpoint")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "health/readiness probe address")
	flag.BoolVar(&enableLeaderElection, "leader-elect", true, "enable leader election")
	flag.StringVar(&defaultRunRuntimeClass, "default-run-runtime-class", "kata-fc",
		"sandbox RuntimeClass applied to AgentRun pods when the Agent doesn't override it (empty = kata-fc)")
	flag.BoolVar(&allowHostRuntime, "allow-host-runtime", false,
		"permit runc (shared host kernel) for AgentRun pods on clusters with no sandbox runtime; otherwise runc is a fail-closed R-SBX-1 violation")
	opts := zap.Options{Development: false}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

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
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to register Agent controller")
		os.Exit(1)
	}
	if err := (&agentmodel.AgentRunReconciler{
		Client: mgr.GetClient(), Scheme: mgr.GetScheme(),
		DefaultRunRuntimeClass: defaultRunRuntimeClass,
		AllowHostRuntime:       allowHostRuntime,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to register AgentRun controller")
		os.Exit(1)
	}
	if err := (&agentmodel.AgentSessionReconciler{
		Client: mgr.GetClient(), Scheme: mgr.GetScheme(),
		DefaultRunRuntimeClass: defaultRunRuntimeClass,
		AllowHostRuntime:       allowHostRuntime,
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
