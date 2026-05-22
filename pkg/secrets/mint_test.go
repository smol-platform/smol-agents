package secrets

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/spiffe/go-spiffe/v2/spiffeid"

	"github.com/smol-platform/smol-agents/pkg/trat"
)

type fakeVerifier struct {
	claims *trat.Claims
	err    error
}

func (f fakeVerifier) Verify(_ context.Context, compact string) (*trat.Claims, error) {
	if f.err != nil {
		return nil, f.err
	}
	c := *f.claims
	c.Compact = compact
	return &c, nil
}

type fakeDynBackend struct {
	got   CredentialRequest
	lease Lease
	err   error
}

func (b *fakeDynBackend) Mint(_ context.Context, req CredentialRequest) (Lease, error) {
	b.got = req
	if b.err != nil {
		return Lease{}, b.err
	}
	return b.lease, nil
}
func (b *fakeDynBackend) Close() error { return nil }

var mintNow = time.Date(2026, 5, 21, 0, 0, 0, 0, time.UTC)

// repoAllowListPolicy mirrors the real intent: only mint `github` when the
// TraT's rctx.repo is on the allow-list.
func repoAllowListPolicy(allow ...string) CredentialPolicy {
	set := map[string]bool{}
	for _, r := range allow {
		set[r] = true
	}
	return CredentialPolicyFunc(func(r CredentialRequest) (CredentialRequest, error) {
		if r.Name != "github" {
			return r, fmt.Errorf("%w: unknown credential %q", ErrUnauthorized, r.Name)
		}
		repo, _ := r.ReqCtx["repo"].(string)
		if !set[repo] {
			return r, fmt.Errorf("%w: repo %q not allow-listed", ErrUnauthorized, repo)
		}
		return r, nil
	})
}

func startMintServer(t *testing.T, principal spiffeid.ID, v trat.Verifier, dyn DynamicBackend, cp CredentialPolicy) string {
	t.Helper()
	// Short base dir: macOS caps unix socket paths at ~104 bytes, and
	// t.TempDir() embeds the (long) test name.
	dir, err := os.MkdirTemp("/tmp", "smolbk")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "s.sock")
	s := &Server{
		SocketPath:   socket,
		MaxLeaseTTL:  5 * time.Minute,
		DefaultTTL:   time.Minute,
		Backend:      NewStaticBackend(),
		Policy:       NewStaticPolicy(),
		Attestor:     FixedPeerAttestor{ID: principal},
		Now:          func() time.Time { return mintNow },
		Dynamic:      dyn,
		TraTVerifier: v,
		CredPolicy:   cp,
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ready := make(chan struct{})
	go func() {
		go func() {
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
	<-ready
	t.Cleanup(func() { _ = s.Close() })
	return socket
}

func TestServer_Mint_OK(t *testing.T) {
	claims := &trat.Claims{
		Subject: idA.String(), Scope: "github:repo:read",
		ReqWL: idA.String(), ReqCtx: map[string]any{"repo": "smol-platform/app"},
	}
	dyn := &fakeDynBackend{lease: Lease{Value: []byte("ghs_installation_token"), ExpiresAt: mintNow.Add(time.Hour)}}
	socket := startMintServer(t, idA, fakeVerifier{claims: claims}, dyn, repoAllowListPolicy("smol-platform/app"))

	c := NewClient(socket)
	defer c.Close()
	lease, err := c.Mint(context.Background(), "github", "compact.trat.value")
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if string(lease.Value) != "ghs_installation_token" {
		t.Errorf("value = %q", lease.Value)
	}
	// 1h provider expiry capped to the broker's 5m MaxLeaseTTL.
	if lease.TTL != 5*time.Minute {
		t.Errorf("TTL = %v, want capped 5m", lease.TTL)
	}
	if lease.Audience != idA {
		t.Errorf("audience = %s", lease.Audience)
	}
	// Backend received the verified TraT context.
	if dyn.got.Subject != idA.String() || dyn.got.Scope != "github:repo:read" ||
		dyn.got.ReqCtx["repo"] != "smol-platform/app" || dyn.got.Principal != idA {
		t.Errorf("backend got = %+v", dyn.got)
	}
}

func TestServer_Mint_InvalidTraT(t *testing.T) {
	dyn := &fakeDynBackend{lease: Lease{Value: []byte("x")}}
	socket := startMintServer(t, idA, fakeVerifier{err: errors.New("bad signature")}, dyn, repoAllowListPolicy("smol-platform/app"))
	c := NewClient(socket)
	defer c.Close()
	_, err := c.Mint(context.Background(), "github", "forged")
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("want ErrUnauthorized, got %v", err)
	}
	if dyn.got.Name != "" {
		t.Error("backend must not be called when the TraT is invalid")
	}
}

func TestServer_Mint_PolicyDeny_RepoNotAllowed(t *testing.T) {
	claims := &trat.Claims{Subject: idA.String(), Scope: "github:repo:read",
		ReqWL: idA.String(), ReqCtx: map[string]any{"repo": "evil/exfil"}}
	dyn := &fakeDynBackend{lease: Lease{Value: []byte("x")}}
	socket := startMintServer(t, idA, fakeVerifier{claims: claims}, dyn, repoAllowListPolicy("smol-platform/app"))
	c := NewClient(socket)
	defer c.Close()
	_, err := c.Mint(context.Background(), "github", "valid.but.wrong.repo")
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("want ErrUnauthorized (repo not allow-listed), got %v", err)
	}
	if dyn.got.Name != "" {
		t.Error("backend must not be called when policy denies")
	}
}

// A valid TraT minted for workload A (req_wl=idA) but presented over a
// connection attested as workload B must be rejected: the token is bound to
// its requesting workload and is not a replayable bearer token. R-SEGR-SEC-1.
func TestServer_Mint_RejectsReplayedTraT(t *testing.T) {
	claims := &trat.Claims{
		Subject: idA.String(), Scope: "github:repo:read",
		ReqWL: idA.String(), ReqCtx: map[string]any{"repo": "smol-platform/app"},
	}
	dyn := &fakeDynBackend{lease: Lease{Value: []byte("x")}}
	// Peer attests as idB, but the (otherwise valid) TraT was minted for idA.
	socket := startMintServer(t, idB, fakeVerifier{claims: claims}, dyn, repoAllowListPolicy("smol-platform/app"))
	c := NewClient(socket)
	defer c.Close()
	_, err := c.Mint(context.Background(), "github", "stolen.but.valid")
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("want ErrUnauthorized for replayed TraT, got %v", err)
	}
	if dyn.got.Name != "" {
		t.Error("backend must not be called for a TraT bound to another workload")
	}
}

func TestServer_Mint_NotConfigured(t *testing.T) {
	// No Dynamic/Verifier/CredPolicy ⇒ mint is rejected.
	socket := startMintServer(t, idA, nil, nil, nil)
	c := NewClient(socket)
	defer c.Close()
	_, err := c.Mint(context.Background(), "github", "x")
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("want ErrInvalidRequest when minting not configured, got %v", err)
	}
}
