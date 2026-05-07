//go:build wgnetstack

package wireguard

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestNetstack_DeviceBootsAndAcceptsConfig brings up a netstack-
// backed UserspaceDevice with a synthetic peer and asserts:
//  1. EnableNetstackBackend installs a startFn (no ErrNotWired).
//  2. Start() returns nil — the device created its TUN, parsed
//     the UAPI, and is up.
//  3. Peers() returns a non-empty slice (state may be
//     "handshaking" since the peer endpoint is unreachable;
//     that's the smoke we need — the device boots).
//
// We don't assert handshake completion here because that needs a
// reachable peer (handled by the L0 e2e ring against wg-hub).
func TestNetstack_DeviceBootsAndAcceptsConfig(t *testing.T) {
	mk32 := func(b byte) []byte {
		out := make([]byte, 32)
		for i := range out {
			out[i] = b
		}
		return out
	}

	dev := NewUserspaceDevice()
	EnableNetstackBackend(dev)

	cfg := Config{
		Mode:       ModeClient,
		PrivateKey: mk32(0xAA),
		Addresses:  []string{"10.99.0.5/32"},
		MTU:        1420,
		Peers: []Peer{{
			Name:                       "hub",
			PublicKey:                  mk32(0xBB),
			Endpoint:                   "127.0.0.1:51820",
			AllowedIPs:                 []string{"10.0.0.0/16"},
			PersistentKeepaliveSeconds: 0,
		}},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := dev.Start(ctx, cfg); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer dev.Stop()

	peers := dev.Peers()
	if len(peers) == 0 {
		t.Error("Peers() returned empty after Start with one peer configured")
	}
	for _, p := range peers {
		if !strings.HasPrefix(p.PublicKey, "bbbbbbbb") {
			t.Errorf("peer key not echoed: %q", p.PublicKey)
		}
	}
}
