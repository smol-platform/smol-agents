// Package sandbox describes the user-space sandbox under which agents run.
//
// The default RuntimeClass is "gvisor" (R-SBX-1). The package itself does
// not start a sandbox — it lives at the Kubernetes layer via RuntimeClass —
// but it offers a small abstraction so other runtimes (Kata, wasm) can plug
// in by changing one Spec value.
package sandbox
