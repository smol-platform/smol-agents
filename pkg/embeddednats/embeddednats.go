// Package embeddednats optionally runs an in-process NATS + JetStream server
// inside the operator pod, so a lightweight self-host install needs NO separate
// NATS deployment to run AgentSessions/AgentTeams (7fr.7).
//
// The heavyweight nats-server dependency is only compiled in under the
// `embeddednats` build tag (mirroring agentnet/wireguard's `wgnetstack`
// pattern), so the default operator binary stays lean and the dep is opt-in at
// build time. Without the tag, Start returns ErrNotWired.
//
// Topology note: for the gateway / session workers (separate pods) to reach the
// embedded server, expose the operator's NATS port via a Service and point
// --session-nats-url at it. Co-located workers can dial the loopback URL.
package embeddednats

import (
	"context"
	"errors"
)

// Config configures the embedded server.
type Config struct {
	// Host is the listen host. "127.0.0.1" for operator-local use; "0.0.0.0"
	// when other pods reach it through a Service.
	Host string
	// Port is the client listen port (NATS default 4222).
	Port int
	// StoreDir is the JetStream file-store directory (a PVC mount). Empty = an
	// in-memory store (non-durable).
	StoreDir string
}

// Handle is a running embedded server.
type Handle struct {
	// URL is the nats:// client URL to dial (and to set as --session-nats-url
	// for co-located clients).
	URL string
	// Shutdown stops the server and waits for it to drain.
	Shutdown func()
}

// ErrNotWired means the embedded-server implementation isn't linked in this
// build. Build with -tags=embeddednats to include it.
var ErrNotWired = errors.New("embeddednats: not wired (build with -tags=embeddednats)")

// startFn is installed by server_embedded.go's init() under the embeddednats tag.
var startFn func(ctx context.Context, cfg Config) (*Handle, error)

// Wired reports whether the embedded server implementation is linked in.
func Wired() bool { return startFn != nil }

// Start runs an in-process NATS + JetStream server. Returns ErrNotWired unless
// the binary was built with -tags=embeddednats.
func Start(ctx context.Context, cfg Config) (*Handle, error) {
	if startFn == nil {
		return nil, ErrNotWired
	}
	return startFn(ctx, cfg)
}
