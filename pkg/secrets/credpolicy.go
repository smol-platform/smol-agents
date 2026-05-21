package secrets

import (
	"fmt"

	"github.com/spiffe/go-spiffe/v2/spiffeid"
)

// CredentialGrant is one entry in a StaticCredentialPolicy: a (principal,
// scope) is allowed to mint Credential, optionally constrained to a set of
// repositories the TraT's rctx.repo must fall within.
type CredentialGrant struct {
	Credential string              // allowed credential name (e.g. "github")
	Repos      map[string]struct{} // allow-listed rctx.repo values; empty = unconstrained
}

// StaticCredentialPolicy is a deny-by-default (principal → scope → grant) map.
// It authorizes a mint and validates the TraT's rctx.repo against the grant's
// allow-list so rctx cannot request an arbitrary repo. Implements
// R-SEGR-API-2 / R-SEGR-AUTH-1.
type StaticCredentialPolicy struct {
	grants map[string]map[string]CredentialGrant // principal → scope → grant
}

func NewStaticCredentialPolicy() *StaticCredentialPolicy {
	return &StaticCredentialPolicy{grants: map[string]map[string]CredentialGrant{}}
}

// Grant allows `id` with TraT `scope` to mint `credential`, constrained to
// `repos` (empty = any repo).
func (p *StaticCredentialPolicy) Grant(id spiffeid.ID, scope, credential string, repos ...string) {
	key := id.String()
	if p.grants[key] == nil {
		p.grants[key] = map[string]CredentialGrant{}
	}
	set := make(map[string]struct{}, len(repos))
	for _, r := range repos {
		set[r] = struct{}{}
	}
	p.grants[key][scope] = CredentialGrant{Credential: credential, Repos: set}
}

func (p *StaticCredentialPolicy) AuthorizeMint(r CredentialRequest) (CredentialRequest, error) {
	byScope, ok := p.grants[r.Principal.String()]
	if !ok {
		return r, fmt.Errorf("%w: principal %s has no credential grants", ErrUnauthorized, r.Principal)
	}
	g, ok := byScope[r.Scope]
	if !ok {
		return r, fmt.Errorf("%w: scope %q not granted for %s", ErrUnauthorized, r.Scope, r.Principal)
	}
	if g.Credential != r.Name {
		return r, fmt.Errorf("%w: credential %q not allowed for scope %q", ErrUnauthorized, r.Name, r.Scope)
	}
	if len(g.Repos) > 0 {
		repo, _ := r.ReqCtx["repo"].(string)
		if _, ok := g.Repos[repo]; !ok {
			return r, fmt.Errorf("%w: repo %q not allow-listed for scope %q", ErrUnauthorized, repo, r.Scope)
		}
	}
	return r, nil
}
