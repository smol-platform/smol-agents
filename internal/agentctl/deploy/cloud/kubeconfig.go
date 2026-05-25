package cloud

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
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
