package secrets

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/spiffe/go-spiffe/v2/spiffeid"
)

var (
	tdStigen = spiffeid.RequireTrustDomainFromString("stigen.ai")
	idA      = spiffeid.RequireFromPath(tdStigen, "/ns/agents/sa/agent-a")
	idB      = spiffeid.RequireFromPath(tdStigen, "/ns/agents/sa/agent-b")
	idIntr   = spiffeid.RequireFromPath(tdStigen, "/ns/intruder/sa/x")
)

func startServer(t *testing.T, principal spiffeid.ID, attestErr error) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	socket := filepath.Join(dir, "broker.sock")
	backend := NewStaticBackend()
	backend.Set(idA, "db-cred", []byte("super-secret"))
	backend.Set(idB, "api-key", []byte("k123"))
	policy := NewStaticPolicy()
	policy.Grant(idA, "db-cred")
	policy.Grant(idB, "api-key")

	s := &Server{
		SocketPath:  socket,
		MaxLeaseTTL: 5 * time.Minute,
		DefaultTTL:  time.Minute,
		Backend:     backend,
		Policy:      policy,
		Attestor:    FixedPeerAttestor{ID: principal, Err: attestErr},
		Now:         func() time.Time { return time.Date(2026, 5, 6, 0, 0, 0, 0, time.UTC) },
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	ready := make(chan struct{})
	go func() {
		// Listen blocks; signal ready after the listener is bound.
		go func() {
			// Poll until socket exists.
			deadline := time.Now().Add(2 * time.Second)
			for time.Now().Before(deadline) {
				if c, err := net.Dial("unix", socket); err == nil {
					_ = c.Close()
					close(ready)
					return
				}
				time.Sleep(10 * time.Millisecond)
			}
			close(ready)
		}()
		_ = s.Listen(ctx)
	}()

	select {
	case <-ready:
	case <-time.After(2 * time.Second):
		t.Fatal("server failed to start")
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, socket
}

func TestServer_Authorized(t *testing.T) {
	_, socket := startServer(t, idA, nil)
	c := NewClient(socket)
	defer c.Close()

	lease, err := c.Lease(context.Background(), "db-cred", 0)
	if err != nil {
		t.Fatalf("Lease: %v", err)
	}
	if string(lease.Value) != "super-secret" {
		t.Errorf("got value %q, want super-secret", lease.Value)
	}
	if lease.TTL != time.Minute {
		t.Errorf("default TTL not applied: got %v", lease.TTL)
	}
	if lease.Audience != idA {
		t.Errorf("audience = %s, want %s", lease.Audience, idA)
	}
}

func TestServer_Unauthorized(t *testing.T) {
	_, socket := startServer(t, idIntr, nil)
	c := NewClient(socket)
	defer c.Close()
	_, err := c.Lease(context.Background(), "db-cred", 0)
	if err == nil {
		t.Fatal("expected unauthorized error")
	}
	if !errors.Is(err, ErrUnauthorized) {
		t.Errorf("got %v, want ErrUnauthorized", err)
	}
}

func TestServer_AttestFailure(t *testing.T) {
	_, socket := startServer(t, spiffeid.ID{}, errors.New("attestor down"))
	c := NewClient(socket)
	defer c.Close()
	_, err := c.Lease(context.Background(), "db-cred", 0)
	if err == nil {
		t.Fatal("expected error when attestor fails")
	}
	if !errors.Is(err, ErrUnauthorized) {
		t.Errorf("got %v, want ErrUnauthorized", err)
	}
}

func TestServer_TTLExceeded(t *testing.T) {
	_, socket := startServer(t, idA, nil)
	c := NewClient(socket)
	defer c.Close()
	_, err := c.Lease(context.Background(), "db-cred", time.Hour)
	if err == nil {
		t.Fatal("expected TTLExceeded")
	}
	if !errors.Is(err, ErrTTLExceeded) {
		t.Errorf("got %v, want ErrTTLExceeded", err)
	}
}

func TestServer_NotFound(t *testing.T) {
	_, socket := startServer(t, idA, nil)
	// idA is allowed in policy — we add a name that isn't in policy.
	c := NewClient(socket)
	defer c.Close()
	_, err := c.Lease(context.Background(), "nonexistent", 0)
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, ErrUnauthorized) {
		t.Errorf("expected unauthorized for unknown name in policy: got %v", err)
	}
}

func TestStaticBackend_Isolation(t *testing.T) {
	b := NewStaticBackend()
	b.Set(idA, "db", []byte("v1"))
	b.Set(idB, "db", []byte("v2"))
	v1, err := b.Fetch(context.Background(), idA, "db")
	if err != nil || string(v1) != "v1" {
		t.Errorf("idA → %q, %v", v1, err)
	}
	v2, err := b.Fetch(context.Background(), idB, "db")
	if err != nil || string(v2) != "v2" {
		t.Errorf("idB → %q, %v", v2, err)
	}
}

func TestStaticBackend_NotFound(t *testing.T) {
	b := NewStaticBackend()
	_, err := b.Fetch(context.Background(), idA, "missing")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("got %v, want ErrNotFound", err)
	}
}

func TestStaticPolicy(t *testing.T) {
	p := NewStaticPolicy()
	p.Grant(idA, "x", "y")
	if !p.Allowed(idA, "x") {
		t.Error("expected allow")
	}
	if p.Allowed(idA, "z") {
		t.Error("expected deny")
	}
	if p.Allowed(idB, "x") {
		t.Error("expected deny for other principal")
	}
}

func TestServer_ConcurrentClients(t *testing.T) {
	_, socket := startServer(t, idA, nil)
	const N = 8
	var wg sync.WaitGroup
	wg.Add(N)
	errs := make(chan error, N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			c := NewClient(socket)
			defer c.Close()
			_, err := c.Lease(context.Background(), "db-cred", 0)
			if err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Errorf("concurrent: %v", e)
	}
}
