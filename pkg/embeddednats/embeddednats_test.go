package embeddednats_test

import (
	"context"
	"errors"
	"testing"

	"github.com/smol-platform/smol-agents/pkg/embeddednats"
)

// TestStart_NotWiredWithoutTag verifies the default (untagged) build returns
// ErrNotWired — the heavy nats-server dep is excluded unless -tags=embeddednats.
func TestStart_NotWiredWithoutTag(t *testing.T) {
	if embeddednats.Wired() {
		t.Skip("built with -tags=embeddednats; embedded server is wired")
	}
	if _, err := embeddednats.Start(context.Background(), embeddednats.Config{}); !errors.Is(err, embeddednats.ErrNotWired) {
		t.Fatalf("want ErrNotWired, got %v", err)
	}
}
