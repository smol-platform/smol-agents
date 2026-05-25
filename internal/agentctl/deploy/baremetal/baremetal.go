// Package baremetal implements `agentctl deploy --target=baremetal`: SSH to a
// pre-provisioned host, install k0s + kata, then run the shared platform
// install against the resulting single-node cluster — the "edge" path.
//
// V1 status: stub — flag surface wired, provisioning not implemented. Once
// enabled this target will use golang.org/x/crypto/ssh for the bootstrap and
// the k0s recipe in docs/runbooks/k0s-local-cluster.md.
package baremetal

import (
	"context"
	"fmt"

	"github.com/smol-platform/smol-agents/internal/agentctl/deploy"
)

type Target struct{}

func New() *Target           { return &Target{} }
func (*Target) Name() string { return "baremetal" }

func (*Target) Validate(o *deploy.Options) error {
	if o.BareMetal.Host == "" {
		return fmt.Errorf("--ssh-host is required (host[:port])")
	}
	if o.BareMetal.KeyPath == "" {
		return fmt.Errorf("--ssh-key is required (path to private key)")
	}
	if o.BareMetal.User == "" {
		o.BareMetal.User = "root"
	}
	return nil
}

func (*Target) Deploy(_ context.Context, o *deploy.Options) error {
	fmt.Fprintf(o.Common.Out, "==> target=baremetal (host=%s user=%s)\n",
		o.BareMetal.Host, o.BareMetal.User)
	return fmt.Errorf("baremetal target is not yet implemented; the k0s recipe in docs/runbooks/k0s-local-cluster.md is the bootstrap source")
}

func (*Target) Teardown(_ context.Context, _ *deploy.Options) error {
	return fmt.Errorf("baremetal target is not yet implemented")
}
