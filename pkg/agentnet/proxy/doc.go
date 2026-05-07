// Package proxy implements the in-Pod side of the AgentNetwork
// identity proxy: TCP byte-forwarders and HTTP reverse proxies that
// authenticate to per-resource gateways using SPIFFE.
//
//   - TCP: each Resource gets a local listener; accepted connections
//     are proxied over a SPIFFE mTLS dial to the resource's gateway.
//   - HTTP: each Resource gets a local listener that forwards via
//     httputil.ReverseProxy; every outbound request carries a fresh
//     JWT-SVID for the resource's audience.
//
// The package depends only on pkg/identity + pkg/transport so the
// existing platform-level guarantees (R-IDN, R-MTL) extend straight
// into the agent's egress path.
package proxy
