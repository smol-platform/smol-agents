package agentbench

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	amv1 "github.com/smol-platform/smol-agents/operator/api/agentmodel/v1"
	pure "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

// runDriver creates an AgentRun CR per sample and polls its .status to terminal.
type runDriver struct {
	client client.Client
	// pollInterval between status reads; pollTimeout caps a single Collect.
	pollInterval time.Duration
	pollTimeout  time.Duration
	// owner, when set, is added as an ownerReference so the run is GC'd with the
	// namespace/fleet.
	owner *metav1.OwnerReference
}

// NewRunDriver builds a runDriver with sensible defaults.
func NewRunDriver(c client.Client, owner *metav1.OwnerReference) *runDriver {
	return &runDriver{
		client:       c,
		pollInterval: 2 * time.Second,
		pollTimeout:  15 * time.Minute,
		owner:        owner,
	}
}

func (d *runDriver) Kind() DriverKind { return DriverRun }

func (d *runDriver) Submit(ctx context.Context, c BenchCase, ns, prompt string, sampleIdx int) (Handle, error) {
	name := fmt.Sprintf("%s-%d", c.Metadata.Name, sampleIdx)
	input, err := json.Marshal(map[string]string{"prompt": prompt})
	if err != nil {
		return Handle{}, fmt.Errorf("run: marshal input: %w", err)
	}
	run := &amv1.AgentRun{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: pure.AgentRunSpec{
			AgentRef: c.AgentRef,
			Seed:     c.Seed,
			Input:    input,
		},
	}
	if d.owner != nil {
		run.OwnerReferences = []metav1.OwnerReference{*d.owner}
	}
	if err := d.client.Create(ctx, run); err != nil {
		return Handle{}, fmt.Errorf("run: create AgentRun %s/%s: %w", ns, name, err)
	}
	return Handle{Name: name, Namespace: ns, Cold: sampleIdx == 0}, nil
}

func (d *runDriver) Collect(ctx context.Context, h Handle) (pure.RunStatus, error) {
	deadline := time.Now().Add(d.pollTimeout)
	tick := time.NewTicker(d.pollInterval)
	defer tick.Stop()
	for {
		var run amv1.AgentRun
		err := d.client.Get(ctx, client.ObjectKey{Namespace: h.Namespace, Name: h.Name}, &run)
		if err != nil {
			return pure.RunStatus{}, fmt.Errorf("run: get %s/%s: %w", h.Namespace, h.Name, err)
		}
		if run.Status.State.Terminal() {
			return run.Status, nil
		}
		if time.Now().After(deadline) {
			return run.Status, fmt.Errorf("run: %s/%s did not reach a terminal state within %s (last state %q)",
				h.Namespace, h.Name, d.pollTimeout, run.Status.State)
		}
		select {
		case <-ctx.Done():
			return pure.RunStatus{}, ctx.Err()
		case <-tick.C:
		}
	}
}
