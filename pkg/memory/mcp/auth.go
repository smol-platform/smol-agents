package mcp

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/spiffe/go-spiffe/v2/bundle/jwtbundle"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/svid/jwtsvid"
)

// CallerIdentity holds the gateway-derived identity fields for one request.
// It is built from a validated JWT-SVID and is never derived from caller-
// supplied input fields (tenant injection: R-MEM-AUTH-1, R-MEM-SEC-1).
type CallerIdentity struct {
	// SPIFFEID is the full SPIFFE URI, e.g. "spiffe://td/ns/agents/sa/coder".
	SPIFFEID string

	// Tenant is derived from the SPIFFE path component: the segment following
	// "/ns/" up to the next "/". For example, "spiffe://td/ns/team-alpha/..."
	// yields tenant "team-alpha".
	Tenant string

	// TrustDomain is the SPIFFE trust domain, e.g. "stigen.ai".
	TrustDomain string
}

// JWTBundleSource is satisfied by workloadapi.JWTSource and by test fakes.
// It lets the gateway validate JWT-SVIDs without coupling to the full identity
// Source interface.
type JWTBundleSource interface {
	// GetJWTBundleForTrustDomain returns the JWT bundle for validation.
	GetJWTBundleForTrustDomain(trustDomain spiffeid.TrustDomain) (*jwtbundle.Bundle, error)
}

// AuthConfig controls how the gateway validates inbound JWT-SVIDs.
type AuthConfig struct {
	// BundleSource is used for verified JWT-SVID validation.
	// When nil, ParseInsecure is used (for tests / insecure mode).
	BundleSource JWTBundleSource

	// Audience is the expected JWT audience (the gateway's own SPIFFE ID
	// or a well-known URI). Empty means any audience is accepted.
	Audience string

	// TrustDomain is the expected trust domain. Empty means any.
	TrustDomain string
}

// ExtractIdentity reads the Authorization header, validates the JWT-SVID, and
// derives the CallerIdentity. Returns a typed memory error on any failure so
// the caller can audit and return the appropriate MCP error.
func (cfg AuthConfig) ExtractIdentity(r *http.Request) (CallerIdentity, error) {
	raw := r.Header.Get("Authorization")
	if raw == "" {
		return CallerIdentity{}, fmt.Errorf("missing Authorization header")
	}
	if !strings.HasPrefix(raw, "Bearer ") {
		return CallerIdentity{}, fmt.Errorf("Authorization header must be Bearer <jwt>")
	}
	token := strings.TrimPrefix(raw, "Bearer ")

	var (
		svid *jwtsvid.SVID
		err  error
	)

	auds := []string{}
	if cfg.Audience != "" {
		auds = []string{cfg.Audience}
	}

	if cfg.BundleSource != nil {
		svid, err = jwtsvid.ParseAndValidate(token, cfg.BundleSource, auds)
	} else {
		// Insecure / test mode: skip signature verification.
		svid, err = jwtsvid.ParseInsecure(token, auds)
	}
	if err != nil {
		return CallerIdentity{}, fmt.Errorf("invalid JWT-SVID: %w", err)
	}

	// Validate trust domain if configured.
	if cfg.TrustDomain != "" {
		want, tdErr := spiffeid.TrustDomainFromString(cfg.TrustDomain)
		if tdErr != nil {
			return CallerIdentity{}, fmt.Errorf("server trust-domain config invalid: %w", tdErr)
		}
		if !svid.ID.MemberOf(want) {
			return CallerIdentity{}, fmt.Errorf("caller %s is not in trust domain %s", svid.ID, want)
		}
	}

	tenant, err := tenantFromSPIFFEPath(svid.ID.Path())
	if err != nil {
		return CallerIdentity{}, fmt.Errorf("cannot derive tenant from SPIFFE path %q: %w", svid.ID.Path(), err)
	}

	return CallerIdentity{
		SPIFFEID:    svid.ID.String(),
		Tenant:      tenant,
		TrustDomain: svid.ID.TrustDomain().String(),
	}, nil
}

// tenantFromSPIFFEPath extracts the tenant segment from a SPIFFE path.
// The convention followed by this platform is:
//
//	/ns/<tenant>/…
//
// Examples:
//
//	/ns/team-alpha/sa/coder    → "team-alpha"
//	/ns/default                → "default"
//	/agents/ns/team-beta/…     → "team-beta"  (agentnet path variant)
//
// Returns an error if no "/ns/" segment is found.
func tenantFromSPIFFEPath(path string) (string, error) {
	// Normalise: ensure leading slash.
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}

	const marker = "/ns/"
	idx := strings.Index(path, marker)
	if idx < 0 {
		return "", fmt.Errorf("path %q has no /ns/ segment", path)
	}

	rest := path[idx+len(marker):]
	if rest == "" {
		return "", fmt.Errorf("path %q: /ns/ segment is empty", path)
	}

	// The tenant is the next path component (up to the next "/" or end).
	slash := strings.Index(rest, "/")
	if slash < 0 {
		return rest, nil
	}
	return rest[:slash], nil
}

// validateCallerTenant checks that the caller-supplied tenant (if any) matches
// the attested tenant. Returns a PermissionDenied error on mismatch so the
// gateway can fail-closed. R-MEM-AUTH-1.
func validateCallerTenant(callerSupplied, attested string) error {
	if callerSupplied == "" || callerSupplied == attested {
		return nil
	}
	return fmt.Errorf("caller-supplied tenant %q does not match attested tenant %q", callerSupplied, attested)
}

// insecureTestIdentity builds a CallerIdentity without JWT validation for
// tests that use a synthesized SPIFFE ID string directly.
func insecureTestIdentity(rawSPIFFEID string) (CallerIdentity, error) {
	id, err := spiffeid.FromString(rawSPIFFEID)
	if err != nil {
		return CallerIdentity{}, err
	}
	tenant, err := tenantFromSPIFFEPath(id.Path())
	if err != nil {
		return CallerIdentity{}, err
	}
	return CallerIdentity{
		SPIFFEID:    rawSPIFFEID,
		Tenant:      tenant,
		TrustDomain: id.TrustDomain().String(),
	}, nil
}

// tokenAge returns how long ago a JWT-SVID was issued. Used for observability.
func tokenAge(svid *jwtsvid.SVID) time.Duration {
	iat, ok := svid.Claims["iat"]
	if !ok {
		return 0
	}
	switch v := iat.(type) {
	case float64:
		return time.Since(time.Unix(int64(v), 0))
	}
	return 0
}

// compile-time check that the SVID type we reference is valid.
var _ = (*jwtsvid.SVID)(nil)
var _ = tokenAge
