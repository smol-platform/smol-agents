package trat

import (
	"errors"
	"time"
)

// Errors callers can branch on.
var (
	ErrExchange = errors.New("trat: token exchange failed")
	ErrVerify   = errors.New("trat: verification failed")
)

// RFC 8693 + Transaction Token constants.
const (
	GrantTokenExchange = "urn:ietf:params:oauth:grant-type:token-exchange"
	TokenTypeTxn       = "urn:ietf:params:oauth:token-type:txn_token"
	TokenTypeJWT       = "urn:ietf:params:oauth:token-type:jwt"

	// Header is the HTTP header that conveys a TraT between workloads
	// (IETF Transaction Tokens; exactly one value).
	Header = "Txn-Token"
)

// ExchangeParams are the per-resource inputs to a TraT mint.
type ExchangeParams struct {
	Scope    string // RFC 8693 scope — the transaction intent
	Audience string // trust domain (TraT aud)
}

// Claims is the subset of TraT claims the platform consumes after
// verification. Compact holds the raw JWT so it can be forwarded verbatim.
type Claims struct {
	Subject  string         // sub — the requesting principal (the agent)
	Audience string         // aud — trust domain
	Scope    string         // scope — transaction intent
	TxnID    string         // txn — transaction id (audit correlation)
	ReqWL    string         // req_wl — requesting workload
	ReqCtx   map[string]any // rctx — environmental/request context
	TxnCtx   map[string]any // tctx — immutable call-chain context
	Expiry   time.Time
	IssuedAt time.Time
	Compact  string
}
