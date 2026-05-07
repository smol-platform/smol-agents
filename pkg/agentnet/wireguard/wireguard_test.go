package wireguard

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	v1 "github.com/stigen/knative-agents/pkg/agentmodel/v1"
)

func mk32(b byte) []byte {
	out := make([]byte, 32)
	for i := range out {
		out[i] = b
	}
	return out
}

func TestBuildConfig_ClientHappy(t *testing.T) {
	spec := v1.WireGuardSpec{
		Mode:          "client",
		PrivateKeyRef: v1.AuthRef{SecretName: "k"},
		Addresses:     []string{"10.99.0.5/32"},
		Peers: []v1.WGPeer{{
			Name:       "hub",
			PublicKey:  base64.StdEncoding.EncodeToString(mk32(0x01)),
			Endpoint:   "vpn.example.com:51820",
			AllowedIPs: []string{"10.0.0.0/16"},
		}},
	}
	cfg, err := BuildConfig(spec, mk32(0x09), nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MTU != 1420 {
		t.Errorf("default MTU = %d", cfg.MTU)
	}
	if len(cfg.Peers) != 1 {
		t.Errorf("peers = %d", len(cfg.Peers))
	}
	if !bytes.Equal(cfg.Peers[0].PublicKey, mk32(0x01)) {
		t.Error("peer key not decoded")
	}
}

func TestBuildConfig_ServerDefaultsListenPort(t *testing.T) {
	spec := v1.WireGuardSpec{
		Mode:          "server",
		PrivateKeyRef: v1.AuthRef{SecretName: "k"},
		Addresses:     []string{"10.99.0.1/24"},
	}
	cfg, err := BuildConfig(spec, mk32(0x09), nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ListenPort != 51820 {
		t.Errorf("default server port = %d", cfg.ListenPort)
	}
}

func TestBuildConfig_RejectsBadKey(t *testing.T) {
	spec := v1.WireGuardSpec{Mode: "client", PrivateKeyRef: v1.AuthRef{SecretName: "k"}}
	if _, err := BuildConfig(spec, []byte{1, 2, 3}, nil); err == nil {
		t.Error("expected key length rejection")
	}
}

func TestBuildConfig_RejectsBadMode(t *testing.T) {
	spec := v1.WireGuardSpec{Mode: "garbage", PrivateKeyRef: v1.AuthRef{SecretName: "k"}}
	if _, err := BuildConfig(spec, mk32(1), nil); err == nil {
		t.Error("expected mode rejection")
	}
}

func TestBuildConfig_PSKMissingErrors(t *testing.T) {
	spec := v1.WireGuardSpec{
		Mode:          "client",
		PrivateKeyRef: v1.AuthRef{SecretName: "k"},
		Peers: []v1.WGPeer{{
			Name:       "hub",
			PublicKey:  base64.StdEncoding.EncodeToString(mk32(0x01)),
			AllowedIPs: []string{"10.0.0.0/16"},
			PSKRef:     &v1.AuthRef{SecretName: "psk-secret"},
		}},
	}
	_, err := BuildConfig(spec, mk32(9), map[string][]byte{}) // no PSK provided
	if err == nil || !strings.Contains(err.Error(), "PSK") {
		t.Errorf("expected PSK error: %v", err)
	}
}

func TestRenderUAPI_StableShape(t *testing.T) {
	cfg := Config{
		Mode:       "client",
		PrivateKey: mk32(0xAA),
		ListenPort: 51820,
		Peers: []Peer{{
			Name:                       "hub",
			PublicKey:                  mk32(0xBB),
			Endpoint:                   "vpn.example.com:51820",
			AllowedIPs:                 []string{"10.0.0.0/16"},
			PersistentKeepaliveSeconds: 25,
		}},
	}
	out := cfg.RenderUAPI()
	for _, want := range []string{
		"private_key=aaaaaaaa",
		"listen_port=51820",
		"public_key=bbbbbbbb",
		"endpoint=vpn.example.com:51820",
		"persistent_keepalive_interval=25",
		"allowed_ip=10.0.0.0/16",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("UAPI missing %q\n---\n%s", want, out)
		}
	}
}

func TestFakeAdapter_StartStop(t *testing.T) {
	a := NewFakeAdapter()
	cfg := Config{Mode: "client", PrivateKey: mk32(1), Peers: []Peer{{Name: "p1", PublicKey: mk32(2), AllowedIPs: []string{"10/8"}}}}
	if err := a.Start(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if !a.Started {
		t.Error("Started not set")
	}
	if got := a.Peers(); len(got) != 1 || got[0].State != "handshaking" {
		t.Errorf("expected one handshaking peer, got %+v", got)
	}
	a.SetPeerConnected("p1")
	if got := a.Peers(); got[0].State != "connected" {
		t.Errorf("expected connected, got %s", got[0].State)
	}
	if err := a.Stop(); err != nil {
		t.Fatal(err)
	}
	if a.Started {
		t.Error("Stop did not clear Started")
	}
	if a.Stops != 1 {
		t.Errorf("Stops = %d", a.Stops)
	}
}

func TestFakeAdapter_DoubleStartErrors(t *testing.T) {
	a := NewFakeAdapter()
	cfg := Config{Mode: "client", PrivateKey: mk32(1)}
	if err := a.Start(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	err := a.Start(context.Background(), cfg)
	if err == nil {
		t.Error("expected double-start error")
	}
}

func TestUserspaceDevice_NoStartFn_ReturnsErrNotWired(t *testing.T) {
	d := NewUserspaceDevice()
	err := d.Start(context.Background(), Config{Mode: "client", PrivateKey: mk32(1)})
	if !errors.Is(err, ErrNotWired) {
		t.Errorf("expected ErrNotWired, got %v", err)
	}
}

func TestUserspaceDevice_WithStartFn_Lifecycle(t *testing.T) {
	d := NewUserspaceDevice()
	stub := &stubDevice{}
	d.WithStartFn(func(_ context.Context, _ Config) (statefulDevice, error) {
		return stub, nil
	})
	if err := d.Start(context.Background(), Config{Mode: "client", PrivateKey: mk32(1)}); err != nil {
		t.Fatal(err)
	}
	if !stub.ipcSet {
		t.Error("IpcSet was not invoked")
	}
	if err := d.Stop(); err != nil {
		t.Fatal(err)
	}
	if !stub.closed {
		t.Error("Close was not invoked")
	}
}

type stubDevice struct {
	ipcSet bool
	closed bool
}

func (s *stubDevice) IpcSet(_ string) error   { s.ipcSet = true; return nil }
func (s *stubDevice) Close()                  { s.closed = true }
func (s *stubDevice) PeerStates() []PeerState { return nil }
