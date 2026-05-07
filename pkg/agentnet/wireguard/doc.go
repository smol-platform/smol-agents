// Package wireguard is the userspace WireGuard adapter for the
// AgentNetwork wireguardMesh transport.
//
// The adapter runs WireGuard entirely in userspace (no kernel
// module) using:
//
//	golang.zx2c4.com/wireguard          – the WireGuard core
//	golang.zx2c4.com/wireguard/tun/netstack — gVisor-backed TUN
//
// This is what makes WireGuard usable inside a Kata-FC microVM —
// the agent process owns the device, and there's no privileged
// network setup on the host.
//
// Two modes:
//
//	client  — joins an existing WireGuard hub by registering a static
//	          peer list with endpoints.
//	server  — listens for inbound peers; useful for letting external
//	          tunnels reach the agent without a public hub.
//
// Implements R-AN-WG-1..4.
package wireguard
