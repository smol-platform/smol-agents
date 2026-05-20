package proxy

import (
	"context"
	"errors"
	"fmt"
	"sync"

	v1 "github.com/stigen/smol-agents/pkg/agentmodel/v1"
	"github.com/stigen/smol-agents/pkg/identity"
)

// Sidecar runs every resource defined in an IdentityProxySpec under a
// single context. One goroutine per resource. The sidecar exits when
// ctx is cancelled OR any single resource fails fatally.
type Sidecar struct {
	Spec     v1.IdentityProxySpec
	Identity identity.Source
	Metrics  ProxyMetrics
}

// Run blocks until ctx is cancelled. Returns the first fatal error
// from any resource (or nil if shutdown was clean).
func (s *Sidecar) Run(ctx context.Context) error {
	if s.Identity == nil {
		return errors.New("agentnet/proxy: Sidecar.Identity required")
	}
	if len(s.Spec.Resources) == 0 {
		return errors.New("agentnet/proxy: no resources configured")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var wg sync.WaitGroup
	errs := make(chan error, len(s.Spec.Resources))

	for i := range s.Spec.Resources {
		r := s.Spec.Resources[i]
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := s.runOne(ctx, r)
			if err != nil {
				errs <- fmt.Errorf("resource %s: %w", r.Name, err)
				cancel()
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		return e
	}
	return nil
}

func (s *Sidecar) runOne(ctx context.Context, r v1.ResourceTarget) error {
	switch r.Kind {
	case "tcp":
		return (&TCPProxy{Resource: r, Identity: s.Identity, Metrics: s.Metrics}).Run(ctx)
	case "http":
		return (&HTTPProxy{Resource: r, Identity: s.Identity, Metrics: s.Metrics}).Run(ctx)
	default:
		return fmt.Errorf("unknown resource.kind=%q", r.Kind)
	}
}
