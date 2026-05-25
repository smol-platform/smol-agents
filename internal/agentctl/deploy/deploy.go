// Package deploy implements the multi-target installer behind `agentctl deploy`.
//
// One Options union, one Target per environment (k8s / aws / hetzner /
// baremetal). The shared package owns the option types and the dispatcher;
// each target subpackage owns its provisioning + platform-install logic.
package deploy

import (
	"context"
	"fmt"
	"io"
	"os"
)

// Target is the contract every deploy backend satisfies. Implementations live
// in subpackages (k8s, aws, hetzner, baremetal) so their dependency trees stay
// scoped — pulling in the AWS SDK shouldn't bloat the k8s path, etc.
type Target interface {
	// Name is the value of --target this implementation answers to.
	Name() string

	// Validate inspects Options and returns a clear error before any side
	// effects run. The dispatcher invokes Validate before Deploy/Teardown.
	Validate(*Options) error

	// Deploy provisions (if applicable) and installs the platform.
	Deploy(context.Context, *Options) error

	// Teardown idempotently removes whatever Deploy created.
	Teardown(context.Context, *Options) error
}

// CommonOptions are flags every target accepts. The networking fields below
// apply to provisioning targets (aws, hetzner, baremetal) — k8s target ignores
// them.
type CommonOptions struct {
	Name         string    // logical release name (default: smol-agents)
	TrustDomain  string    // SPIFFE trust domain (default: smol-agents.ai)
	OperatorImg  string    // operator image override (optional)
	ManifestsDir string    // kustomize root; empty -> walk-up repo discovery
	WithWebhooks bool      // install operator with admission webhooks (needs cert-manager)
	Sample       string    // also apply this sample CR (e.g. "minimal", "full")
	Teardown     bool      // remove the deployment instead of installing
	DryRun       bool      // render + validate, no cluster changes
	Out          io.Writer // progress sink (stderr)

	// CloudflareTunnelToken, when set, installs cloudflared on the
	// provisioned host pointing at https://127.0.0.1:6443. The user is
	// responsible for configuring the tunnel + ingress hostname in
	// Cloudflare beforehand and passing the matching token here.
	// Mutually compatible with WireGuardConfig (a node can hold both).
	CloudflareTunnelToken string

	// APIHostname is the public hostname configured for the Cloudflare
	// tunnel (e.g. "k8s.example.com"). Required iff CloudflareTunnelToken
	// is set: the deploy rewrites the kubeconfig to this hostname.
	APIHostname string

	// WireGuardConfig is the path to a wg-quick config file (wg0.conf) on
	// the local machine. The deploy uploads its contents via cloud-init,
	// installs wireguard-tools, brings up wg0, and rewrites the
	// kubeconfig to the [Interface] Address.
	WireGuardConfig string

	// TailscaleAuthKey, when set, installs Tailscale on the provisioned host
	// and runs `tailscale up` with this key. The deploy reads the assigned
	// tailnet IP back over SSH and points the kubeconfig at it. Prefer an
	// ephemeral key so teardown de-registers the node. Secret: never logged.
	TailscaleAuthKey string

	// TailscaleTags is an optional comma-separated set of ACL tags advertised
	// via `tailscale up --advertise-tags` (e.g. "tag:k8s"). Required when the
	// auth key is tagged; ignored without TailscaleAuthKey.
	TailscaleTags string
}

// K8sOptions are the --target=k8s flags.
type K8sOptions struct {
	Kubeconfig     string // empty -> $KUBECONFIG / ~/.kube/config
	Context        string // empty -> current-context
	InstallSpire   bool   // gated; not implemented in V1
	InstallCertMgr bool   // gated; not implemented in V1
}

// AWSOptions are the --target=aws flags. Drives EC2 + SG + k0s edge.
type AWSOptions struct {
	Profile      string
	Region       string
	InstanceType string
	SubnetID     string // empty -> first subnet of the default VPC
	KeyName      string // EC2 key pair name (must already exist in AWS)
	SSHKey       string // local path to the matching private key
	AMI          string // empty -> Amazon Linux 2023 via SSM lookup
}

// HetznerOptions are the --target=hetzner flags.
type HetznerOptions struct {
	TokenEnv   string // env var holding hcloud token (default: HCLOUD_TOKEN)
	Location   string // e.g. nbg1, hel1
	ServerType string // e.g. cax21
	Image      string // default: ubuntu-24.04
	SSHKey     string // local path to private key; public is read from <path>.pub
}

// BareMetalOptions are the --target=baremetal flags. SSH to a pre-provisioned
// box and bootstrap k0s.
type BareMetalOptions struct {
	Host    string // host[:port]
	User    string // SSH user (default: root)
	KeyPath string // SSH private key
}

// Options is the union passed into Run. Each target reads only its own slice.
type Options struct {
	Common    CommonOptions
	K8s       K8sOptions
	AWS       AWSOptions
	Hetzner   HetznerOptions
	BareMetal BareMetalOptions
}

// Run validates and dispatches to the target's Deploy or Teardown.
func Run(ctx context.Context, target Target, o *Options) error {
	if target == nil {
		return fmt.Errorf("deploy: nil target")
	}
	if o == nil || o.Common.Out == nil {
		return fmt.Errorf("deploy: Options.Common.Out is required")
	}
	if err := validateCommon(&o.Common); err != nil {
		return fmt.Errorf("deploy(%s): %w", target.Name(), err)
	}
	if err := target.Validate(o); err != nil {
		return fmt.Errorf("deploy(%s): %w", target.Name(), err)
	}
	if o.Common.Teardown {
		return target.Teardown(ctx, o)
	}
	return target.Deploy(ctx, o)
}

// validateCommon enforces the cross-target invariants on networking flags:
// CloudflareTunnelToken and APIHostname must be set together; WireGuardConfig
// (if set) must point at a readable file.
func validateCommon(c *CommonOptions) error {
	if (c.CloudflareTunnelToken == "") != (c.APIHostname == "") {
		return fmt.Errorf("--cloudflare-tunnel-token and --api-hostname must be set together")
	}
	if c.WireGuardConfig != "" {
		if _, err := os.Stat(c.WireGuardConfig); err != nil {
			return fmt.Errorf("--wireguard-config %s: %w", c.WireGuardConfig, err)
		}
	}
	if c.TailscaleTags != "" && c.TailscaleAuthKey == "" {
		return fmt.Errorf("--tailscale-tags requires --tailscale-auth-key")
	}
	return nil
}
