package identity

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/spiffetls/tlsconfig"
	"github.com/spiffe/go-spiffe/v2/svid/x509svid"
)

// Authorizer accepts or rejects a peer presenting an SVID.
// Implements R-MTL-1 acceptance #1.
type Authorizer interface {
	// Authorize returns nil iff the peer is allowed.
	Authorize(svid *x509svid.SVID) error

	// AsAuthorizer returns the go-spiffe authorizer used by tlsconfig helpers.
	AsAuthorizer() tlsconfig.Authorizer
}

// AuthorizeAny matches any valid SPIFFE ID in the trust domain.
type AuthorizeAny struct {
	TrustDomain spiffeid.TrustDomain
}

func (a AuthorizeAny) Authorize(svid *x509svid.SVID) error {
	if !svid.ID.MemberOf(a.TrustDomain) {
		return fmt.Errorf("identity: peer %s is not in trust domain %s", svid.ID, a.TrustDomain)
	}
	return nil
}

func (a AuthorizeAny) AsAuthorizer() tlsconfig.Authorizer {
	return tlsconfig.AuthorizeMemberOf(a.TrustDomain)
}

// AuthorizeIDs matches an explicit set of SPIFFE IDs.
type AuthorizeIDs struct {
	IDs []spiffeid.ID
}

func (a AuthorizeIDs) Authorize(svid *x509svid.SVID) error {
	for _, id := range a.IDs {
		if id == svid.ID {
			return nil
		}
	}
	return fmt.Errorf("identity: peer %s not in allowed set", svid.ID)
}

func (a AuthorizeIDs) AsAuthorizer() tlsconfig.Authorizer {
	return tlsconfig.AuthorizeOneOf(a.IDs...)
}

// AuthorizePathPrefix matches any ID whose path begins with Prefix.
// Useful for allowing all agents in a namespace e.g. spiffe://stigen.ai/ns/agents.
type AuthorizePathPrefix struct {
	TrustDomain spiffeid.TrustDomain
	Prefix      string
}

func (a AuthorizePathPrefix) Authorize(svid *x509svid.SVID) error {
	if !svid.ID.MemberOf(a.TrustDomain) {
		return fmt.Errorf("identity: peer %s outside trust domain %s", svid.ID, a.TrustDomain)
	}
	if !strings.HasPrefix(svid.ID.Path(), a.Prefix) {
		return fmt.Errorf("identity: peer path %q lacks prefix %q", svid.ID.Path(), a.Prefix)
	}
	return nil
}

func (a AuthorizePathPrefix) AsAuthorizer() tlsconfig.Authorizer {
	return tlsconfig.AdaptMatcher(func(id spiffeid.ID) error {
		if !id.MemberOf(a.TrustDomain) {
			return fmt.Errorf("peer %s outside trust domain %s", id, a.TrustDomain)
		}
		if !strings.HasPrefix(id.Path(), a.Prefix) {
			return fmt.Errorf("peer path %q lacks prefix %q", id.Path(), a.Prefix)
		}
		return nil
	})
}

// ParseAuthorizer parses a single authorizer descriptor. Supported forms:
//
//   - "spiffe://td/path"           — exact ID match
//   - "prefix:spiffe://td/ns/foo"  — path-prefix match
//   - "any:spiffe://td"            — any member of the trust domain
func ParseAuthorizer(td spiffeid.TrustDomain, descriptor string) (Authorizer, error) {
	switch {
	case strings.HasPrefix(descriptor, "any:"):
		t, err := spiffeid.TrustDomainFromString(strings.TrimPrefix(descriptor, "any:"))
		if err != nil {
			return nil, fmt.Errorf("identity: parse any: %w", err)
		}
		return AuthorizeAny{TrustDomain: t}, nil
	case strings.HasPrefix(descriptor, "prefix:"):
		raw := strings.TrimPrefix(descriptor, "prefix:")
		id, err := spiffeid.FromString(raw)
		if err != nil {
			return nil, fmt.Errorf("identity: parse prefix id: %w", err)
		}
		return AuthorizePathPrefix{TrustDomain: id.TrustDomain(), Prefix: id.Path()}, nil
	case strings.HasPrefix(descriptor, "spiffe://"):
		id, err := spiffeid.FromString(descriptor)
		if err != nil {
			return nil, fmt.Errorf("identity: parse spiffe id: %w", err)
		}
		return AuthorizeIDs{IDs: []spiffeid.ID{id}}, nil
	default:
		return nil, fmt.Errorf("identity: unrecognized authorizer descriptor %q", descriptor)
	}
}

// ParseAuthorizers turns a slice of descriptors into a single authorizer
// that matches if any underlying authorizer matches.
func ParseAuthorizers(td spiffeid.TrustDomain, descriptors []string) (Authorizer, error) {
	if len(descriptors) == 0 {
		return AuthorizeAny{TrustDomain: td}, nil
	}
	auths := make([]Authorizer, 0, len(descriptors))
	for _, d := range descriptors {
		a, err := ParseAuthorizer(td, d)
		if err != nil {
			return nil, err
		}
		auths = append(auths, a)
	}
	return composite{auths: auths}, nil
}

type composite struct{ auths []Authorizer }

func (c composite) Authorize(svid *x509svid.SVID) error {
	var errs []error
	for _, a := range c.auths {
		if err := a.Authorize(svid); err == nil {
			return nil
		} else {
			errs = append(errs, err)
		}
	}
	return fmt.Errorf("identity: no authorizer matched: %w", errors.Join(errs...))
}

func (c composite) AsAuthorizer() tlsconfig.Authorizer {
	return tlsconfig.AdaptMatcher(func(id spiffeid.ID) error {
		// Wrap as a pseudo-SVID; we only inspect ID.
		// AdaptMatcher operates on the id only.
		fakeSVID := &x509svid.SVID{ID: id}
		return c.Authorize(fakeSVID)
	})
}
