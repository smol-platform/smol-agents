// Package identity provides SPIFFE workload identity for the agent runtime.
//
// It exposes:
//   - Source: an X509Source + JWTSource pair fed from the SPIRE workload API,
//     with auto-rotation (R-IDN-1, R-IDN-2).
//   - Authorizer: SPIFFE-ID-pattern based peer authorization (R-MTL-1).
//   - Mode: insecure | permissive | strict | degraded (R-IDN-3).
//
// The package is intentionally small and side-effect free where possible.
// All blocking I/O (workload API dial, SVID rotation) is encapsulated in
// the constructor + the X509Source goroutines owned by go-spiffe.
package identity
