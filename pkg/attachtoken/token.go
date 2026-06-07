// Package attachtoken mints and verifies the short-TTL bearer tokens the
// terminal attach gateway (cmd/agentterminal, M4.10) issues after it resolves an
// AttachGrant (M4.6). A token is AUDIENCE-BOUND to exactly one agent's terminal
// (aud=spiffe://<trust-domain>/<ns>/<name>/terminal), so a token minted for one
// agent cannot be replayed against another — Verify rejects an audience
// mismatch. Tokens are HMAC-SHA256 signed with an operator/gateway key (no
// external JWT dependency); they carry the human subject + role (viewer|driver)
// the proxy enforces.
package attachtoken

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Role is the attach capability a token grants.
type Role string

const (
	RoleViewer Role = "viewer" // read-only attach (no keystrokes reach the PTY)
	RoleDriver Role = "driver" // read/write attach (drives the terminal)
)

// Valid reports whether r is a known role.
func (r Role) Valid() bool { return r == RoleViewer || r == RoleDriver }

// Claims are the verified contents of an attach token.
type Claims struct {
	Subject  string `json:"sub"` // human identity (OIDC subject / email)
	Role     Role   `json:"role"`
	Audience string `json:"aud"`            // spiffe://<td>/<ns>/<name>/terminal
	AgentRef string `json:"agent"`          // <ns>/<name> the grant is for
	Expiry   int64  `json:"exp"`            // unix seconds; hard cap
	GrantID  string `json:"gid,omitempty"`  // AttachGrant name (audit correlation)
	CastID   string `json:"cast,omitempty"` // recording id (audit correlation)
}

// Audience returns the canonical, agent-scoped audience string. Binding a token
// to this value is what stops cross-agent replay.
func Audience(trustDomain, namespace, name string) string {
	return fmt.Sprintf("spiffe://%s/%s/%s/terminal", trustDomain, namespace, name)
}

var (
	// ErrMalformed is returned when a token is not the expected payload.sig shape.
	ErrMalformed = errors.New("attachtoken: malformed token")
	// ErrSignature is returned when the HMAC does not verify.
	ErrSignature = errors.New("attachtoken: bad signature")
	// ErrExpired is returned when the token's exp is at/after now.
	ErrExpired = errors.New("attachtoken: expired")
	// ErrAudience is returned when the token's aud != the expected audience —
	// i.e. a token minted for a different agent (replay).
	ErrAudience = errors.New("attachtoken: audience mismatch")
	// ErrRole is returned when the token carries an unknown role.
	ErrRole = errors.New("attachtoken: invalid role")
)

// Mint signs claims into a compact "<payload>.<sig>" token. key is the shared
// HMAC secret. The caller sets Expiry (unix seconds); Mint does not invent time
// (so it stays deterministic and testable).
func Mint(key []byte, c Claims) (string, error) {
	if !c.Role.Valid() {
		return "", ErrRole
	}
	if c.Audience == "" || c.Subject == "" {
		return "", errors.New("attachtoken: subject and audience are required")
	}
	body, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	payload := base64.RawURLEncoding.EncodeToString(body)
	return payload + "." + sign(key, payload), nil
}

// Verify checks the signature, expiry (against nowUnix), and audience, returning
// the claims only when all hold. wantAudience is the audience of the agent the
// request is attaching to — a token minted for another agent fails ErrAudience.
func Verify(key []byte, token, wantAudience string, nowUnix int64) (Claims, error) {
	payload, sig, ok := strings.Cut(token, ".")
	if !ok || payload == "" || sig == "" {
		return Claims{}, ErrMalformed
	}
	if !hmac.Equal([]byte(sig), []byte(sign(key, payload))) {
		return Claims{}, ErrSignature
	}
	body, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return Claims{}, ErrMalformed
	}
	var c Claims
	if err := json.Unmarshal(body, &c); err != nil {
		return Claims{}, ErrMalformed
	}
	if !c.Role.Valid() {
		return Claims{}, ErrRole
	}
	if c.Expiry != 0 && nowUnix >= c.Expiry {
		return Claims{}, ErrExpired
	}
	if c.Audience != wantAudience {
		return Claims{}, ErrAudience
	}
	return c, nil
}

func sign(key []byte, payload string) string {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}
