package k8s

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/smol-platform/smol-agents/internal/agentctl/deploy"
)

// Target installs the smol-agents operator into an existing Kubernetes cluster
// reached via the user's kubeconfig.
type Target struct{}

func New() *Target           { return &Target{} }
func (*Target) Name() string { return "k8s" }

func (*Target) Validate(o *deploy.Options) error {
	if o.K8s.InstallSpire || o.K8s.InstallCertMgr {
		return fmt.Errorf("--install-spire / --install-cert-manager are reserved for a future iteration")
	}
	return nil
}

// Deploy: connect → detect autoscalers → render manifests → apply CRDs +
// wait Established → apply rest + wait operator → apply SmolAgentPlatform
// (+ optional sample).
func (*Target) Deploy(ctx context.Context, o *deploy.Options) error {
	out := o.Common.Out
	c, disc, cfg, err := connect(o)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	fmt.Fprintf(out, "==> Connected to context %q (server %s)\n", contextName(o), cfg.Host)

	fmt.Fprintf(out, "==> Detecting node autoscalers\n")
	ac, err := detectAutoscalers(ctx, disc, c)
	if err != nil {
		return fmt.Errorf("detect: %w", err)
	}
	fmt.Fprintf(out, "    karpenter:           %s\n", boolDetail(ac.Karpenter, ac.KarpenterDetail))
	fmt.Fprintf(out, "    cluster-autoscaler:  %s\n", boolDetail(ac.ClusterAutoscaler, ac.CADetail))
	if !ac.Karpenter && !ac.ClusterAutoscaler {
		fmt.Fprintf(out, "    ⚠ neither autoscaler detected — sandboxed agents (kata-fc) may stay Pending without a\n")
		fmt.Fprintf(out, "      provisioner that knows about *.metal capacity. See docs/runbooks/agent-node-pools.md.\n")
	}

	overlay, err := resolveOverlay(o)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "==> Rendering manifests from %s\n", overlay)
	objs, err := renderOverlay(overlay)
	if err != nil {
		return err
	}
	crds, others := splitCRDs(objs)
	fmt.Fprintf(out, "    %d objects rendered (%d CRDs + %d other)\n", len(objs), len(crds), len(others))

	if n, err := overrideOperatorImage(others, o.Common.OperatorImg); err != nil {
		return fmt.Errorf("override operator image: %w", err)
	} else if n > 0 {
		fmt.Fprintf(out, "    operator image -> %s (%d container(s))\n", o.Common.OperatorImg, n)
	}

	if o.Common.DryRun {
		fmt.Fprintf(out, "==> dry-run: skipping apply\n")
		return nil
	}

	fmt.Fprintf(out, "==> Applying CRDs\n")
	if err := applyAll(ctx, c, out, crds); err != nil {
		return fmt.Errorf("apply CRDs: %w", err)
	}
	fmt.Fprintf(out, "==> Waiting for CRDs to become Established\n")
	if err := waitCRDsEstablished(ctx, c, crds, 60*time.Second); err != nil {
		return fmt.Errorf("wait CRDs Established: %w", err)
	}

	fmt.Fprintf(out, "==> Applying namespace + RBAC + manager\n")
	if err := applyAll(ctx, c, out, others); err != nil {
		return fmt.Errorf("apply manifests: %w", err)
	}

	const opNs, opName = "smol-agents-system", "smol-agents-operator"
	fmt.Fprintf(out, "==> Waiting for operator (%s/%s) to become Available\n", opNs, opName)
	if err := waitDeployment(ctx, c, opNs, opName, 3*time.Minute); err != nil {
		return fmt.Errorf("operator not Ready: %w", err)
	}

	fmt.Fprintf(out, "==> Applying SmolAgentPlatform (so SmolAgents don't stay PlatformAbsent)\n")
	if err := applySampleFile(ctx, c, out, o, "smolagentplatform.yaml"); err != nil {
		return fmt.Errorf("apply platform: %w", err)
	}

	if o.Common.Sample != "" {
		f, ok := sampleFileFor(o.Common.Sample)
		if !ok {
			return fmt.Errorf("unknown --sample %q (one of: minimal, full, claude-code, codex, pi)", o.Common.Sample)
		}
		fmt.Fprintf(out, "==> Applying sample agent (%s)\n", f)
		if err := applySampleFile(ctx, c, out, o, f); err != nil {
			return fmt.Errorf("apply sample: %w", err)
		}
	}

	fmt.Fprintf(out, "==> Deployment complete\n")
	fmt.Fprintf(out, "    next:  kubectl --context=%q get smolagentplatform,smolagent -A\n", contextName(o))
	return nil
}

// Teardown deletes whatever the manifests describe, plus the platform CR (and
// any sample CR if --sample was given on this invocation). CRDs go last so
// dependent CRs vanish first.
func (*Target) Teardown(ctx context.Context, o *deploy.Options) error {
	out := o.Common.Out
	c, _, _, err := connect(o)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	overlay, err := resolveOverlay(o)
	if err != nil {
		return err
	}
	objs, err := renderOverlay(overlay)
	if err != nil {
		return err
	}

	// Also remove the platform CR (and any sample) we may have applied. We
	// best-effort delete via name lookups — if they don't exist, ignore.
	extra := []string{"smolagentplatform.yaml"}
	if o.Common.Sample != "" {
		if f, ok := sampleFileFor(o.Common.Sample); ok {
			extra = append(extra, f)
		}
	}
	for _, f := range extra {
		if u, err := loadSampleFile(o, f); err == nil {
			objs = append(objs, u...)
		}
	}

	// CRDs last so the CRs they own are deletable while the schema still exists.
	sort.SliceStable(objs, func(i, j int) bool {
		return !isCRD(objs[i]) && isCRD(objs[j])
	})

	for _, obj := range objs {
		err := c.Delete(ctx, obj.DeepCopy())
		switch {
		case err == nil:
			fmt.Fprintf(out, "    deleted  %s/%s\n", obj.GetKind(), nameForLog(obj))
		case apierrors.IsNotFound(err):
			// fine — already gone
		default:
			fmt.Fprintf(out, "    delete   %s/%s: %v\n", obj.GetKind(), nameForLog(obj), err)
		}
	}
	fmt.Fprintf(out, "==> Teardown complete\n")
	return nil
}

// resolveOverlay picks operator/config/kind (no webhooks) by default, or
// operator/config/default if --with-webhooks. ManifestsDir overrides repo
// discovery.
func resolveOverlay(o *deploy.Options) (string, error) {
	root := o.Common.ManifestsDir
	if root == "" {
		r, err := findRepoRoot()
		if err != nil {
			return "", fmt.Errorf("--manifests-dir not given and could not find a repo root with operator/config: %w", err)
		}
		root = r
	}
	name := "kind"
	if o.Common.WithWebhooks {
		name = "default"
	}
	p := filepath.Join(root, "operator", "config", name)
	if _, err := os.Stat(p); err != nil {
		return "", fmt.Errorf("overlay %q not found (use --manifests-dir to point at the repo root): %w", p, err)
	}
	return p, nil
}

// findRepoRoot walks up from cwd until it finds operator/config/.
func findRepoRoot() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for dir := cwd; ; {
		if _, err := os.Stat(filepath.Join(dir, "operator", "config")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("operator/config not found walking up from %s", cwd)
		}
		dir = parent
	}
}

// sampleFileFor maps user-friendly --sample values to filenames under
// operator/config/samples/.
func sampleFileFor(name string) (string, bool) {
	switch name {
	case "minimal":
		return "smolagent_minimal.yaml", true
	case "full":
		return "smolagent_full.yaml", true
	case "claude-code":
		return "agent_claude_code.yaml", true
	case "codex":
		return "agent_codex.yaml", true
	case "pi":
		return "agent_pi.yaml", true
	}
	return "", false
}

// loadSampleFile reads one sample CR file and returns its objects.
func loadSampleFile(o *deploy.Options, file string) ([]*unstructured.Unstructured, error) {
	root := o.Common.ManifestsDir
	if root == "" {
		r, err := findRepoRoot()
		if err != nil {
			return nil, err
		}
		root = r
	}
	path := filepath.Join(root, "operator", "config", "samples", file)
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read sample %s: %w", path, err)
	}
	return splitYAML(raw)
}

func applySampleFile(ctx context.Context, c client.Client, out io.Writer, o *deploy.Options, file string) error {
	objs, err := loadSampleFile(o, file)
	if err != nil {
		return err
	}
	for _, obj := range objs {
		if err := applySSA(ctx, c, obj); err != nil {
			return fmt.Errorf("apply %s: %w", obj.GetKind(), err)
		}
		fmt.Fprintf(out, "    %s %s ✓\n", obj.GetKind(), nameForLog(obj))
	}
	return nil
}

// connect builds a controller-runtime client, a discovery client, and the
// underlying *rest.Config from the user's kubeconfig + --context.
func connect(o *deploy.Options) (client.Client, discovery.DiscoveryInterface, *rest.Config, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if o.K8s.Kubeconfig != "" {
		rules.ExplicitPath = o.K8s.Kubeconfig
	}
	cc := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		rules, &clientcmd.ConfigOverrides{CurrentContext: o.K8s.Context},
	)
	cfg, err := cc.ClientConfig()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("load kubeconfig: %w", err)
	}
	sch := scheme.Scheme
	// The default scheme doesn't include apiextensions; the CRD wait needs it.
	_ = apiextv1.AddToScheme(sch)
	cl, err := client.New(cfg, client.Options{Scheme: sch})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("new client: %w", err)
	}
	disc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("new discovery: %w", err)
	}
	return cl, disc, cfg, nil
}

func contextName(o *deploy.Options) string {
	if o.K8s.Context != "" {
		return o.K8s.Context
	}
	return "current"
}

func boolDetail(b bool, detail string) string {
	if b {
		if detail != "" {
			return "found (" + detail + ")"
		}
		return "found"
	}
	return "not found"
}
