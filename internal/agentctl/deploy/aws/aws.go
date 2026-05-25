// Package aws implements `agentctl deploy --target=aws`: provision an EC2 host
// (VPC + SG + IAM + EBS), bootstrap k0s + kata via cloud-init, then run the
// shared platform install against the joined cluster.
//
// V1 status: stub — the flag surface is wired so the UX is settled, but
// provisioning is not implemented yet. Once enabled this target will use the
// AWS SDK for Go v2 and the hardened cloud-init recipes under scripts/aws-l2/.
package aws

import (
	"context"
	"fmt"

	"github.com/smol-platform/smol-agents/internal/agentctl/deploy"
)

type Target struct{}

func New() *Target           { return &Target{} }
func (*Target) Name() string { return "aws" }

func (*Target) Validate(o *deploy.Options) error {
	if o.AWS.Profile == "" {
		return fmt.Errorf("--aws-profile is required (or set $AWS_PROFILE)")
	}
	if o.AWS.Region == "" {
		return fmt.Errorf("--aws-region is required (or set $AWS_REGION)")
	}
	if o.AWS.InstanceType == "" {
		return fmt.Errorf("--instance-type is required (use a *.metal type for kata-fc)")
	}
	return nil
}

func (*Target) Deploy(_ context.Context, o *deploy.Options) error {
	fmt.Fprintf(o.Common.Out, "==> target=aws (profile=%s region=%s instance=%s)\n",
		o.AWS.Profile, o.AWS.Region, o.AWS.InstanceType)
	return fmt.Errorf("aws target is not yet implemented; V1 lands the k8s target first. " +
		"The hardened cloud-init at scripts/aws-l2/cloud-init-al2023.yaml.tmpl + " +
		"infra/terraform/aws-e2e are the reuse targets")
}

func (*Target) Teardown(_ context.Context, _ *deploy.Options) error {
	return fmt.Errorf("aws target is not yet implemented")
}
