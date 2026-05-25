package cloud

import (
	"context"
	"fmt"
	"net/netip"
	"strings"

	"golang.org/x/crypto/ssh"
)

// tailscaleCGNAT is the 100.64.0.0/10 range Tailscale assigns tailnet IPv4s from.
var tailscaleCGNAT = netip.MustParsePrefix("100.64.0.0/10")

// FetchTailscaleIPv4 runs `tailscale ip -4` over SSH and returns the host's
// tailnet IPv4. Called after the node has joined the tailnet (post-sentinel):
// the address is assigned by the Tailscale control plane at join time, so —
// unlike WireGuard, whose address is in the config — it isn't known until the
// box is up. addr is the host:port to reach sshd on (the public IP during
// bootstrap; the tailnet is for the k8s API, not this SSH hop).
func FetchTailscaleIPv4(ctx context.Context, addr string, cfg *ssh.ClientConfig) (string, error) {
	out, _, err := runSSH(ctx, addr, cfg, "tailscale ip -4")
	if err != nil {
		return "", fmt.Errorf("ssh tailscale ip -4: %w", err)
	}
	return parseTailscaleIPv4(string(out))
}

// parseTailscaleIPv4 extracts the first line of `tailscale ip -4` output and
// validates it's a 100.64.0.0/10 address. Split out from the SSH call so the
// validation is unit-testable.
func parseTailscaleIPv4(raw string) (string, error) {
	ip := strings.TrimSpace(raw)
	if i := strings.IndexByte(ip, '\n'); i >= 0 {
		ip = strings.TrimSpace(ip[:i])
	}
	if ip == "" {
		return "", fmt.Errorf("tailscale ip -4 returned no address; is the node joined?")
	}
	a, err := netip.ParseAddr(ip)
	if err != nil {
		return "", fmt.Errorf("parse tailscale ip %q: %w", ip, err)
	}
	if !tailscaleCGNAT.Contains(a) {
		return "", fmt.Errorf("tailscale ip %s not in 100.64.0.0/10; is the node joined to the tailnet?", ip)
	}
	return ip, nil
}
