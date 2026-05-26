package cloud

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/crypto/ssh"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// KubeconfigRewrite controls how RewriteKubeconfig transforms the k0s admin.conf.
type KubeconfigRewrite struct {
	// Server is the URL the kubeconfig's cluster server is rewritten to.
	// E.g. "https://203.0.113.42:6443" or "https://k8s.example.com".
	Server string

	// InsecureSkipTLSVerify drops the bundled CA and tells the client to
	// skip server verification. Set to true when the cert presented by the
	// server doesn't match Server (e.g., dialing k0s directly by IP — k0s'
	// default cert SAN is "localhost"). Set to false when the Server is
	// fronted by a load balancer / Cloudflare Tunnel that presents a
	// publicly-trusted cert for the hostname.
	InsecureSkipTLSVerify bool
}

// RewriteKubeconfig parses raw and applies r. Returns the rewritten YAML bytes.
func RewriteKubeconfig(raw []byte, r KubeconfigRewrite) ([]byte, error) {
	cfg, err := clientcmd.Load(raw)
	if err != nil {
		return nil, fmt.Errorf("parse kubeconfig: %w", err)
	}
	if len(cfg.Clusters) == 0 {
		return nil, fmt.Errorf("kubeconfig has no clusters")
	}
	for name, c := range cfg.Clusters {
		c.Server = r.Server
		if r.InsecureSkipTLSVerify {
			c.InsecureSkipTLSVerify = true
			c.CertificateAuthorityData = nil
			c.CertificateAuthority = ""
		} else {
			// Verifying against the bundled k0s CA would fail (its SAN is
			// localhost). When the caller wants verify-on, drop the bundled
			// CA so the system trust store is used for the new hostname.
			c.InsecureSkipTLSVerify = false
			c.CertificateAuthorityData = nil
			c.CertificateAuthority = ""
		}
		cfg.Clusters[name] = c
	}
	return clientcmd.Write(*cfg)
}

// TokenKubeconfig builds a kubeconfig that authenticates with a bearer token
// against server. No CA is embedded: the server is reached via a hostname
// fronted by a publicly-trusted cert (Cloudflare Tunnel), so the system trust
// store verifies it. Used where client-cert auth can't survive a
// TLS-terminating proxy. token is a secret — keep it out of logs.
func TokenKubeconfig(server, token string) ([]byte, error) {
	cfg := clientcmdapi.NewConfig()
	cfg.Clusters["cluster"] = &clientcmdapi.Cluster{Server: server}
	cfg.AuthInfos["agentctl"] = &clientcmdapi.AuthInfo{Token: token}
	cfg.Contexts["agentctl"] = &clientcmdapi.Context{Cluster: "cluster", AuthInfo: "agentctl"}
	cfg.CurrentContext = "agentctl"
	return clientcmd.Write(*cfg)
}

// StageKubeconfigInput drives StageKubeconfig.
type StageKubeconfigInput struct {
	SSHAddr      string            // host:22 of the bootstrapped node
	SSHCfg       *ssh.ClientConfig // to fetch admin.conf or mint a token
	Plan         APIPlan           // networking plan (decides cert vs token)
	EndpointAddr string            // resolved addr for FinalizeServer (public/tailnet IP)
	Name         string            // temp-dir discriminator
}

// StageKubeconfig produces the kubeconfig the platform install will use and
// returns its temp path, the resolved server URL, and how long to wait for the
// API to answer.
//
// For cloudflare mode it mints a ServiceAccount token over SSH and writes a
// token-based kubeconfig (see MintAdminToken for why client certs don't work
// through Cloudflare), and allows extra time for cloudflared's cold-start +
// edge registration. Every other mode keeps the cert-based admin.conf (TLS is
// end-to-end to k0s) and just rewrites the server URL.
//
// Callers must os.RemoveAll the returned path (it holds cluster credentials).
func StageKubeconfig(ctx context.Context, in StageKubeconfigInput) (kcPath, server string, apiTimeout time.Duration, err error) {
	server, skipVerify, err := in.Plan.FinalizeServer(in.EndpointAddr)
	if err != nil {
		return "", "", 0, fmt.Errorf("finalize server: %w", err)
	}

	var kc []byte
	if in.Plan.Mode == "cloudflare" {
		token, terr := MintAdminToken(ctx, in.SSHAddr, in.SSHCfg, 24*time.Hour)
		if terr != nil {
			return "", "", 0, terr
		}
		kc, err = TokenKubeconfig(server, token)
		apiTimeout = 4 * time.Minute // cloudflared cold-start + edge registration + DNS
	} else {
		raw, ferr := FetchFile(ctx, in.SSHAddr, in.SSHCfg, DefaultKubeconfigPath)
		if ferr != nil {
			return "", "", 0, fmt.Errorf("fetch kubeconfig: %w", ferr)
		}
		kc, err = RewriteKubeconfig(raw, KubeconfigRewrite{Server: server, InsecureSkipTLSVerify: skipVerify})
		apiTimeout = 90 * time.Second
	}
	if err != nil {
		return "", "", 0, fmt.Errorf("build kubeconfig: %w", err)
	}

	kcPath, err = WriteKubeconfigTemp(in.Name, kc)
	if err != nil {
		return "", "", 0, err
	}
	return kcPath, server, apiTimeout, nil
}

// WriteKubeconfigTemp writes data to a unique 0600 file under $TMPDIR and
// returns its path. Callers should defer os.RemoveAll on the returned
// dir (kubeconfigs hold cluster credentials).
func WriteKubeconfigTemp(name string, data []byte) (string, error) {
	dir, err := os.MkdirTemp("", "agentctl-"+name+"-")
	if err != nil {
		return "", fmt.Errorf("mktemp: %w", err)
	}
	p := filepath.Join(dir, "kubeconfig")
	if err := os.WriteFile(p, data, 0o600); err != nil {
		return "", fmt.Errorf("write kubeconfig: %w", err)
	}
	return p, nil
}

// WaitForAPI polls <server>/readyz until it answers 200 or the deadline fires.
// Used after a provisioning target's networking is in place (Cloudflare Tunnel
// can take seconds to propagate; WireGuard may need a moment after wg-quick).
//
// Honors InsecureSkipTLSVerify from the kubeconfig so the same check works
// both for direct-IP and CF-hostname cases.
func WaitForAPI(ctx context.Context, kubeconfigPath string, deadline time.Duration) error {
	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	if err != nil {
		return fmt.Errorf("load kubeconfig: %w", err)
	}
	tr, err := rest.TransportFor(cfg)
	if err != nil {
		return fmt.Errorf("transport: %w", err)
	}
	u, err := url.Parse(cfg.Host)
	if err != nil {
		return fmt.Errorf("parse server: %w", err)
	}
	u.Path = "/readyz"
	target := u.String()

	hc := &http.Client{Transport: tr, Timeout: 5 * time.Second}
	dctx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	tick := time.NewTicker(3 * time.Second)
	defer tick.Stop()
	for {
		req, _ := http.NewRequestWithContext(dctx, http.MethodGet, target, nil)
		resp, err := hc.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		select {
		case <-dctx.Done():
			return fmt.Errorf("api %s not reachable within %s: %w", target, deadline, dctx.Err())
		case <-tick.C:
		}
	}
}
