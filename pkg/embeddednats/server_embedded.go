//go:build embeddednats

// The real embedded NATS + JetStream server. Compiled only under
// -tags=embeddednats so the heavyweight nats-server dependency stays out of the
// default operator binary.
package embeddednats

import (
	"context"
	"fmt"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
)

func init() { startFn = startEmbedded }

func startEmbedded(_ context.Context, cfg Config) (*Handle, error) {
	host := cfg.Host
	if host == "" {
		host = "127.0.0.1"
	}
	port := cfg.Port
	if port == 0 {
		port = 4222
	}
	opts := &natsserver.Options{
		Host:      host,
		Port:      port,
		JetStream: true,
		StoreDir:  cfg.StoreDir, // empty => in-memory JetStore
	}
	ns, err := natsserver.NewServer(opts)
	if err != nil {
		return nil, fmt.Errorf("embeddednats: new server: %w", err)
	}
	go ns.Start()
	if !ns.ReadyForConnections(10 * time.Second) {
		ns.Shutdown()
		return nil, fmt.Errorf("embeddednats: server not ready within 10s")
	}
	return &Handle{
		URL: ns.ClientURL(),
		Shutdown: func() {
			ns.Shutdown()
			ns.WaitForShutdown()
		},
	}, nil
}
