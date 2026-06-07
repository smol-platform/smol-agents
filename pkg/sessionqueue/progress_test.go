package sessionqueue

import (
	"context"
	"testing"

	v1 "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
	"github.com/smol-platform/smol-agents/pkg/agentruntime"
)

// ProgressSink must structurally satisfy the executor's StepSink seam.
var _ agentruntime.StepSink = (*ProgressSink)(nil)

func TestProgressSubject(t *testing.T) {
	if got := progressSubject("tenant-a.sess1", "turn-7"); got != "agentsession.tenant-a.sess1.progress.turn-7" {
		t.Fatalf("progress subject: %q", got)
	}
}

func TestProgressSink_NilSafe(t *testing.T) {
	// A nil sink and a nil conn must not panic (Emit is best-effort).
	(*ProgressSink)(nil).Emit(context.Background(), v1.Step{})
	(&ProgressSink{}).Emit(context.Background(), v1.Step{})
}
