// Package hetzner implements `agentctl deploy --target=hetzner`: provision a
// Hetzner Cloud server, bootstrap k0s + kata, then run the shared platform
// install against the joined cluster.
//
// V1 status: stub — flag surface wired, provisioning not implemented. Once
// enabled this target will use github.com/hetznercloud/hcloud-go.
package hetzner

import (
	"context"
	"fmt"
	"os"

	"github.com/smol-platform/smol-agents/internal/agentctl/deploy"
)

type Target struct{}

func New() *Target           { return &Target{} }
func (*Target) Name() string { return "hetzner" }

func (*Target) Validate(o *deploy.Options) error {
	if o.Hetzner.TokenEnv == "" {
		o.Hetzner.TokenEnv = "HCLOUD_TOKEN"
	}
	if os.Getenv(o.Hetzner.TokenEnv) == "" {
		return fmt.Errorf("env var %s is not set (Hetzner Cloud API token)", o.Hetzner.TokenEnv)
	}
	if o.Hetzner.Location == "" {
		return fmt.Errorf("--hcloud-location is required (e.g., nbg1, hel1)")
	}
	if o.Hetzner.ServerType == "" {
		return fmt.Errorf("--server-type is required (e.g., cax21, ccx33)")
	}
	return nil
}

func (*Target) Deploy(_ context.Context, o *deploy.Options) error {
	fmt.Fprintf(o.Common.Out, "==> target=hetzner (location=%s type=%s)\n",
		o.Hetzner.Location, o.Hetzner.ServerType)
	return fmt.Errorf("hetzner target is not yet implemented; the hcloud-go SDK is the integration path")
}

func (*Target) Teardown(_ context.Context, _ *deploy.Options) error {
	return fmt.Errorf("hetzner target is not yet implemented")
}
