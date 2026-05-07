// Package health serves Kubernetes-shaped /healthz and /readyz endpoints.
package health

import (
	"context"
	"errors"
	"net/http"
	"time"
)

// Source returns liveness/readiness signals.
type Source interface {
	Healthy() bool
	Ready() bool
}

// Server wraps an *http.Server with /healthz and /readyz.
type Server struct {
	Addr   string
	Source Source

	srv *http.Server
}

// New returns a Server bound to addr.
func New(addr string, src Source) *Server {
	if src == nil {
		panic("health: Source is required")
	}
	return &Server{Addr: addr, Source: src}
}

// Name implements runtime.Service.
func (s *Server) Name() string { return "health" }

// Start binds the listener and serves health endpoints.
func (s *Server) Start(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handle(s.Source.Healthy))
	mux.HandleFunc("/readyz", s.handle(s.Source.Ready))
	s.srv = &http.Server{
		Addr:              s.Addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		if err := s.srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			// In production, route this to the configured logger.
		}
	}()
	return nil
}

// Drain is a no-op; health endpoints are kept alive during draining
// so probes can observe NotReady.
func (s *Server) Drain(ctx context.Context) error { return nil }

func (s *Server) Stop(ctx context.Context) error {
	if s.srv == nil {
		return nil
	}
	return s.srv.Shutdown(ctx)
}

// Ready always returns true once Start succeeds; the underlying source
// is consulted in /readyz.
func (s *Server) Ready() bool { return s.srv != nil }

func (s *Server) handle(check func() bool) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		if check() {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok\n"))
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("not ready\n"))
	}
}
