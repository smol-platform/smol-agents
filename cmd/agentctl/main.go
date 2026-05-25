// Command agentctl is a small CLI that talks to a local agent / broker
// for diagnostics.
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"text/tabwriter"
	"time"

	"github.com/smol-platform/smol-agents/internal/agentctl/deploy"
	"github.com/smol-platform/smol-agents/internal/agentctl/deploy/aws"
	"github.com/smol-platform/smol-agents/internal/agentctl/deploy/baremetal"
	"github.com/smol-platform/smol-agents/internal/agentctl/deploy/hetzner"
	"github.com/smol-platform/smol-agents/internal/agentctl/deploy/k8s"
	"github.com/smol-platform/smol-agents/internal/version"
	"github.com/smol-platform/smol-agents/pkg/secrets"
)

func main() {
	flag.Usage = usage
	flag.Parse()

	args := flag.Args()
	if len(args) == 0 {
		usage()
		os.Exit(2)
	}
	switch args[0] {
	case "version":
		fmt.Println(version.String())
	case "status":
		os.Exit(cmdStatus(args[1:]))
	case "lease":
		os.Exit(cmdLease(args[1:]))
	case "deploy":
		os.Exit(cmdDeploy(args[1:]))
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "Usage: agentctl <command> [args]")
	fmt.Fprintln(os.Stderr, "Commands:")
	fmt.Fprintln(os.Stderr, "  status [-addr http://127.0.0.1:8080]   ping /readyz and /healthz")
	fmt.Fprintln(os.Stderr, "  lease  [-socket path] -name NAME       request a lease from local broker")
	fmt.Fprintln(os.Stderr, "  deploy -target <k8s|aws|hetzner|baremetal> [flags]")
	fmt.Fprintln(os.Stderr, "                                         install the operator stack on a target")
	fmt.Fprintln(os.Stderr, "  version                                print version")
}

func cmdStatus(args []string) int {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	addr := fs.String("addr", "http://127.0.0.1:8080", "agent health endpoint")
	_ = fs.Parse(args)

	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	defer w.Flush()
	fmt.Fprintln(w, "ENDPOINT\tSTATUS\tLATENCY")
	for _, p := range []string{"/healthz", "/readyz"} {
		t := time.Now()
		resp, err := http.Get(*addr + p)
		dur := time.Since(t).Truncate(time.Microsecond)
		if err != nil {
			fmt.Fprintf(w, "%s\tERROR: %v\t%s\n", p, err, dur)
			continue
		}
		_ = resp.Body.Close()
		fmt.Fprintf(w, "%s\t%d\t%s\n", p, resp.StatusCode, dur)
	}
	return 0
}

func cmdLease(args []string) int {
	fs := flag.NewFlagSet("lease", flag.ExitOnError)
	socket := fs.String("socket", "/run/secret-broker/secret-broker.sock", "broker UDS")
	name := fs.String("name", "", "secret name (required)")
	ttl := fs.Duration("ttl", 0, "requested TTL (0 = broker default)")
	_ = fs.Parse(args)

	if *name == "" {
		fmt.Fprintln(os.Stderr, "lease: -name is required")
		return 2
	}
	c := secrets.NewClient(*socket)
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	l, err := c.Lease(ctx, *name, *ttl)
	if err != nil {
		fmt.Fprintf(os.Stderr, "lease error: %v\n", err)
		return 1
	}
	// Never print the value to stdout — leaks may be captured by shell history.
	fmt.Printf("name=%s audience=%s issued=%s expires=%s ttl=%s bytes=%d\n",
		l.Name, l.Audience, l.Issued.Format(time.RFC3339), l.ExpiresAt.Format(time.RFC3339),
		l.TTL, len(l.Value))
	return 0
}

func cmdDeploy(args []string) int {
	fs := flag.NewFlagSet("deploy", flag.ExitOnError)
	target := fs.String("target", "", "deploy target: k8s | aws | hetzner | baremetal")

	// Common flags
	name := fs.String("name", "smol-agents", "logical release name")
	trustDomain := fs.String("trust-domain", "smol-agents.ai", "SPIFFE trust domain")
	operatorImg := fs.String("operator-image", "", "operator image override (default: chart pin)")
	manifestsDir := fs.String("manifests-dir", "", "kustomize source root (default: walk up to repo)")
	withWebhooks := fs.Bool("with-webhooks", false, "install operator with admission webhooks (requires cert-manager)")
	sample := fs.String("sample", "", "also apply a sample CR: minimal | full | claude-code | codex | pi")
	teardown := fs.Bool("teardown", false, "remove the deployment instead of installing")
	dryRun := fs.Bool("dry-run", false, "render + validate, no cluster changes")

	// target=k8s
	kubeconfig := fs.String("kubeconfig", "", "kubeconfig path (default: $KUBECONFIG or ~/.kube/config)")
	kctx := fs.String("context", "", "kubeconfig context (default: current-context)")
	installSpire := fs.Bool("install-spire", false, "also install SPIRE (not yet implemented)")
	installCertMgr := fs.Bool("install-cert-manager", false, "also install cert-manager (not yet implemented)")

	// target=aws
	awsProfile := fs.String("aws-profile", os.Getenv("AWS_PROFILE"), "AWS profile (or $AWS_PROFILE)")
	awsRegion := fs.String("aws-region", os.Getenv("AWS_REGION"), "AWS region (or $AWS_REGION)")
	instanceType := fs.String("instance-type", "", "EC2 instance type (e.g., t3.small for amd64, t4g.small for arm64 + --ami)")
	subnetID := fs.String("subnet-id", "", "existing subnet id (empty -> first subnet of the default VPC)")
	keyName := fs.String("aws-key-name", "", "EC2 key pair name registered in your AWS account")
	ami := fs.String("ami", "", "EC2 AMI id (empty -> latest AL2023 amd64 via SSM)")

	// target=hetzner
	hcloudTokenEnv := fs.String("hcloud-token-env", "HCLOUD_TOKEN", "env var holding the Hetzner Cloud API token")
	hcloudLocation := fs.String("hcloud-location", "", "Hetzner location (e.g., nbg1, hel1, ash)")
	serverType := fs.String("server-type", "", "Hetzner server type (e.g., cax21, cx22)")
	hcloudImage := fs.String("hcloud-image", "ubuntu-24.04", "Hetzner image name")

	// shared: provisioning targets all SSH-fetch the kubeconfig from the host.
	sshKey := fs.String("ssh-key", "", "SSH private key path (used by aws, hetzner, baremetal)")

	// target=baremetal
	sshHost := fs.String("ssh-host", "", "SSH host[:port]")
	sshUser := fs.String("ssh-user", "root", "SSH user (baremetal only)")

	_ = fs.Parse(args)

	if *target == "" {
		fmt.Fprintln(os.Stderr, "deploy: -target is required (k8s | aws | hetzner | baremetal)")
		fs.Usage()
		return 2
	}

	var t deploy.Target
	switch *target {
	case "k8s":
		t = k8s.New()
	case "aws":
		t = aws.New()
	case "hetzner":
		t = hetzner.New()
	case "baremetal":
		t = baremetal.New()
	default:
		fmt.Fprintf(os.Stderr, "deploy: unknown target %q\n", *target)
		return 2
	}

	opts := &deploy.Options{
		Common: deploy.CommonOptions{
			Name:         *name,
			TrustDomain:  *trustDomain,
			OperatorImg:  *operatorImg,
			ManifestsDir: *manifestsDir,
			WithWebhooks: *withWebhooks,
			Sample:       *sample,
			Teardown:     *teardown,
			DryRun:       *dryRun,
			Out:          os.Stderr,
		},
		K8s: deploy.K8sOptions{
			Kubeconfig: *kubeconfig, Context: *kctx,
			InstallSpire: *installSpire, InstallCertMgr: *installCertMgr,
		},
		AWS: deploy.AWSOptions{
			Profile: *awsProfile, Region: *awsRegion, InstanceType: *instanceType,
			SubnetID: *subnetID, KeyName: *keyName, SSHKey: *sshKey, AMI: *ami,
		},
		Hetzner: deploy.HetznerOptions{
			TokenEnv: *hcloudTokenEnv, Location: *hcloudLocation, ServerType: *serverType,
			Image: *hcloudImage, SSHKey: *sshKey,
		},
		BareMetal: deploy.BareMetalOptions{
			Host: *sshHost, User: *sshUser, KeyPath: *sshKey,
		},
	}

	// Honor SIGINT / SIGTERM so a long deploy cancels its in-flight cluster work.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := deploy.Run(ctx, t, opts); err != nil {
		fmt.Fprintf(os.Stderr, "deploy error: %v\n", err)
		return 1
	}
	return 0
}
