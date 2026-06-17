package v1

import (
	"errors"
	"fmt"
	"strings"
)

// DynamicCredentialBackendSpec declares a platform-owned dynamic credential
// backend (D8): a root secret — broker-only, agent-blind — that mints
// short-lived, request-scoped provider credentials, plus the per-Agent grants
// authorized to mint. It is namespaced platform infrastructure (recommend a
// RBAC-locked platform-secrets namespace); per-Agent authorization is the grant
// list. This is the declarative surface over the existing pkg/secrets mint path
// (GitHubAppBackend + StaticCredentialPolicy); the inline-Go API is unchanged.
type DynamicCredentialBackendSpec struct {
	// CredentialName MUST match the AgentNetwork resources[].credential.name that
	// consumes this backend.
	CredentialName string `json:"credentialName"`

	// Provider selects the backend implementation. Only "githubApp" exists today.
	// +kubebuilder:validation:Enum=githubApp
	Provider string `json:"provider"`

	// GitHubApp is the provider config; required when provider=githubApp.
	// +optional
	GitHubApp *GitHubAppBackendSpec `json:"githubApp,omitempty"`

	// MaxLeaseTTL caps a minted lease's lifetime (e.g. "5m"); the broker caps it
	// to its own hard MaxLeaseTTL. Empty = the broker default. +optional
	MaxLeaseTTL string `json:"maxLeaseTTL,omitempty"`

	// Grants authorize specific Agent principals to mint a scope. +optional
	Grants []CredentialGrantSpec `json:"grants,omitempty"`
}

// GitHubAppBackendSpec configures a GitHub App installation-token backend: the
// root key reference (broker-only) + the scope→permissions map.
type GitHubAppBackendSpec struct {
	AppID string `json:"appID"`

	// PrivateKeyRef is the ROOT secret (the App private key). It is mounted into
	// the SPIRE-backed broker only — never the agent.
	PrivateKeyRef AuthRef `json:"privateKeyRef"`

	// BaseURL overrides the GitHub API base for GHES. +optional
	BaseURL string `json:"baseURL,omitempty"`

	// ScopePermissions maps a TraT scope (e.g. "github:repo:read") to the
	// installation-token permissions it grants (e.g. {"contents":"read"}).
	// +optional
	ScopePermissions map[string]map[string]string `json:"scopePermissions,omitempty"`
}

// CredentialGrantSpec authorizes one Agent principal to mint one scope,
// optionally constrained to a repo allow-list (the request's rctx.repo).
type CredentialGrantSpec struct {
	// Principal is the Agent's SPIFFE ID (spiffe://<trust-domain>/<path>).
	Principal string `json:"principal"`

	// Scope is the credential scope this principal may mint (a key of
	// GitHubApp.ScopePermissions).
	Scope string `json:"scope"`

	// Repos restricts rctx.repo; empty = any repo the App can reach. +optional
	Repos []string `json:"repos,omitempty"`
}

// DynamicCredentialBackendStatus is the observed backend state.
type DynamicCredentialBackendStatus struct {
	// Phase: Ready | Pending | Failed.
	// +optional
	Phase string `json:"phase,omitempty"`
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// GrantCount is the number of authorized grants.
	// +optional
	GrantCount int `json:"grantCount,omitempty"`
	// +optional
	Reason string `json:"reason,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`
}

// ValidateDynamicCredentialBackend enforces the spec invariants (D8): a
// credentialName, a known provider with its matching config block, and
// well-formed grants (a parseable SPIFFE principal + a non-empty scope).
func ValidateDynamicCredentialBackend(s DynamicCredentialBackendSpec) error {
	var errs []error
	if strings.TrimSpace(s.CredentialName) == "" {
		errs = append(errs, errors.New("dynamicCredentialBackend.credentialName is required"))
	}
	switch s.Provider {
	case "githubApp":
		if s.GitHubApp == nil {
			errs = append(errs, errors.New("dynamicCredentialBackend.githubApp is required when provider=githubApp"))
		} else {
			errs = append(errs, validateGitHubAppBackend(*s.GitHubApp)...)
		}
	case "":
		errs = append(errs, errors.New("dynamicCredentialBackend.provider is required"))
	default:
		errs = append(errs, fmt.Errorf("dynamicCredentialBackend.provider=%q is invalid (only githubApp today)", s.Provider))
	}
	for i, g := range s.Grants {
		if err := validateCredentialGrant(g); err != nil {
			errs = append(errs, fmt.Errorf("dynamicCredentialBackend.grants[%d]: %w", i, err))
		}
	}
	// Four-string alignment (c5r.22): a grant must authorize a scope the backend
	// actually maps to permissions. A grant.scope absent from
	// githubApp.scopePermissions mints an empty/invalid token at runtime — the
	// misalignment that crash-loops the broker sidecar. Reject it at admission,
	// naming the offending scope, instead of failing on the mint path.
	if s.Provider == "githubApp" && s.GitHubApp != nil {
		for i, g := range s.Grants {
			scope := strings.TrimSpace(g.Scope)
			if scope == "" {
				continue // already flagged by validateCredentialGrant
			}
			if _, ok := s.GitHubApp.ScopePermissions[scope]; !ok {
				errs = append(errs, fmt.Errorf("dynamicCredentialBackend.grants[%d].scope %q "+
					"is not a key of githubApp.scopePermissions (no permission mapping — the mint would fail)", i, scope))
			}
		}
	}
	return errors.Join(errs...)
}

func validateGitHubAppBackend(g GitHubAppBackendSpec) []error {
	var errs []error
	if strings.TrimSpace(g.AppID) == "" {
		errs = append(errs, errors.New("githubApp.appID is required"))
	}
	if strings.TrimSpace(g.PrivateKeyRef.SecretName) == "" {
		errs = append(errs, errors.New("githubApp.privateKeyRef.secretName is required"))
	}
	return errs
}

func validateCredentialGrant(g CredentialGrantSpec) error {
	if !isSPIFFEID(g.Principal) {
		return fmt.Errorf("principal %q is not a valid SPIFFE ID (spiffe://<trust-domain>/<path>)", g.Principal)
	}
	if strings.TrimSpace(g.Scope) == "" {
		return errors.New("scope is required")
	}
	return nil
}

// isSPIFFEID does a lightweight structural check — the pure package avoids a
// go-spiffe dependency: spiffe://<non-empty trust domain>/<non-empty path>.
func isSPIFFEID(id string) bool {
	rest, ok := strings.CutPrefix(id, "spiffe://")
	if !ok {
		return false
	}
	td, path, ok := strings.Cut(rest, "/")
	return ok && td != "" && path != ""
}
