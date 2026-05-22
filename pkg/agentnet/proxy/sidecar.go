package proxy

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/spiffe/go-spiffe/v2/svid/jwtsvid"

	v1 "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
	"github.com/smol-platform/smol-agents/pkg/identity"
	"github.com/smol-platform/smol-agents/pkg/trat"
)

// Sidecar runs every resource defined in an IdentityProxySpec under a
// single context. One goroutine per resource. The sidecar exits when
// ctx is cancelled OR any single resource fails fatally.
type Sidecar struct {
	Spec     v1.IdentityProxySpec
	Identity identity.Source
	Metrics  ProxyMetrics

	// TraTMinter mints Txn-Tokens for TraT/Credential resources. If nil and
	// Spec.TTS is set, one is built from the TTS ref + identity. R-SEGR-MINT-1.
	TraTMinter trat.Minter
	// Broker mints provider credentials for Credential resources. Must be
	// injected by the caller (it owns the broker connection). R-SEGR-INJECT-1.
	Broker CredentialMinter
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

	// Build a TraT minter from the TTS ref if one wasn't injected. The
	// TraT audience defaults to this workload's trust domain.
	minter := s.TraTMinter
	if minter == nil && s.Spec.TTS != nil {
		minter = s.buildMinter()
	}
	traTAud := s.Identity.TrustDomain().IDString()

	var wg sync.WaitGroup
	errs := make(chan error, len(s.Spec.Resources))

	for i := range s.Spec.Resources {
		r := s.Spec.Resources[i]
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := s.runOne(ctx, r, minter, traTAud)
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

func (s *Sidecar) runOne(ctx context.Context, r v1.ResourceTarget, minter trat.Minter, traTAud string) error {
	switch r.Kind {
	case "tcp":
		return (&TCPProxy{Resource: r, Identity: s.Identity, Metrics: s.Metrics}).Run(ctx)
	case "http":
		return (&HTTPProxy{
			Resource: r, Identity: s.Identity, Metrics: s.Metrics,
			TraTMinter: minter, Broker: s.Broker, TraTAudience: traTAud,
		}).Run(ctx)
	default:
		return fmt.Errorf("unknown resource.kind=%q", r.Kind)
	}
}

// buildMinter constructs an RFC 8693 token-exchange minter from the spec's
// TTS ref. The subject token is a JWT-SVID fetched for the TTS audience.
func (s *Sidecar) buildMinter() trat.Minter {
	return &trat.ExchangeMinter{
		TokenURL:         s.Spec.TTS.URL,
		SubjectTokenType: s.Spec.TTS.SubjectTokenType,
		SubjectAudience:  s.Spec.TTS.SubjectAudience,
		SubjectToken: func(ctx context.Context, aud string) (string, error) {
			src := s.Identity.JWTSource()
			if src == nil {
				return "", errors.New("agentnet/proxy: no JWTSource for TraT subject token")
			}
			svid, err := src.FetchJWTSVID(ctx, jwtsvid.Params{Audience: aud})
			if err != nil {
				return "", err
			}
			return svid.Marshal(), nil
		},
	}
}
