// Package hetzner implements `agentctl deploy --target=hetzner`: provision a
// Hetzner Cloud server + firewall + ssh key, bootstrap k0s with cloud-init,
// then delegate to the k8s target to install the operator stack against the
// fetched kubeconfig.
//
// Resources are labelled `agentctl-deployment=<name>` for label-based
// idempotency: re-running deploy reuses an existing server/firewall/key with
// the same label; teardown finds and removes them.
package hetzner

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"time"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"
	"golang.org/x/crypto/ssh"

	"github.com/smol-platform/smol-agents/internal/agentctl/deploy"
	"github.com/smol-platform/smol-agents/internal/agentctl/deploy/cloud"
	"github.com/smol-platform/smol-agents/internal/agentctl/deploy/k8s"
)

// labelKey is the discriminator used on every resource we create.
const labelKey = "agentctl-deployment"

type Target struct{}

func New() *Target           { return &Target{} }
func (*Target) Name() string { return "hetzner" }

func (*Target) Validate(o *deploy.Options) error {
	if o.Hetzner.TokenEnv == "" {
		o.Hetzner.TokenEnv = "HCLOUD_TOKEN"
	}
	if os.Getenv(o.Hetzner.TokenEnv) == "" {
		return fmt.Errorf("env var %s is not set (Hetzner Cloud API token)", o.Hetzner.TokenEnv)
	}
	if o.Hetzner.Location == "" {
		return fmt.Errorf("--hcloud-location is required (e.g., nbg1, hel1, ash)")
	}
	if o.Hetzner.ServerType == "" {
		return fmt.Errorf("--server-type is required (e.g., cax21 for arm64, cx22 for amd64)")
	}
	if o.Hetzner.SSHKey == "" {
		return fmt.Errorf("--ssh-key is required (local path to private key; public key is derived)")
	}
	if _, err := os.Stat(o.Hetzner.SSHKey); err != nil {
		return fmt.Errorf("--ssh-key %s: %w", o.Hetzner.SSHKey, err)
	}
	if o.Hetzner.Image == "" {
		o.Hetzner.Image = "ubuntu-24.04"
	}
	return nil
}

// Deploy provisions (or reuses) the server, bootstraps k0s, fetches the
// kubeconfig, and runs the shared platform install.
func (*Target) Deploy(ctx context.Context, o *deploy.Options) error {
	out := o.Common.Out
	fmt.Fprintf(out, "==> target=hetzner location=%s type=%s image=%s\n",
		o.Hetzner.Location, o.Hetzner.ServerType, o.Hetzner.Image)

	client := hcloud.NewClient(hcloud.WithToken(os.Getenv(o.Hetzner.TokenEnv)))

	// 0. Networking plan: decides whether the firewall opens 6443 and what URL
	// the rewritten kubeconfig will point at after bootstrap.
	plan, err := cloud.PlanAPIEndpoint(cloud.APIEndpointInput{
		CloudflareTunnelToken: o.Common.CloudflareTunnelToken,
		APIHostname:           o.Common.APIHostname,
		TailscaleAuthKey:      o.Common.TailscaleAuthKey,
		WireGuardConfig:       o.Common.WireGuardConfig,
	})
	if err != nil {
		return fmt.Errorf("networking plan: %w", err)
	}
	fmt.Fprintf(out, "==> Networking plan: mode=%s expose-api=%v\n", plan.Mode, plan.ExposeAPIPublicly)

	// 1. Ensure the SSH key (find by label or upload).
	pubKey, err := derivePublicKey(o.Hetzner.SSHKey)
	if err != nil {
		return fmt.Errorf("derive public key: %w", err)
	}
	sshKey, err := ensureSSHKey(ctx, client, out, o.Common.Name, pubKey)
	if err != nil {
		return fmt.Errorf("ensure ssh key: %w", err)
	}

	// 2. Ensure the firewall (SSH always; k8s API only when public).
	fw, err := ensureFirewall(ctx, client, out, o.Common.Name, plan.ExposeAPIPublicly)
	if err != nil {
		return fmt.Errorf("ensure firewall: %w", err)
	}

	// 3. Ensure the server (find by label or create).
	srv, reused, err := ensureServer(ctx, client, out, ensureServerInput{
		Name:       o.Common.Name,
		ServerType: o.Hetzner.ServerType,
		Image:      o.Hetzner.Image,
		Location:   o.Hetzner.Location,
		SSHKey:     sshKey,
		Firewall:   fw,
		CloudInit: cloud.CloudInitOptions{
			Hostname:              "agentctl-" + o.Common.Name,
			CloudflareTunnelToken: o.Common.CloudflareTunnelToken,
			TailscaleAuthKey:      o.Common.TailscaleAuthKey,
			TailscaleTags:         o.Common.TailscaleTags,
			WireGuardConfig:       plan.WGContent,
		},
	})
	if err != nil {
		return fmt.Errorf("ensure server: %w", err)
	}
	if reused {
		fmt.Fprintf(out, "==> Reusing existing server %d (label %s=%s)\n", srv.ID, labelKey, o.Common.Name)
	}

	if o.Common.DryRun {
		fmt.Fprintf(out, "==> dry-run: provisioning skipped past this point\n")
		return nil
	}

	publicIP := srv.PublicNet.IPv4.IP.String()
	fmt.Fprintf(out, "==> Server %d @ %s — waiting for k0s bootstrap\n", srv.ID, publicIP)

	// 4. SSH-wait for the sentinel.
	sshCfg, err := cloud.SSHConfig("root", o.Hetzner.SSHKey)
	if err != nil {
		return fmt.Errorf("ssh config: %w", err)
	}
	wctx, cancel := context.WithTimeout(ctx, 8*time.Minute)
	defer cancel()
	if err := cloud.WaitForSentinel(wctx, net.JoinHostPort(publicIP, "22"), sshCfg, cloud.DefaultSentinelPath, 5*time.Second); err != nil {
		return fmt.Errorf("wait sentinel: %w", err)
	}
	fmt.Fprintf(out, "==> k0s bootstrap complete\n")

	// 5. Stage the kubeconfig. Server URL + auth depend on the plan: CF tunnel
	// hostname uses token auth (client certs don't survive CF's TLS
	// termination); WG/tailnet/public use the cert-based admin.conf.
	sshAddr := net.JoinHostPort(publicIP, "22")
	// For tailscale the API address is the tailnet IP assigned at join time;
	// read it back over the same SSH hop. Every other mode uses publicIP (or
	// ignores it — CF/WG carry their own address in the plan).
	endpointAddr := publicIP
	if plan.Mode == "tailscale" {
		tsIP, err := cloud.FetchTailscaleIPv4(ctx, sshAddr, sshCfg)
		if err != nil {
			return fmt.Errorf("resolve tailscale ip: %w", err)
		}
		fmt.Fprintf(out, "==> Tailscale address %s\n", tsIP)
		endpointAddr = tsIP
	}
	kcPath, server, apiTimeout, err := cloud.StageKubeconfig(ctx, cloud.StageKubeconfigInput{
		SSHAddr: sshAddr, SSHCfg: sshCfg, Plan: plan, EndpointAddr: endpointAddr, Name: o.Common.Name,
	})
	if err != nil {
		return fmt.Errorf("stage kubeconfig: %w", err)
	}
	defer func() { _ = os.RemoveAll(kcPath) }()
	fmt.Fprintf(out, "==> Kubeconfig staged at %s (server=%s)\n", kcPath, server)

	// 6. Wait for the API at the chosen URL (CF tunnel cold-start propagation).
	fmt.Fprintf(out, "==> Waiting for API at %s (timeout %s)\n", server, apiTimeout)
	if err := cloud.WaitForAPI(ctx, kcPath, apiTimeout); err != nil {
		return fmt.Errorf("api not reachable: %w", err)
	}

	// 7. Delegate to the k8s target.
	k8sOpts := *o
	k8sOpts.K8s.Kubeconfig = kcPath
	k8sOpts.K8s.Context = ""
	if err := k8s.New().Deploy(ctx, &k8sOpts); err != nil {
		return fmt.Errorf("platform install on server %d: %w", srv.ID, err)
	}

	fmt.Fprintf(out, "==> hetzner deploy complete — server=%d ip=%s mode=%s\n", srv.ID, publicIP, plan.Mode)
	fmt.Fprintf(out, "    SSH:        ssh -i %s root@%s\n", o.Hetzner.SSHKey, publicIP)
	fmt.Fprintf(out, "    API:        %s\n", server)
	fmt.Fprintf(out, "    kubeconfig: keep %s while you use the cluster; agentctl will remove it on exit\n", kcPath)
	return nil
}

func (*Target) Teardown(ctx context.Context, o *deploy.Options) error {
	out := o.Common.Out
	client := hcloud.NewClient(hcloud.WithToken(os.Getenv(o.Hetzner.TokenEnv)))

	selector := labelKey + "=" + o.Common.Name

	// Servers first (servers reference firewall + sshKey; deleting them first
	// avoids the firewall/key delete being held by an in-use reference).
	servers, err := client.Server.AllWithOpts(ctx, hcloud.ServerListOpts{ListOpts: hcloud.ListOpts{LabelSelector: selector}})
	if err != nil {
		return fmt.Errorf("list servers: %w", err)
	}
	for _, s := range servers {
		fmt.Fprintf(out, "==> Deleting server %d (%s)\n", s.ID, s.Name)
		if _, _, err := client.Server.DeleteWithResult(ctx, s); err != nil {
			return fmt.Errorf("delete server %d: %w", s.ID, err)
		}
	}

	fws, err := client.Firewall.AllWithOpts(ctx, hcloud.FirewallListOpts{ListOpts: hcloud.ListOpts{LabelSelector: selector}})
	if err != nil {
		return fmt.Errorf("list firewalls: %w", err)
	}
	for _, f := range fws {
		fmt.Fprintf(out, "==> Deleting firewall %d (%s)\n", f.ID, f.Name)
		if _, err := client.Firewall.Delete(ctx, f); err != nil {
			fmt.Fprintf(out, "    delete firewall: %v\n", err)
		}
	}

	keys, err := client.SSHKey.AllWithOpts(ctx, hcloud.SSHKeyListOpts{ListOpts: hcloud.ListOpts{LabelSelector: selector}})
	if err != nil {
		return fmt.Errorf("list ssh keys: %w", err)
	}
	for _, k := range keys {
		fmt.Fprintf(out, "==> Deleting ssh key %d (%s)\n", k.ID, k.Name)
		if _, err := client.SSHKey.Delete(ctx, k); err != nil {
			fmt.Fprintf(out, "    delete ssh key: %v\n", err)
		}
	}

	fmt.Fprintf(out, "==> hetzner teardown complete\n")
	return nil
}

func ensureSSHKey(ctx context.Context, c *hcloud.Client, out io.Writer, name, pub string) (*hcloud.SSHKey, error) {
	keyName := "agentctl-" + name
	keys, err := c.SSHKey.AllWithOpts(ctx, hcloud.SSHKeyListOpts{ListOpts: hcloud.ListOpts{LabelSelector: labelKey + "=" + name}})
	if err != nil {
		return nil, err
	}
	if len(keys) > 0 {
		return keys[0], nil
	}
	created, _, err := c.SSHKey.Create(ctx, hcloud.SSHKeyCreateOpts{
		Name:      keyName,
		PublicKey: pub,
		Labels:    map[string]string{labelKey: name},
	})
	if err != nil {
		return nil, fmt.Errorf("create ssh key: %w", err)
	}
	fmt.Fprintf(out, "==> Created ssh key %d (%s)\n", created.ID, keyName)
	return created, nil
}

func ensureFirewall(ctx context.Context, c *hcloud.Client, out io.Writer, name string, exposeAPI bool) (*hcloud.Firewall, error) {
	fws, err := c.Firewall.AllWithOpts(ctx, hcloud.FirewallListOpts{ListOpts: hcloud.ListOpts{LabelSelector: labelKey + "=" + name}})
	if err != nil {
		return nil, err
	}
	if len(fws) > 0 {
		return fws[0], nil
	}
	fwName := "agentctl-" + name
	rules := []hcloud.FirewallRule{fwIngressTCP("22", "ssh")}
	if exposeAPI {
		rules = append(rules, fwIngressTCP("6443", "k8s API"))
	}
	res, _, err := c.Firewall.Create(ctx, hcloud.FirewallCreateOpts{
		Name:   fwName,
		Labels: map[string]string{labelKey: name},
		Rules:  rules,
	})
	if err != nil {
		return nil, fmt.Errorf("create firewall: %w", err)
	}
	if exposeAPI {
		fmt.Fprintf(out, "==> Created firewall %d (%s) — ingress: tcp/22 + tcp/6443 from 0.0.0.0/0,::/0\n", res.Firewall.ID, fwName)
	} else {
		fmt.Fprintf(out, "==> Created firewall %d (%s) — ingress: tcp/22 only (k8s API via tunnel/mesh)\n", res.Firewall.ID, fwName)
	}
	return res.Firewall, nil
}

func fwIngressTCP(port, desc string) hcloud.FirewallRule {
	v4 := &net.IPNet{IP: net.IPv4(0, 0, 0, 0), Mask: net.CIDRMask(0, 32)}
	v6 := &net.IPNet{IP: net.IPv6zero, Mask: net.CIDRMask(0, 128)}
	return hcloud.FirewallRule{
		Direction:   hcloud.FirewallRuleDirectionIn,
		Protocol:    hcloud.FirewallRuleProtocolTCP,
		Port:        &port,
		SourceIPs:   []net.IPNet{*v4, *v6},
		Description: &desc,
	}
}

type ensureServerInput struct {
	Name, ServerType, Image, Location string
	SSHKey                            *hcloud.SSHKey
	Firewall                          *hcloud.Firewall
	CloudInit                         cloud.CloudInitOptions
}

func ensureServer(ctx context.Context, c *hcloud.Client, out io.Writer, in ensureServerInput) (*hcloud.Server, bool, error) {
	servers, err := c.Server.AllWithOpts(ctx, hcloud.ServerListOpts{ListOpts: hcloud.ListOpts{LabelSelector: labelKey + "=" + in.Name}})
	if err != nil {
		return nil, false, err
	}
	for _, s := range servers {
		if s.Status == hcloud.ServerStatusRunning || s.Status == hcloud.ServerStatusInitializing || s.Status == hcloud.ServerStatusStarting {
			return s, true, nil
		}
	}

	userData, err := cloud.CloudInitScript(in.CloudInit)
	if err != nil {
		return nil, false, err
	}

	fmt.Fprintf(out, "==> Creating server (%s, %s, %s)\n", in.ServerType, in.Image, in.Location)
	res, _, err := c.Server.Create(ctx, hcloud.ServerCreateOpts{
		Name:       "agentctl-" + in.Name,
		ServerType: &hcloud.ServerType{Name: in.ServerType},
		Image:      &hcloud.Image{Name: in.Image},
		Location:   &hcloud.Location{Name: in.Location},
		SSHKeys:    []*hcloud.SSHKey{in.SSHKey},
		Firewalls:  []*hcloud.ServerCreateFirewall{{Firewall: hcloud.Firewall{ID: in.Firewall.ID}}},
		UserData:   userData,
		Labels:     map[string]string{labelKey: in.Name},
	})
	if err != nil {
		return nil, false, fmt.Errorf("server create: %w", err)
	}
	// Server.Create returns immediately; the API call is queued. Wait for
	// the create action to finish so PublicNet.IPv4 is populated.
	if err := waitAction(ctx, c, res.Action, 3*time.Minute); err != nil {
		return nil, false, fmt.Errorf("wait server create: %w", err)
	}
	srv, _, err := c.Server.GetByID(ctx, res.Server.ID)
	if err != nil {
		return nil, false, err
	}
	return srv, false, nil
}

func waitAction(ctx context.Context, c *hcloud.Client, a *hcloud.Action, deadline time.Duration) error {
	if a == nil {
		return nil
	}
	dctx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	for {
		got, _, err := c.Action.GetByID(dctx, a.ID)
		if err != nil {
			return err
		}
		switch got.Status {
		case hcloud.ActionStatusSuccess:
			return nil
		case hcloud.ActionStatusError:
			return fmt.Errorf("action %d failed: %s", got.ID, got.ErrorMessage)
		}
		select {
		case <-dctx.Done():
			return dctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}

// derivePublicKey reads the private key at path and returns the OpenSSH-format
// public key string. Avoids requiring the user to keep <path>.pub alongside.
func derivePublicKey(privateKeyPath string) (string, error) {
	raw, err := os.ReadFile(privateKeyPath)
	if err != nil {
		return "", err
	}
	signer, err := ssh.ParsePrivateKey(raw)
	if err != nil {
		return "", fmt.Errorf("parse private key: %w", err)
	}
	return string(ssh.MarshalAuthorizedKey(signer.PublicKey())), nil
}
