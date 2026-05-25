package cloud

import (
	"fmt"
	"os"
	"path/filepath"

	"k8s.io/client-go/tools/clientcmd"
)

// RewriteKubeconfig replaces every cluster server URL in the kubeconfig with
// publicURL. k0s writes admin.conf with server=https://localhost:6443, which
// the deploy driver cannot reach from outside the VPC — we patch it in-process
// before saving the file so subsequent steps Just Work with --kubeconfig.
//
// Returns the rewritten YAML bytes; does not touch disk.
func RewriteKubeconfig(raw []byte, publicURL string) ([]byte, error) {
	cfg, err := clientcmd.Load(raw)
	if err != nil {
		return nil, fmt.Errorf("parse kubeconfig: %w", err)
	}
	if len(cfg.Clusters) == 0 {
		return nil, fmt.Errorf("kubeconfig has no clusters")
	}
	for name, c := range cfg.Clusters {
		c.Server = publicURL
		// k0s sets the bundle as CA data, but its CN is localhost. Telling
		// clients to skip TLS verify is unfortunate; the deploy driver is
		// a one-shot install, not a steady-state credential, so the trade-off
		// is acceptable for V1. A future iteration can either fetch the
		// real CA + a SAN for the public IP, or use --tls-server-name.
		c.InsecureSkipTLSVerify = true
		c.CertificateAuthorityData = nil
		c.CertificateAuthority = ""
		cfg.Clusters[name] = c
	}
	return clientcmd.Write(*cfg)
}

// WriteKubeconfigTemp writes rewritten kubeconfig bytes to a unique file under
// $TMPDIR and returns its path. Callers should defer os.Remove on the returned
// path (kubeconfigs hold cluster credentials).
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
