// Package ebpf provides the agent's eBPF runtime.
//
// On Linux it loads CO-RE BPF programs via cilium/ebpf, manages ring buffers,
// and exposes events on a typed EventBus. On non-Linux platforms it
// compiles to a no-op implementation so unit tests can run anywhere.
//
// Implements R-EBP-1 (CO-RE program loading) and R-EBP-2 (ring-buffer
// event delivery).
package ebpf
