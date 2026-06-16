package agentmodel

import (
	"context"
	"net/http"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/smol-platform/smol-agents/pkg/eventsink"
)

// resultEmittedAnnotation marks an object whose completion CloudEvent has been
// POSTed to its resultSink (wbb), so a re-reconcile of the terminal object never
// re-emits. At-least-once: a POST failure leaves it unset to retry, and the stable
// ce-id (the object UID) lets consumers dedupe a rare duplicate.
const resultEmittedAnnotation = "runtime.agents.smol-agents.ai/result-emitted"

// emitResultEventOnce POSTs ev to sink the first time an object completes (wbb —
// platform as event source), guarded by resultEmittedAnnotation on obj. No sink,
// or already emitted → no-op. Best-effort + bounded (a slow sink never stalls
// reconcile); on success it stamps the annotation via a conflict-free merge patch
// (the status write that follows is independent of this metadata patch). Shared by
// the AgentRun + AgentWorkflow result-emission paths.
func emitResultEventOnce(ctx context.Context, c client.Client, httpClient *http.Client, obj client.Object, sink string, ev eventsink.Event) {
	if sink == "" || obj.GetAnnotations()[resultEmittedAnnotation] == "true" {
		return
	}
	logger := log.FromContext(ctx)
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := eventsink.Emit(cctx, boundedSinkClient(httpClient), sink, ev); err != nil {
		// Best-effort: leave the annotation unset so the next reconcile retries.
		logger.Info("result sink emit failed (will retry)", "sink", sink, "err", err.Error())
		return
	}
	patch := client.MergeFrom(obj.DeepCopyObject().(client.Object))
	ann := obj.GetAnnotations()
	if ann == nil {
		ann = map[string]string{}
	}
	ann[resultEmittedAnnotation] = "true"
	obj.SetAnnotations(ann)
	if err := c.Patch(ctx, obj, patch); err != nil {
		// The CloudEvent was sent; a failed annotation patch only risks one
		// duplicate next reconcile (consumers dedupe on the stable ce-id).
		logger.Info("result-emitted annotation patch failed", "err", err.Error())
	}
}

// boundedSinkClient returns the bounded HTTP client for result emission: the
// injected client (tests), else a 5s-timeout default so a slow sink never stalls
// reconcile.
func boundedSinkClient(c *http.Client) *http.Client {
	if c != nil {
		return c
	}
	return &http.Client{Timeout: 5 * time.Second}
}
