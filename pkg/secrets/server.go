package secrets

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/spiffe/go-spiffe/v2/spiffeid"

	"github.com/smol-platform/smol-agents/pkg/trat"
)

// Server is the broker process serving Lease requests over a UDS.
//
// Implements R-SEC-1, R-SEC-2, R-SEC-3.
type Server struct {
	SocketPath  string
	MaxLeaseTTL time.Duration
	DefaultTTL  time.Duration
	Backend     Backend
	Policy      Policy
	Attestor    PeerAttestor
	Logger      *slog.Logger
	Now         func() time.Time

	// Dynamic + TraTVerifier + CredPolicy enable the dynamic provider-
	// credential mint path (R-SEGR). If any is nil, reqMint is rejected.
	Dynamic      DynamicBackend
	TraTVerifier trat.Verifier
	CredPolicy   CredentialPolicy

	mu    sync.Mutex
	ln    net.Listener
	conns map[*serverConn]struct{}
	// issued tracks non-expired leases so refreshes can be validated.
	issued map[string]Lease
}

type serverConn struct {
	id   string
	conn net.Conn
}

// Listen begins serving on s.SocketPath. It is safe to call once per Server.
func (s *Server) Listen(ctx context.Context) error {
	if s.SocketPath == "" {
		return errors.New("secrets: SocketPath is required")
	}
	if s.Backend == nil {
		return errors.New("secrets: Backend is required")
	}
	if s.Policy == nil {
		return errors.New("secrets: Policy is required")
	}
	if s.Attestor == nil {
		return errors.New("secrets: Attestor is required")
	}
	if s.MaxLeaseTTL <= 0 {
		s.MaxLeaseTTL = MaxLeaseTTL
	}
	if s.MaxLeaseTTL > MaxLeaseTTL {
		s.MaxLeaseTTL = MaxLeaseTTL
	}
	if s.DefaultTTL <= 0 || s.DefaultTTL > s.MaxLeaseTTL {
		s.DefaultTTL = s.MaxLeaseTTL
	}
	if s.Logger == nil {
		s.Logger = slog.Default()
	}
	if s.Now == nil {
		s.Now = time.Now
	}
	if s.conns == nil {
		s.conns = make(map[*serverConn]struct{})
	}
	if s.issued == nil {
		s.issued = make(map[string]Lease)
	}

	if err := os.MkdirAll(filepath.Dir(s.SocketPath), 0o750); err != nil {
		return fmt.Errorf("secrets: mkdir: %w", err)
	}
	_ = os.Remove(s.SocketPath)
	ln, err := net.Listen("unix", s.SocketPath)
	if err != nil {
		return fmt.Errorf("secrets: listen: %w", err)
	}
	if err := os.Chmod(s.SocketPath, 0o660); err != nil {
		_ = ln.Close()
		return fmt.Errorf("secrets: chmod: %w", err)
	}

	s.mu.Lock()
	s.ln = ln
	s.mu.Unlock()

	go s.gcLoop(ctx)

	return s.acceptLoop(ctx)
}

func (s *Server) acceptLoop(ctx context.Context) error {
	// Capture the listener once; Close will close it under lock.
	s.mu.Lock()
	ln := s.ln
	s.mu.Unlock()
	if ln == nil {
		return errors.New("secrets: not listening")
	}
	go func() {
		<-ctx.Done()
		_ = s.Close()
	}()
	for {
		c, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return fmt.Errorf("secrets: accept: %w", err)
		}
		go s.handle(ctx, c)
	}
}

func (s *Server) handle(ctx context.Context, c net.Conn) {
	sc := &serverConn{id: newConnID(), conn: c}
	s.trackConn(sc, true)
	defer func() {
		s.trackConn(sc, false)
		_ = c.Close()
	}()

	// Authenticate peer once.
	id, err := s.Attestor.Attest(ctx, c)
	if err != nil {
		s.Logger.Warn("peer attestation failed", "err", err, "conn", sc.id)
		_ = writeFrame(c, response{ErrorCode: errorCodeFor(ErrUnauthorized), ErrorMessage: err.Error()})
		return
	}

	for {
		var req request
		if err := readFrame(c, &req); err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
				s.Logger.Debug("read frame", "err", err, "conn", sc.id)
			}
			return
		}
		resp := s.dispatch(ctx, id, req)
		if err := writeFrame(c, resp); err != nil {
			s.Logger.Debug("write frame", "err", err, "conn", sc.id)
			return
		}
		if req.Kind == reqClose {
			return
		}
	}
}

func (s *Server) dispatch(ctx context.Context, principal spiffeid.ID, req request) response {
	switch req.Kind {
	case reqLease:
		return s.handleLease(ctx, principal, req)
	case reqRefresh:
		return s.handleRefresh(ctx, principal, req)
	case reqMint:
		return s.handleMint(ctx, principal, req)
	case reqClose:
		return response{}
	default:
		return errResponse(ErrInvalidRequest, fmt.Sprintf("unknown kind %q", req.Kind))
	}
}

func (s *Server) handleLease(ctx context.Context, principal spiffeid.ID, req request) response {
	if req.Name == "" {
		return errResponse(ErrInvalidRequest, "name is required")
	}
	if !s.Policy.Allowed(principal, req.Name) {
		s.Logger.Warn("policy denied", "principal", principal, "name", req.Name)
		return errResponse(ErrUnauthorized, fmt.Sprintf("%s not allowed for %s", principal, req.Name))
	}
	ttl := req.TTL
	if ttl <= 0 {
		ttl = s.DefaultTTL
	}
	if ttl > s.MaxLeaseTTL {
		return errResponse(ErrTTLExceeded, fmt.Sprintf("requested %s > max %s", ttl, s.MaxLeaseTTL))
	}

	value, err := s.Backend.Fetch(ctx, principal, req.Name)
	if err != nil {
		s.Logger.Warn("backend fetch failed", "principal", principal, "name", req.Name, "err", err)
		return errResponseWrap(err)
	}
	now := s.Now()
	l := Lease{
		Name:      req.Name,
		Value:     value,
		Issued:    now,
		ExpiresAt: now.Add(ttl),
		Audience:  principal,
		TTL:       ttl,
	}
	s.recordLease(l)
	return response{Lease: &l}
}

func (s *Server) handleRefresh(ctx context.Context, principal spiffeid.ID, req request) response {
	s.mu.Lock()
	prev, ok := s.issued[req.Lease]
	s.mu.Unlock()
	if !ok {
		return errResponse(ErrLeaseExpired, "no such lease")
	}
	if prev.Audience != principal {
		return errResponse(ErrUnauthorized, "lease belongs to another principal")
	}
	if !prev.Valid(s.Now()) {
		return errResponse(ErrLeaseExpired, "lease expired")
	}
	// Re-validate policy + re-fetch backend so a newly-revoked policy
	// blocks future refreshes (R-SEC-2 #2).
	return s.handleLease(ctx, principal, request{Kind: reqLease, Name: prev.Name, TTL: prev.TTL})
}

// handleMint mints a dynamic provider credential (R-SEGR). It (1) requires the
// dynamic path to be configured, (2) verifies the TraT signature + aud/exp via
// the TTS JWKS, (3) lets the CredentialPolicy authorize + narrow the request
// from the verified claims, then (4) calls the DynamicBackend. The minted value
// is returned to the calling sidecar (which injects it) — never logged.
func (s *Server) handleMint(ctx context.Context, principal spiffeid.ID, req request) response {
	if s.Dynamic == nil || s.TraTVerifier == nil || s.CredPolicy == nil {
		return errResponse(ErrInvalidRequest, "dynamic credential minting not configured")
	}
	if req.Name == "" {
		return errResponse(ErrInvalidRequest, "credential name is required")
	}
	if req.TraT == "" {
		return errResponse(ErrUnauthorized, "trat is required for mint")
	}
	claims, err := s.TraTVerifier.Verify(ctx, req.TraT)
	if err != nil {
		s.Logger.Warn("trat verification failed", "principal", principal, "credential", req.Name, "err", err)
		return errResponse(ErrUnauthorized, "trat invalid")
	}
	// Sender-constraint: req_wl binds the TraT to the workload it was minted
	// for. Enforce it against the SO_PEERCRED-attested peer so a TraT minted
	// for workload A cannot be replayed by workload B (a bearer TraT would
	// otherwise be mintable by anyone whose policy overlaps). Fail closed if
	// the binding is absent. R-SEGR-AUTH-1 / R-SEGR-SEC-1.
	if claims.ReqWL == "" || claims.ReqWL != principal.String() {
		s.Logger.Warn("trat not bound to caller", "principal", principal, "req_wl", claims.ReqWL, "credential", req.Name)
		return errResponse(ErrUnauthorized, "trat not bound to caller")
	}
	cr := CredentialRequest{
		Name:      req.Name,
		Principal: principal,
		Subject:   claims.Subject,
		Scope:     claims.Scope,
		ReqWL:     claims.ReqWL,
		ReqCtx:    claims.ReqCtx,
	}
	cr, err = s.CredPolicy.AuthorizeMint(cr)
	if err != nil {
		s.Logger.Warn("mint policy denied", "principal", principal, "credential", req.Name, "scope", claims.Scope, "err", err)
		return errResponse(ErrUnauthorized, err.Error())
	}
	lease, err := s.Dynamic.Mint(ctx, cr)
	if err != nil {
		s.Logger.Warn("mint failed", "principal", principal, "credential", req.Name, "err", err)
		return errResponseWrap(err)
	}
	now := s.Now()
	if lease.Issued.IsZero() {
		lease.Issued = now
	}
	if lease.ExpiresAt.IsZero() || lease.ExpiresAt.After(now.Add(s.MaxLeaseTTL)) {
		lease.ExpiresAt = now.Add(s.MaxLeaseTTL)
	}
	lease.Name = req.Name
	lease.Audience = principal
	lease.TTL = lease.ExpiresAt.Sub(lease.Issued)
	s.recordLease(lease)
	return response{Lease: &lease}
}

func errResponse(err error, msg string) response {
	return response{ErrorCode: errorCodeFor(err), ErrorMessage: msg}
}

func errResponseWrap(err error) response {
	return response{ErrorCode: errorCodeFor(err), ErrorMessage: err.Error()}
}

func (s *Server) trackConn(c *serverConn, add bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if add {
		s.conns[c] = struct{}{}
	} else {
		delete(s.conns, c)
	}
}

func (s *Server) recordLease(l Lease) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := l.Audience.String() + "|" + l.Name + "|" + l.Issued.UTC().Format(time.RFC3339Nano)
	s.issued[key] = l
}

func (s *Server) gcLoop(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.gcExpired()
		}
	}
}

func (s *Server) gcExpired() {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.Now()
	for k, l := range s.issued {
		if !l.Valid(now) {
			delete(s.issued, k)
		}
	}
}

// Close stops the listener and closes all in-flight connections.
func (s *Server) Close() error {
	s.mu.Lock()
	ln := s.ln
	// Snapshot the connection set under lock; iterating the live map
	// would race with concurrent trackConn writers.
	conns := make([]*serverConn, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	s.ln = nil
	s.mu.Unlock()
	if ln != nil {
		_ = ln.Close()
	}
	for _, c := range conns {
		_ = c.conn.Close()
	}
	if s.Backend != nil {
		_ = s.Backend.Close()
	}
	return nil
}

func newConnID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
