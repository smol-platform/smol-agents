package wireguard

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"

	v1 "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

// Mode constants for WireGuardSpec.Mode and Config.Mode.
const (
	ModeClient = "client"
	ModeServer = "server"
)

// Adapter abstracts the WireGuard backend. Production = UserspaceDevice
// (wireguard-go + netstack). Tests = FakeAdapter.
type Adapter interface {
	// Start brings the device up with the given config. Idempotent
	// for a single Adapter instance — callers MUST Stop before
	// re-Start.
	Start(ctx context.Context, cfg Config) error

	// Stop tears the device down. After Stop, Start can be called
	// again with a new config.
	Stop() error

	// Peers returns the current peer state for telemetry.
	Peers() []PeerState
}

// Config is the resolved WireGuard configuration. PrivateKey is the
// raw 32-byte key (already base64-decoded from the broker secret);
// the spec's PrivateKeyRef does NOT live here.
type Config struct {
	Mode       string // "client" | "server"
	PrivateKey []byte // 32 bytes
	ListenPort uint16
	Addresses  []string
	DNS        []string
	MTU        int
	Peers      []Peer
}

// Peer is a resolved peer entry; PSK already decoded from the broker.
type Peer struct {
	Name                       string
	PublicKey                  []byte // 32 bytes
	Endpoint                   string // host:port; may be empty for server-mode peers
	AllowedIPs                 []string
	PersistentKeepaliveSeconds int
	PresharedKey               []byte // 32 bytes; optional
}

// PeerState is the runtime view of one peer.
type PeerState struct {
	Name          string
	PublicKey     string // base64
	LastHandshake string // RFC3339; "" if never
	BytesRX       int64
	BytesTX       int64
	State         string // "connected" | "handshaking" | "disconnected"
}

// BuildConfig translates the CRD spec + the broker-fetched private key
// + per-peer PSKs into a runtime Config. Pure — easy to test.
func BuildConfig(spec v1.WireGuardSpec, privateKey []byte, psks map[string][]byte) (Config, error) {
	if len(privateKey) != 32 {
		return Config{}, fmt.Errorf("wireguard: privateKey must be 32 bytes, got %d", len(privateKey))
	}
	if spec.Mode != "client" && spec.Mode != "server" {
		return Config{}, fmt.Errorf("wireguard: mode=%q invalid", spec.Mode)
	}
	port := uint16(0)
	if spec.ListenPort > 0 {
		port = uint16(spec.ListenPort)
	} else if spec.Mode == "server" {
		port = 51820
	}
	mtu := int(spec.MTU)
	if mtu == 0 {
		mtu = 1420
	}

	peers := make([]Peer, 0, len(spec.Peers))
	for i, p := range spec.Peers {
		pk, err := decodeKey(p.PublicKey)
		if err != nil {
			return Config{}, fmt.Errorf("peer[%d].publicKey: %w", i, err)
		}
		psk := psks[p.Name]
		if p.PSKRef != nil && len(psk) == 0 {
			return Config{}, fmt.Errorf("peer[%d]: pskRef set but no PSK supplied for %q", i, p.Name)
		}
		peers = append(peers, Peer{
			Name:                       p.Name,
			PublicKey:                  pk,
			Endpoint:                   p.Endpoint,
			AllowedIPs:                 append([]string(nil), p.AllowedIPs...),
			PersistentKeepaliveSeconds: int(p.PersistentKeepalive),
			PresharedKey:               psk,
		})
	}

	return Config{
		Mode:       spec.Mode,
		PrivateKey: privateKey,
		ListenPort: port,
		Addresses:  append([]string(nil), spec.Addresses...),
		DNS:        append([]string(nil), spec.DNS...),
		MTU:        mtu,
		Peers:      peers,
	}, nil
}

// RenderUAPI returns a wireguard-go IpcSet text block. This is the
// WireGuard userspace control protocol's wire format and is exactly
// what wireguard-go's `device.IpcSet(string)` expects.
//
// We render this from Config (which is already validated) so tests
// can assert on the bytes without a real device.
func (c Config) RenderUAPI() string {
	var b strings.Builder
	b.WriteString("private_key=")
	b.WriteString(hex32(c.PrivateKey))
	b.WriteByte('\n')
	if c.ListenPort > 0 {
		fmt.Fprintf(&b, "listen_port=%d\n", c.ListenPort)
	}
	for _, p := range c.Peers {
		fmt.Fprintf(&b, "public_key=%s\n", hex32(p.PublicKey))
		if len(p.PresharedKey) == 32 {
			fmt.Fprintf(&b, "preshared_key=%s\n", hex32(p.PresharedKey))
		}
		if p.Endpoint != "" {
			fmt.Fprintf(&b, "endpoint=%s\n", p.Endpoint)
		}
		if p.PersistentKeepaliveSeconds > 0 {
			fmt.Fprintf(&b, "persistent_keepalive_interval=%d\n", p.PersistentKeepaliveSeconds)
		}
		for _, cidr := range p.AllowedIPs {
			fmt.Fprintf(&b, "allowed_ip=%s\n", cidr)
		}
	}
	return b.String()
}

func decodeKey(s string) ([]byte, error) {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, err
	}
	if len(b) != 32 {
		return nil, fmt.Errorf("expected 32 bytes, got %d", len(b))
	}
	return b, nil
}

func hex32(b []byte) string {
	if len(b) != 32 {
		return ""
	}
	const hexc = "0123456789abcdef"
	out := make([]byte, 64)
	for i, v := range b {
		out[i*2] = hexc[v>>4]
		out[i*2+1] = hexc[v&0xf]
	}
	return string(out)
}

// FakeAdapter is the in-memory stand-in used by tests. It records
// everything Start was called with and lets tests transition peer
// states.
type FakeAdapter struct {
	mu       sync.Mutex
	Started  bool
	StartCfg Config
	Stops    int
	State    map[string]PeerState
}

// NewFakeAdapter returns an empty fake.
func NewFakeAdapter() *FakeAdapter {
	return &FakeAdapter{State: map[string]PeerState{}}
}

func (f *FakeAdapter) Start(_ context.Context, cfg Config) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Started {
		return errors.New("fake: already started")
	}
	f.Started = true
	f.StartCfg = cfg
	for _, p := range cfg.Peers {
		f.State[p.Name] = PeerState{
			Name:      p.Name,
			PublicKey: base64.StdEncoding.EncodeToString(p.PublicKey),
			State:     "handshaking",
		}
	}
	return nil
}

func (f *FakeAdapter) Stop() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.Started {
		return nil
	}
	f.Started = false
	f.Stops++
	f.State = map[string]PeerState{}
	return nil
}

func (f *FakeAdapter) Peers() []PeerState {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]PeerState, 0, len(f.State))
	for _, s := range f.State {
		out = append(out, s)
	}
	return out
}

// SetPeerConnected is a test helper.
func (f *FakeAdapter) SetPeerConnected(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s, ok := f.State[name]; ok {
		s.State = "connected"
		f.State[name] = s
	}
}
