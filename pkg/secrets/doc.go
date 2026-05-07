// Package secrets implements a kloak-style secret broker.
//
// The broker is a sidecar that listens on a Unix domain socket. Callers
// (co-located in the same Pod) request a secret by name; the broker:
//
//  1. Reads SO_PEERCRED to identify the calling PID.
//  2. Asks the SPIRE workload API to resolve that PID to a SPIFFE ID.
//  3. Checks the configured Policy.
//  4. Fetches the secret from a pluggable Backend.
//  5. Returns a short-lived Lease.
//
// The package implements R-SEC-1 (peer SPIFFE attestation), R-SEC-2
// (ephemeral leases with bounded TTL) and R-SEC-3 (pluggable backends).
package secrets
