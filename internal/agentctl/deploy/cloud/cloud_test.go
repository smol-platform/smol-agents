package cloud

import (
	"os"
	"strings"
	"testing"

	"k8s.io/client-go/tools/clientcmd"
)

func TestCloudInitScript_BareK0s(t *testing.T) {
	got, err := CloudInitScript(CloudInitOptions{Hostname: "agentctl-test"})
	if err != nil {
		t.Fatalf("CloudInitScript: %v", err)
	}
	for _, want := range []string{
		"#!/bin/bash",
		"hostnamectl set-hostname agentctl-test",
		"https://get.k0s.sh",
		"k0s install controller --single",
		"systemctl enable --now k0scontroller",
		"touch " + DefaultSentinelPath,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered script missing %q\n---\n%s", want, got)
		}
	}
	for _, banned := range []string{"WireGuard", "cloudflared", "tailscale"} {
		if strings.Contains(got, banned) {
			t.Errorf("bare script unexpectedly contains %q", banned)
		}
	}
}

func TestCloudInitScript_WithWireGuard(t *testing.T) {
	conf := "[Interface]\nPrivateKey = SECRET\nAddress = 10.0.0.5/24\n[Peer]\nPublicKey = OK\nEndpoint = wg.example.com:51820\n"
	got, err := CloudInitScript(CloudInitOptions{WireGuardConfig: conf})
	if err != nil {
		t.Fatalf("CloudInitScript: %v", err)
	}
	for _, want := range []string{
		"wireguard-tools",                     // package install
		"/etc/wireguard/wg0.conf",             // config path
		"AGENTCTL_WG_EOF",                     // heredoc delimiter
		"Address = 10.0.0.5/24",               // verbatim user content
		"systemctl enable --now wg-quick@wg0", // bring-up
	} {
		if !strings.Contains(got, want) {
			t.Errorf("WG script missing %q\n---\n%s", want, got)
		}
	}
}

func TestCloudInitScript_WithCloudflareTunnel(t *testing.T) {
	got, err := CloudInitScript(CloudInitOptions{CloudflareTunnelToken: "eyJabc.def"})
	if err != nil {
		t.Fatalf("CloudInitScript: %v", err)
	}
	for _, want := range []string{
		"cloudflared-linux-",
		"cloudflared service install 'eyJabc.def'", // shell-quoted token
	} {
		if !strings.Contains(got, want) {
			t.Errorf("CF script missing %q\n---\n%s", want, got)
		}
	}
}

func TestCloudInitScript_TokenIsShellQuoted(t *testing.T) {
	// A pathological token containing a single quote should be safely escaped.
	got, err := CloudInitScript(CloudInitOptions{CloudflareTunnelToken: "a'b"})
	if err != nil {
		t.Fatalf("CloudInitScript: %v", err)
	}
	if !strings.Contains(got, `cloudflared service install 'a'\''b'`) {
		t.Errorf("token not properly shell-quoted; got:\n%s", got)
	}
}

func TestCloudInitScript_WithTailscale(t *testing.T) {
	got, err := CloudInitScript(CloudInitOptions{
		Hostname:         "agentctl-edge",
		TailscaleAuthKey: "tskey-auth-abc123",
		TailscaleTags:    "tag:k8s",
	})
	if err != nil {
		t.Fatalf("CloudInitScript: %v", err)
	}
	for _, want := range []string{
		"https://tailscale.com/install.sh",           // installer
		"systemctl enable --now tailscaled",          // daemon
		"tailscale up --authkey 'tskey-auth-abc123'", // shell-quoted key
		"--hostname agentctl-edge",                   // explicit hostname
		"--advertise-tags 'tag:k8s'",                 // shell-quoted tags
	} {
		if !strings.Contains(got, want) {
			t.Errorf("tailscale script missing %q\n---\n%s", want, got)
		}
	}
}

func TestCloudInitScript_TailscaleWithoutTags(t *testing.T) {
	got, err := CloudInitScript(CloudInitOptions{TailscaleAuthKey: "tskey-auth-xyz"})
	if err != nil {
		t.Fatalf("CloudInitScript: %v", err)
	}
	if !strings.Contains(got, "tailscale up --authkey 'tskey-auth-xyz'") {
		t.Errorf("tailscale up line missing; got:\n%s", got)
	}
	if strings.Contains(got, "--advertise-tags") {
		t.Errorf("no tags supplied but --advertise-tags rendered:\n%s", got)
	}
}

func TestCloudInitScript_TailscaleKeyShellQuoted(t *testing.T) {
	got, err := CloudInitScript(CloudInitOptions{TailscaleAuthKey: "a'b"})
	if err != nil {
		t.Fatalf("CloudInitScript: %v", err)
	}
	if !strings.Contains(got, `tailscale up --authkey 'a'\''b'`) {
		t.Errorf("auth key not properly shell-quoted; got:\n%s", got)
	}
}

func TestPlanAPIEndpoint_Tailscale(t *testing.T) {
	p, err := PlanAPIEndpoint(APIEndpointInput{TailscaleAuthKey: "tskey-auth-1"})
	if err != nil {
		t.Fatalf("PlanAPIEndpoint: %v", err)
	}
	if p.Mode != "tailscale" {
		t.Errorf("Mode = %q, want tailscale", p.Mode)
	}
	if p.ExposeAPIPublicly {
		t.Errorf("ExposeAPIPublicly = true; tailscale keeps 6443 off the public internet")
	}
}

func TestPlanAPIEndpoint_Precedence(t *testing.T) {
	wg := "[Interface]\nAddress = 10.0.0.5/24\n"
	tmp := t.TempDir() + "/wg0.conf"
	if err := writeFile(tmp, wg); err != nil {
		t.Fatal(err)
	}
	// cloudflare beats tailscale.
	if p, _ := PlanAPIEndpoint(APIEndpointInput{CloudflareTunnelToken: "t", APIHostname: "h", TailscaleAuthKey: "k"}); p.Mode != "cloudflare" {
		t.Errorf("CF+tailscale: Mode = %q, want cloudflare", p.Mode)
	}
	// tailscale beats wireguard.
	if p, _ := PlanAPIEndpoint(APIEndpointInput{TailscaleAuthKey: "k", WireGuardConfig: tmp}); p.Mode != "tailscale" {
		t.Errorf("tailscale+WG: Mode = %q, want tailscale", p.Mode)
	}
}

func TestFinalizeServer_Tailscale(t *testing.T) {
	p := APIPlan{Mode: "tailscale"}
	server, skip, err := p.FinalizeServer("100.64.0.7")
	if err != nil {
		t.Fatalf("FinalizeServer: %v", err)
	}
	if server != "https://100.64.0.7:6443" {
		t.Errorf("server = %q", server)
	}
	if !skip {
		t.Errorf("skipVerify = false; k0s cert SAN is localhost, so direct-IP needs skip")
	}
	if _, _, err := p.FinalizeServer(""); err == nil {
		t.Errorf("expected error when tailnet addr is empty")
	}
}

func TestParseTailscaleIPv4(t *testing.T) {
	for _, tc := range []struct {
		name, raw, want string
		err             bool
	}{
		{name: "plain", raw: "100.64.0.7\n", want: "100.64.0.7"},
		{name: "trims + first line", raw: "  100.100.20.3\nfd7a::1\n", want: "100.100.20.3"},
		{name: "empty", raw: "\n", err: true},
		{name: "not cgnat", raw: "10.0.0.1\n", err: true},
		{name: "garbage", raw: "not-an-ip\n", err: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseTailscaleIPv4(tc.raw)
			if tc.err {
				if err == nil {
					t.Errorf("expected error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseTailscaleIPv4: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %q want %q", got, tc.want)
			}
		})
	}
}

func TestRewriteKubeconfig_SkipVerifyIP(t *testing.T) {
	in := minimalKubeconfig
	out, err := RewriteKubeconfig(in, KubeconfigRewrite{Server: "https://203.0.113.42:6443", InsecureSkipTLSVerify: true})
	if err != nil {
		t.Fatalf("RewriteKubeconfig: %v", err)
	}
	cfg, err := clientcmd.Load(out)
	if err != nil {
		t.Fatalf("parse rewritten: %v", err)
	}
	c := cfg.Clusters["k0s"]
	if c.Server != "https://203.0.113.42:6443" {
		t.Errorf("Server = %q", c.Server)
	}
	if !c.InsecureSkipTLSVerify {
		t.Errorf("InsecureSkipTLSVerify = false; expected true")
	}
	if len(c.CertificateAuthorityData) != 0 {
		t.Errorf("CA data not cleared")
	}
}

func TestRewriteKubeconfig_VerifyOnForCFHostname(t *testing.T) {
	// CF tunnel case: server is a real hostname with a publicly-trusted cert.
	out, err := RewriteKubeconfig(minimalKubeconfig, KubeconfigRewrite{Server: "https://k8s.example.com", InsecureSkipTLSVerify: false})
	if err != nil {
		t.Fatalf("RewriteKubeconfig: %v", err)
	}
	cfg, err := clientcmd.Load(out)
	if err != nil {
		t.Fatalf("parse rewritten: %v", err)
	}
	c := cfg.Clusters["k0s"]
	if c.Server != "https://k8s.example.com" {
		t.Errorf("Server = %q", c.Server)
	}
	if c.InsecureSkipTLSVerify {
		t.Errorf("InsecureSkipTLSVerify = true; expected false for CF tunnel case")
	}
	if len(c.CertificateAuthorityData) != 0 {
		t.Errorf("bundled CA leaked through (would override system trust store)")
	}
}

func TestRewriteKubeconfig_RejectsEmpty(t *testing.T) {
	if _, err := RewriteKubeconfig([]byte(``), KubeconfigRewrite{Server: "https://x"}); err == nil {
		t.Errorf("expected error on empty input")
	}
}

func TestParseWGAddress(t *testing.T) {
	for _, tc := range []struct {
		name, conf, want string
		err              bool
	}{
		{
			name: "v4",
			conf: "[Interface]\nAddress = 10.0.0.5/24\nPrivateKey = X\n[Peer]\nPublicKey = Y\n",
			want: "10.0.0.5",
		},
		{
			name: "dual-stack picks v4 first",
			conf: "[Interface]\nAddress = 10.0.0.5/24, fd00::5/64\n",
			want: "10.0.0.5",
		},
		{
			name: "case-insensitive",
			conf: "[interface]\naddress = 10.0.0.5/32\n",
			want: "10.0.0.5",
		},
		{
			name: "ignores Address in [Peer]",
			conf: "[Peer]\nAddress = 1.2.3.4/32\n[Interface]\nAddress = 10.0.0.7/24\n",
			want: "10.0.0.7",
		},
		{
			name: "missing",
			conf: "[Interface]\nPrivateKey = X\n",
			err:  true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseWGAddress([]byte(tc.conf))
			if tc.err {
				if err == nil {
					t.Errorf("expected error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseWGAddress: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %q want %q", got, tc.want)
			}
		})
	}
}

func TestLoadWireGuardConfig_RejectsHeredocCollision(t *testing.T) {
	// Write a config that contains the heredoc delimiter literally — would
	// break the cloud-init heredoc otherwise.
	conf := "[Interface]\nAddress = 10.0.0.1/24\n# AGENTCTL_WG_EOF\n"
	tmp := t.TempDir() + "/wg0.conf"
	if err := writeFile(tmp, conf); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadWireGuardConfig(tmp); err == nil {
		t.Errorf("expected error on heredoc-delimiter collision")
	}
}

// helpers ---------------------------------------------------------------------

var minimalKubeconfig = []byte(`apiVersion: v1
kind: Config
clusters:
- name: k0s
  cluster:
    server: https://localhost:6443
    certificate-authority-data: ZmFrZQ==
contexts:
- name: k0s
  context: {cluster: k0s, user: admin}
current-context: k0s
users:
- name: admin
  user:
    client-certificate-data: ZmFrZQ==
    client-key-data: ZmFrZQ==
`)

func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o600)
}
