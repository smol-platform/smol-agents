//go:build wgnetstack

// Package wireguard, netstack-backed implementation. Builds only with
// `-tags=wgnetstack` so the lightweight test path stays free of the
// wireguard-go dependency tree (gvisor + 30+ MB of code).
//
// Wires UserspaceDevice.WithStartFn to a function that creates a
// gVisor netstack TUN, hands it to wireguard-go's device.Device, and
// loads the rendered UAPI string. The agent (or test driver) gets
// back a device that's "up" and synthesizes packets entirely in
// userspace — no kernel module, no /dev/net/tun.

package wireguard

import (
	"bytes"
	"context"
	"fmt"
	"net/netip"
	"strings"

	"golang.zx2c4.com/wireguard/conn"
	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun/netstack"
)

// netstackDevice satisfies the statefulDevice interface with a real
// wireguard-go device on top of a gVisor TUN.
type netstackDevice struct {
	dev    *device.Device
	logger *device.Logger
}

func (d *netstackDevice) IpcSet(uapi string) error {
	return d.dev.IpcSet(uapi)
}

func (d *netstackDevice) Close() {
	if d.dev != nil {
		d.dev.Close()
	}
}

func (d *netstackDevice) PeerStates() []PeerState {
	// wireguard-go's IpcGet returns UAPI as multi-line text; we
	// could parse it here but for the e2e smoke we just report the
	// device as up.
	var buf bytes.Buffer
	_ = d.dev.IpcGetOperation(&buf)
	return parseUAPIPeerStates(buf.String())
}

// EnableNetstackBackend installs the netstack-backed start function
// on u, replacing the default ErrNotWired path. Call this from the
// production main wiring (or a test-driver init).
func EnableNetstackBackend(u *UserspaceDevice) {
	u.WithStartFn(func(ctx context.Context, cfg Config) (statefulDevice, error) {
		return startNetstack(ctx, cfg)
	})
}

func startNetstack(_ context.Context, cfg Config) (statefulDevice, error) {
	addrs, err := parseAddrs(cfg.Addresses)
	if err != nil {
		return nil, fmt.Errorf("addresses: %w", err)
	}
	dnsAddrs, _ := parseAddrs(cfg.DNS)

	mtu := cfg.MTU
	if mtu == 0 {
		mtu = 1420
	}

	tun, _, err := netstack.CreateNetTUN(addrs, dnsAddrs, mtu)
	if err != nil {
		return nil, fmt.Errorf("netstack TUN: %w", err)
	}

	bindLogger := device.NewLogger(device.LogLevelError, "[wgnetstack] ")
	dev := device.NewDevice(tun, conn.NewDefaultBind(), bindLogger)

	return &netstackDevice{dev: dev, logger: bindLogger}, nil
}

func parseAddrs(in []string) ([]netip.Addr, error) {
	out := make([]netip.Addr, 0, len(in))
	for _, s := range in {
		// Accept "10.99.0.5/32" — strip mask, keep address.
		prefix, err := netip.ParsePrefix(s)
		if err == nil {
			out = append(out, prefix.Addr())
			continue
		}
		addr, err := netip.ParseAddr(s)
		if err != nil {
			return nil, fmt.Errorf("invalid address %q: %w", s, err)
		}
		out = append(out, addr)
	}
	return out, nil
}

// parseUAPIPeerStates pulls minimal info out of wireguard-go's UAPI
// dump. We only care about whether peers exist; full state needs a
// proper parser later.
func parseUAPIPeerStates(uapi string) []PeerState {
	const (
		pubKeyPrefix = "public_key="
		hsPrefix     = "last_handshake_time_sec="
	)
	var out []PeerState
	var current *PeerState
	for _, line := range strings.Split(uapi, "\n") {
		switch {
		case strings.HasPrefix(line, pubKeyPrefix):
			if current != nil {
				out = append(out, *current)
			}
			current = &PeerState{
				PublicKey: strings.TrimPrefix(line, pubKeyPrefix),
				State:     "handshaking",
			}
		case strings.HasPrefix(line, hsPrefix):
			if current != nil {
				v := strings.TrimPrefix(line, hsPrefix)
				if v != "0" {
					current.State = "connected"
					current.LastHandshake = v
				}
			}
		}
	}
	if current != nil {
		out = append(out, *current)
	}
	return out
}

var _ = bytes.Split // keep import slot
