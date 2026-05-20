//go:build integration

package integration

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/spiffe/go-spiffe/v2/spiffeid"

	"github.com/stigen/smol-agents/pkg/secrets"
)

// Test the full client/server round trip including the wire protocol and
// in-memory backend. Uses the FixedPeerAttestor so the test is OS-agnostic.
func TestIntegration_BrokerRoundTrip(t *testing.T) {
	td := spiffeid.RequireTrustDomainFromString("stigen.ai")
	a := spiffeid.RequireFromPath(td, "/ns/agents/sa/a")

	// Darwin's UDS path limit is 104 chars; t.TempDir() under /var/folders
	// can exceed that. Use a short /tmp path instead.
	socket := filepath.Join("/tmp", "ka-it-"+t.Name()+".sock")
	t.Cleanup(func() { _ = os.Remove(socket) })
	backend := secrets.NewStaticBackend()
	backend.Set(a, "creds", []byte("hello"))

	policy := secrets.NewStaticPolicy()
	policy.Grant(a, "creds")

	srv := &secrets.Server{
		SocketPath:  socket,
		Backend:     backend,
		Policy:      policy,
		Attestor:    secrets.FixedPeerAttestor{ID: a},
		MaxLeaseTTL: 5 * time.Minute,
		DefaultTTL:  time.Minute,
		Logger:      slog.Default(),
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() { _ = srv.Listen(ctx) }()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := net.Dial("unix", socket); err == nil {
			_ = c.Close()
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	c := secrets.NewClient(socket)
	defer c.Close()

	l, err := c.Lease(context.Background(), "creds", 0)
	if err != nil {
		t.Fatalf("Lease: %v", err)
	}
	if string(l.Value) != "hello" {
		t.Errorf("got %q, want hello", l.Value)
	}
	if !l.Valid(time.Now()) {
		t.Error("lease should be valid immediately")
	}

	// Drive to TTLExceeded.
	_, err = c.Lease(context.Background(), "creds", time.Hour)
	if !errors.Is(err, secrets.ErrTTLExceeded) {
		t.Errorf("got %v, want ErrTTLExceeded", err)
	}
}
