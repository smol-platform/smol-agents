// Package transport provides two-rail mTLS listeners and dialers.
//
//   - Private (in-mesh): both peers present SPIFFE-X.509-SVIDs. Authorizes
//     by SPIFFE ID. Implements R-MTL-1.
//   - Public  (gateway-fronted): server presents a chain from a public CA;
//     optionally pinned to a server SPIFFE ID for client-side trust pinning.
//     Implements R-MTL-2.
//
// All listeners expose the peer SPIFFE ID via PeerID(ctx).
package transport
