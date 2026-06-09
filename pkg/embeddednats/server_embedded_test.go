//go:build embeddednats

package embeddednats_test

import (
	"context"
	"testing"
	"time"

	nats "github.com/nats-io/nats.go"

	"github.com/smol-platform/smol-agents/pkg/embeddednats"
)

// TestEmbedded_JetStreamRoundTrip proves the in-process server starts, accepts a
// real nats.go client, and serves JetStream (a KV put/get round-trip) — i.e. a
// self-host can run sessions/teams with NO separate NATS deployment.
func TestEmbedded_JetStreamRoundTrip(t *testing.T) {
	h, err := embeddednats.Start(context.Background(), embeddednats.Config{
		Host: "127.0.0.1", Port: -1, StoreDir: t.TempDir(), // -1 = random free port
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer h.Shutdown()

	nc, err := nats.Connect(h.URL, nats.Timeout(5*time.Second))
	if err != nil {
		t.Fatalf("connect %s: %v", h.URL, err)
	}
	defer nc.Close()

	js, err := nc.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	kv, err := js.CreateKeyValue(&nats.KeyValueConfig{Bucket: "embeddednats_test"})
	if err != nil {
		t.Fatalf("create kv: %v", err)
	}
	if _, err := kv.Put("k", []byte("v")); err != nil {
		t.Fatalf("put: %v", err)
	}
	e, err := kv.Get("k")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if string(e.Value()) != "v" {
		t.Fatalf("got %q, want v", e.Value())
	}
}
