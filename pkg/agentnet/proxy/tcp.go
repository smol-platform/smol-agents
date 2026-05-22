package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"

	v1 "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
	"github.com/smol-platform/smol-agents/pkg/identity"
	"github.com/smol-platform/smol-agents/pkg/transport"
)

// TCPProxy is a one-resource byte forwarder. Implements R-AN-PROXY-1.
type TCPProxy struct {
	// Resource is the CRD entry; required.
	Resource v1.ResourceTarget

	// Identity supplies the SVID for the outbound mTLS handshake.
	Identity identity.Source

	// Metrics is optional; if non-nil, every dial is recorded.
	Metrics ProxyMetrics

	// AcceptHook is a test seam — fired for every accepted connection
	// before the proxy starts piping bytes.
	AcceptHook func()

	mu sync.Mutex
	ln net.Listener
}

// Run blocks until ctx is cancelled. Listener is opened lazily.
func (p *TCPProxy) Run(ctx context.Context) error {
	if p.Identity == nil {
		return errors.New("agentnet/proxy: TCPProxy.Identity required")
	}
	if p.Resource.Kind != "tcp" {
		return fmt.Errorf("agentnet/proxy: TCPProxy expects kind=tcp, got %q", p.Resource.Kind)
	}
	if p.Resource.LocalAddr == "" {
		return errors.New("agentnet/proxy: resource.localAddr required")
	}

	authz, err := identity.ParseAuthorizers(p.Identity.TrustDomain(), p.Resource.Authorize)
	if err != nil {
		return fmt.Errorf("agentnet/proxy: parse authorize: %w", err)
	}

	dial := transport.PrivateDialer(p.Identity, transport.PrivateConfig{
		Addr:      p.Resource.Gateway,
		Authorize: authz,
	})

	ln, err := net.Listen("tcp", p.Resource.LocalAddr)
	if err != nil {
		return fmt.Errorf("agentnet/proxy: listen %s: %w", p.Resource.LocalAddr, err)
	}
	p.mu.Lock()
	p.ln = ln
	p.mu.Unlock()

	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return fmt.Errorf("agentnet/proxy: accept: %w", err)
		}
		if p.AcceptHook != nil {
			p.AcceptHook()
		}
		go p.handle(ctx, conn, dial)
	}
}

func (p *TCPProxy) handle(ctx context.Context, local net.Conn, dial func(context.Context, string, string) (net.Conn, error)) {
	defer local.Close()
	upstream, err := dial(ctx, "tcp", p.Resource.Gateway)
	if err != nil {
		metricsOf(p.Metrics).DialError(p.Resource.Name, classify(err))
		return
	}
	defer upstream.Close()
	metricsOf(p.Metrics).DialOK(p.Resource.Name)

	// Bidirectional pipe; close both sides as soon as either errs.
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(upstream, local); done <- struct{}{} }()
	go func() { _, _ = io.Copy(local, upstream); done <- struct{}{} }()
	<-done
}

// LocalAddr returns the actual bound address (after Run() has started).
// Test helper.
func (p *TCPProxy) LocalAddr() net.Addr {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ln != nil {
		return p.ln.Addr()
	}
	return nil
}

// classify maps errors to stable Prometheus labels. Keep small.
func classify(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, context.Canceled):
		return "cancelled"
	default:
		return "dial_failed"
	}
}
