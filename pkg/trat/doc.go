// Package trat mints and verifies Tokenetes Transaction Tokens (TraTs).
//
// A TraT is a short-lived JWT (typ "txntoken+jwt") that carries a verified
// statement of who is acting (sub), to what end (scope), and in what context
// (rctx/tctx) within a single trust domain. The platform uses TraTs two ways:
//
//   - Egress injection: the agentnet sidecar mints a TraT from the agent's
//     SPIFFE JWT-SVID (RFC 8693 token-exchange against the Tokenetes Service)
//     and injects it as the "Txn-Token" header on egress.
//   - Authorization context: the secret broker verifies a TraT and uses its
//     sub/scope/rctx to authorize + scope a minted provider credential
//     (smol-agents-secretless-egress).
//
// Minting (ExchangeMinter) and verification (JWKSVerifier) are split so the
// proxy mints and the broker verifies, each with a narrow dependency.
package trat
