//go:build e2e_l2

package l2

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"text/template"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
)

// Distro is a host OS family the L2 ring knows how to provision.
// Each distro produces the same end-state contract: a single-node
// k0s control plane reachable via SSM, with our manifests applied
// and the /var/log/l2-bootstrap.{READY,FAILED} sentinel written.
//
// The bootstrap PATH differs per distro (cloud-init for AL2023 /
// Ubuntu / Flatcar-via-compat, TOML user-data + bootstrap-container
// for Bottlerocket); the contract does not.
type Distro string

const (
	DistroAL2023             Distro = "al2023"
	DistroUbuntu             Distro = "ubuntu"
	DistroBottlerocket       Distro = "bottlerocket"
	DistroFlatcar            Distro = "flatcar"
	DistroBottlerocketWorker Distro = "bottlerocket-worker"
)

// ResolveDistro reads L2_DISTRO from the environment; default
// is al2023 for backwards compatibility with the original
// single-distro test.
func ResolveDistro() Distro {
	d := strings.ToLower(strings.TrimSpace(os.Getenv("L2_DISTRO")))
	switch Distro(d) {
	case DistroUbuntu, DistroBottlerocket, DistroFlatcar, DistroBottlerocketWorker:
		return Distro(d)
	default:
		return DistroAL2023
	}
}

// AMI resolves a current arm64 image ID for the distro. Each lookup
// path is the canonical one for that distro:
//   - AL2023:       SSM Parameter Store (Amazon-published)
//   - Bottlerocket: SSM Parameter Store (general-purpose variant)
//   - Ubuntu:       EC2 image search (Canonical owner ID)
//   - Flatcar:      EC2 image search (Kinvolk/Microsoft owner ID)
func (d Distro) AMI(ctx context.Context, ec2c *ec2.Client, ssmc *ssm.Client) (string, error) {
	switch d {
	case DistroAL2023:
		// Pin to kernel-6.12 AMI: kata-fc + nydus-snapshotter
		// (blockdev/tarfs mode) needs EROFS tarfs support which only
		// landed in 6.4. The default kernel image still ships 6.1.
		return ssmAMI(ctx, ssmc,
			"/aws/service/ami-amazon-linux-latest/al2023-ami-kernel-6.12-arm64")
	case DistroBottlerocket:
		// general-purpose ECS variant. We tried aws-k8s-1.31 for
		// the kubeadm bootstrap pattern but pluto's EC2 probe
		// blocks boot at 5 min on every non-EKS instance. See
		// scripts/aws-l2/bottlerocket-bootstrap/README.md for
		// the full multi-approach investigation.
		return ssmAMI(ctx, ssmc,
			"/aws/service/bottlerocket/aws-ecs-1/arm64/latest/image_id")
	case DistroBottlerocketWorker:
		// kubelet-on-host variant. Joins an external k0s control
		// plane via [settings.kubernetes] in user-data — see
		// scripts/aws-l2/bottlerocket-worker.toml.tmpl.
		return ssmAMI(ctx, ssmc,
			"/aws/service/bottlerocket/aws-k8s-1.31/arm64/latest/image_id")
	case DistroUbuntu:
		return findAMI(ctx, ec2c, "099720109477", // Canonical
			"ubuntu/images/hvm-ssd-gp3/ubuntu-noble-24.04-arm64-server-*")
	case DistroFlatcar:
		return findAMI(ctx, ec2c, "075585003325", // Kinvolk/Flatcar
			"Flatcar-stable-*-arm64-hvm")
	}
	return "", fmt.Errorf("unknown distro %q", d)
}

func ssmAMI(ctx context.Context, ssmc *ssm.Client, name string) (string, error) {
	out, err := ssmc.GetParameter(ctx, &ssm.GetParameterInput{
		Name: aws.String(name),
	})
	if err != nil {
		return "", fmt.Errorf("ssm parameter %s: %w", name, err)
	}
	return *out.Parameter.Value, nil
}

func findAMI(ctx context.Context, ec2c *ec2.Client, ownerID, namePattern string) (string, error) {
	out, err := ec2c.DescribeImages(ctx, &ec2.DescribeImagesInput{
		Owners: []string{ownerID},
		Filters: []ec2types.Filter{
			{Name: aws.String("name"), Values: []string{namePattern}},
			{Name: aws.String("architecture"), Values: []string{"arm64"}},
			{Name: aws.String("state"), Values: []string{"available"}},
		},
	})
	if err != nil {
		return "", fmt.Errorf("describe images %s/%s: %w", ownerID, namePattern, err)
	}
	if len(out.Images) == 0 {
		return "", fmt.Errorf("no AMIs match %s/%s", ownerID, namePattern)
	}
	// Pick the most recent by CreationDate (lexicographic-comparable).
	var newest ec2types.Image
	for _, img := range out.Images {
		if newest.CreationDate == nil ||
			aws.ToString(img.CreationDate) > aws.ToString(newest.CreationDate) {
			newest = img
		}
	}
	return aws.ToString(newest.ImageId), nil
}

// UserData renders the distro-specific bootstrap input. Returns the
// raw text the caller will base64-encode into RunInstances.UserData.
//
// Flatcar's template is Butane (YAML for Ignition); we Go-template
// it, then shell out to `butane` to compile to Ignition JSON.
//
// Bottlerocket's bootstrap-container reads its config from a
// base64-encoded user-data env; we precompute that so the template
// can interpolate it as a single value.
func (d Distro) UserData(in userDataInputs) (string, error) {
	if d == DistroBottlerocket && in.BottlerocketBootstrapUserData == "" {
		body := fmt.Sprintf("ARTIFACT_BUCKET=%s\nECR_REGISTRY=%s\nIMAGE_TAG=%s\nRUN_ID=%s\n",
			in.ArtifactBucket, in.ECRRegistry, in.ImageTag, in.RunID)
		in.BottlerocketBootstrapUserData = base64.StdEncoding.EncodeToString([]byte(body))
	}

	tmplPath := repoFile("scripts/aws-l2/" + d.userDataFilename())
	raw, err := os.ReadFile(tmplPath)
	if err != nil {
		return "", fmt.Errorf("read %s template: %w", d, err)
	}
	t, err := template.New(string(d)).Parse(string(raw))
	if err != nil {
		return "", fmt.Errorf("parse %s template: %w", d, err)
	}
	var sb strings.Builder
	if err := t.Execute(&sb, in); err != nil {
		return "", fmt.Errorf("execute %s template: %w", d, err)
	}
	if d == DistroFlatcar {
		return butaneCompile(sb.String())
	}
	return sb.String(), nil
}

func (d Distro) userDataFilename() string {
	switch d {
	case DistroAL2023:
		return "cloud-init-al2023.yaml.tmpl"
	case DistroUbuntu:
		return "cloud-init-ubuntu.yaml.tmpl"
	case DistroBottlerocket:
		return "bottlerocket-userdata.toml.tmpl"
	case DistroBottlerocketWorker:
		return "bottlerocket-worker.toml.tmpl"
	case DistroFlatcar:
		return "flatcar.bu.tmpl"
	}
	return ""
}

// butaneCompile pipes a rendered Butane YAML through the `butane`
// CLI and returns the Ignition JSON. Requires `butane` on PATH.
// Install: go install github.com/coreos/butane/internal/cmd/butane,
// or download the prebuilt binary from coreos/butane releases.
func butaneCompile(butaneSrc string) (string, error) {
	bin, err := exec.LookPath("butane")
	if err != nil {
		return "", fmt.Errorf("butane not found on PATH (install: "+
			"https://github.com/coreos/butane/releases): %w", err)
	}
	cmd := exec.Command(bin, "--strict")
	cmd.Stdin = strings.NewReader(butaneSrc)
	var out, errBuf strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("butane compile: %w\nstderr: %s", err, errBuf.String())
	}
	return out.String(), nil
}

// HealthGateMaxWait is how long the driver waits for the
// /var/log/l2-bootstrap.{READY,FAILED} sentinel. Bottlerocket and
// Flatcar bootstraps include a container pull as their first step
// so they're slower than apt/dnf-based AL2023 + Ubuntu.
func (d Distro) HealthGateMaxWait() string {
	switch d {
	case DistroBottlerocket, DistroFlatcar:
		return "12m"
	default:
		return "8m"
	}
}
