package identity

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/svid/x509svid"
	"github.com/spiffe/go-spiffe/v2/workloadapi"
)

// ErrBootTimeout indicates the workload API failed to deliver an SVID
// within the configured boot timeout.
var ErrBootTimeout = errors.New("identity: boot timeout waiting for SVID")

// Source aggregates an X509Source and a JWTSource backed by the SPIRE
// workload API. Implements R-IDN-1 and R-IDN-2.
//
// The interface is intentionally narrow so callers (transport, secrets) can
// be tested with fakes.
type Source interface {
	// X509Source returns a live X.509 SVID source. The returned source
	// auto-rotates; callers should not cache its results.
	X509Source() *workloadapi.X509Source

	// JWTSource returns a live JWT-SVID source. R-IDN-2.
	JWTSource() *workloadapi.JWTSource

	// TrustDomain returns the trust domain extracted from the latest SVID.
	TrustDomain() spiffeid.TrustDomain

	// Mode returns the current operating mode (may be Degraded if the
	// workload API has been unreachable).
	Mode() Mode

	// Close shuts down the workload API client and releases resources.
	Close() error
}

// SourceConfig configures Open.
type SourceConfig struct {
	WorkloadAPIAddr   string        // unix:// path, required
	BootTimeout       time.Duration // R-IDN-1 acceptance #1
	Mode              Mode
	RotationThreshold float64 // 0..1; informational, used for telemetry
	TrustDomain       string  // optional, validated against received SVIDs
}

// realSource is the production implementation backed by go-spiffe.
type realSource struct {
	x509 *workloadapi.X509Source
	jwt  *workloadapi.JWTSource
	td   spiffeid.TrustDomain
	mode atomic.Value // Mode
}

// Open dials the workload API, blocks until an initial SVID arrives or
// BootTimeout elapses, and returns a Source that will auto-rotate.
//
// Implements R-IDN-1 acceptance #1: blocks until an SVID has been received
// or the bounded timeout elapses.
func Open(ctx context.Context, cfg SourceConfig) (Source, error) {
	if cfg.WorkloadAPIAddr == "" {
		return nil, errors.New("identity: WorkloadAPIAddr is required")
	}
	if cfg.BootTimeout <= 0 {
		cfg.BootTimeout = 30 * time.Second
	}
	if !cfg.Mode.Valid() || cfg.Mode == ModeDegraded {
		return nil, fmt.Errorf("identity: invalid configured mode %q", cfg.Mode)
	}

	bootCtx, cancel := context.WithTimeout(ctx, cfg.BootTimeout)
	defer cancel()

	clientOpt := workloadapi.WithClientOptions(
		workloadapi.WithAddr(cfg.WorkloadAPIAddr),
	)

	x509src, err := workloadapi.NewX509Source(bootCtx, clientOpt)
	if err != nil {
		if errors.Is(bootCtx.Err(), context.DeadlineExceeded) {
			return nil, ErrBootTimeout
		}
		return nil, fmt.Errorf("identity: open X509Source: %w", err)
	}

	jwtsrc, err := workloadapi.NewJWTSource(bootCtx, clientOpt)
	if err != nil {
		_ = x509src.Close()
		if errors.Is(bootCtx.Err(), context.DeadlineExceeded) {
			return nil, ErrBootTimeout
		}
		return nil, fmt.Errorf("identity: open JWTSource: %w", err)
	}

	svid, err := x509src.GetX509SVID()
	if err != nil {
		_ = x509src.Close()
		_ = jwtsrc.Close()
		return nil, fmt.Errorf("identity: GetX509SVID: %w", err)
	}
	td := svid.ID.TrustDomain()
	if cfg.TrustDomain != "" && td.String() != cfg.TrustDomain {
		_ = x509src.Close()
		_ = jwtsrc.Close()
		return nil, fmt.Errorf("identity: trust domain mismatch: got %q want %q", td, cfg.TrustDomain)
	}

	s := &realSource{x509: x509src, jwt: jwtsrc, td: td}
	s.mode.Store(cfg.Mode)
	return s, nil
}

func (s *realSource) X509Source() *workloadapi.X509Source { return s.x509 }
func (s *realSource) JWTSource() *workloadapi.JWTSource   { return s.jwt }
func (s *realSource) TrustDomain() spiffeid.TrustDomain   { return s.td }
func (s *realSource) Mode() Mode                          { return s.mode.Load().(Mode) }

// SetDegraded marks the source degraded (workload API down).
// Used by the health watcher; not exposed in the Source interface so callers
// cannot fake it.
func (s *realSource) SetDegraded() { s.mode.Store(ModeDegraded) }

// SetMode is for tests only; production callers should not call this.
func (s *realSource) SetMode(m Mode) { s.mode.Store(m) }

func (s *realSource) Close() error {
	var first error
	if err := s.x509.Close(); err != nil && first == nil {
		first = err
	}
	if err := s.jwt.Close(); err != nil && first == nil {
		first = err
	}
	return first
}

// CurrentSVID returns the SVID currently cached by the source. It is a
// thin convenience wrapper used by callers that want to log identity.
func CurrentSVID(s Source) (*x509svid.SVID, error) {
	return s.X509Source().GetX509SVID()
}
