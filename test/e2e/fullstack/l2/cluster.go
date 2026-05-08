//go:build e2e_l2

package l2

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"

	"github.com/stigen/knative-agents/test/e2e/fullstack/shared"
)

// Region the L2 ring is hard-pinned to. Any other value is a bug;
// the driver fails loudly per memory/aws_e2e_account.md.
const requiredRegion = "us-east-2"

// Cluster is the live L2 EC2 instance and the wiring around it.
type Cluster struct {
	InstanceID string
	PublicDNS  string
	RunID      string
	region     string
	ec2c       *ec2.Client
	ssmc       *ssm.Client
}

// Provision launches a Spot c6gd.metal in us-east-2, tags it
// `knative-agents-e2e=L2 + run-id=<random>`, waits for SSM to
// register, and returns the Cluster handle. Caller MUST defer
// Teardown — leftover instances incur Spot charges.
//
// Pre-flight refuses to start if more than 3 active L2 instances
// already exist in the account (R-E2E-L2-8).
func Provision(ctx context.Context) (*Cluster, error) {
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(requiredRegion))
	if err != nil {
		return nil, fmt.Errorf("load aws config: %w", err)
	}
	if cfg.Region != requiredRegion {
		return nil, fmt.Errorf("AWS_REGION=%q; L2 only supports %q", cfg.Region, requiredRegion)
	}

	ec2c := ec2.NewFromConfig(cfg)
	ssmc := ssm.NewFromConfig(cfg)

	// Pre-flight cap. If the sweeper Lambda is misbehaving + the
	// budget alarm hasn't fired, this catches a runaway loop.
	if n, err := countActiveL2(ctx, ec2c); err != nil {
		return nil, fmt.Errorf("preflight describe: %w", err)
	} else if n > 3 {
		return nil, fmt.Errorf("preflight: %d active L2 instances exceed cap of 3", n)
	}

	runID := randHex(6)
	distro := ResolveDistro()

	imageID, err := distro.AMI(ctx, ec2c, ssmc)
	if err != nil {
		return nil, fmt.Errorf("ami lookup (%s): %w", distro, err)
	}

	userData, err := renderCloudInit(userDataInputs{
		ArtifactBucket: os.Getenv("L2_ARTIFACT_BUCKET"),
		ECRRegistry:    os.Getenv("L2_ECR_REGISTRY"),
		ImageTag:       envOrDefault("L2_IMAGE_TAG", "dev"),
		RunID:          runID,
		Distro:         distro,
	})
	if err != nil {
		return nil, fmt.Errorf("render user-data (%s): %w", distro, err)
	}

	runReq := &ec2.RunInstancesInput{
		ImageId: aws.String(imageID),
		InstanceType: types.InstanceType(
			envOrDefault("L2_INSTANCE_TYPE", string(types.InstanceTypeC6gdMetal))),
		MinCount: aws.Int32(1),
		MaxCount: aws.Int32(1),
		IamInstanceProfile: &types.IamInstanceProfileSpecification{
			Name: aws.String("knative-agents-e2e-l2"),
		},
		UserData: aws.String(base64.StdEncoding.EncodeToString([]byte(userData))),
		TagSpecifications: []types.TagSpecification{{
			ResourceType: types.ResourceTypeInstance,
			Tags: []types.Tag{
				{Key: aws.String("knative-agents-e2e"), Value: aws.String("L2")},
				{Key: aws.String("run-id"), Value: aws.String(runID)},
				{Key: aws.String("expires-at"), Value: aws.String(rfc3339Plus(1 * time.Hour))},
				{Key: aws.String("Name"), Value: aws.String("knative-agents-e2e-l2-" + runID)},
			},
		}},
	}
	// Default to Spot. L2_INSTANCE_MARKET=on-demand opts out — useful
	// when Spot capacity is constrained for the chosen instance type.
	if envOrDefault("L2_INSTANCE_MARKET", "spot") == "spot" {
		runReq.InstanceMarketOptions = &types.InstanceMarketOptionsRequest{
			MarketType: types.MarketTypeSpot,
		}
	}
	out, err := ec2c.RunInstances(ctx, runReq)
	if err != nil {
		return nil, fmt.Errorf("run-instances: %w", err)
	}
	inst := out.Instances[0]

	// Wait for the instance to reach running state. Spot can take
	// 30-90s; allow up to 5 min before giving up.
	if err := waitInstanceRunning(ctx, ec2c, *inst.InstanceId, 5*time.Minute); err != nil {
		_ = terminate(ctx, ec2c, *inst.InstanceId)
		return nil, fmt.Errorf("wait running: %w", err)
	}

	// Refresh to pick up the assigned public DNS.
	desc, err := ec2c.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		InstanceIds: []string{*inst.InstanceId},
	})
	if err != nil {
		_ = terminate(ctx, ec2c, *inst.InstanceId)
		return nil, fmt.Errorf("describe: %w", err)
	}
	publicDNS := ""
	if len(desc.Reservations) > 0 && len(desc.Reservations[0].Instances) > 0 {
		if v := desc.Reservations[0].Instances[0].PublicDnsName; v != nil {
			publicDNS = *v
		}
	}

	// Wait for SSM to register the instance.
	if err := waitSSMReady(ctx, ssmc, *inst.InstanceId, 5*time.Minute); err != nil {
		_ = terminate(ctx, ec2c, *inst.InstanceId)
		return nil, fmt.Errorf("wait ssm-ready: %w", err)
	}

	return &Cluster{
		InstanceID: *inst.InstanceId,
		PublicDNS:  publicDNS,
		RunID:      runID,
		region:     requiredRegion,
		ec2c:       ec2c,
		ssmc:       ssmc,
	}, nil
}

// Teardown terminates the cluster's instance. Idempotent; the
// sweeper Lambda is the third belt of cleanup if this fails.
func (c *Cluster) Teardown(ctx context.Context) error {
	return terminate(ctx, c.ec2c, c.InstanceID)
}

// ----------------------------- helpers -----------------------------

func countActiveL2(ctx context.Context, c *ec2.Client) (int, error) {
	out, err := c.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		Filters: []types.Filter{
			{Name: aws.String("tag:knative-agents-e2e"), Values: []string{"L2"}},
			{Name: aws.String("instance-state-name"), Values: []string{
				"pending", "running", "stopping", "stopped",
			}},
		},
	})
	if err != nil {
		return 0, err
	}
	n := 0
	for _, r := range out.Reservations {
		n += len(r.Instances)
	}
	return n, nil
}

func waitInstanceRunning(ctx context.Context, c *ec2.Client, id string, deadline time.Duration) error {
	w := ec2.NewInstanceRunningWaiter(c, func(o *ec2.InstanceRunningWaiterOptions) {
		o.MinDelay = 5 * time.Second
		o.MaxDelay = 30 * time.Second
	})
	return w.Wait(ctx, &ec2.DescribeInstancesInput{InstanceIds: []string{id}}, deadline)
}

func waitSSMReady(ctx context.Context, c *ssm.Client, id string, deadline time.Duration) error {
	dctx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	tick := time.NewTicker(10 * time.Second)
	defer tick.Stop()
	for {
		out, err := c.DescribeInstanceInformation(dctx, &ssm.DescribeInstanceInformationInput{
			Filters: []ssmtypes.InstanceInformationStringFilter{{
				Key: aws.String("InstanceIds"), Values: []string{id},
			}},
		})
		if err == nil && len(out.InstanceInformationList) > 0 {
			info := out.InstanceInformationList[0]
			if info.PingStatus == ssmtypes.PingStatusOnline {
				return nil
			}
		}
		select {
		case <-dctx.Done():
			return fmt.Errorf("ssm not ready within %s", deadline)
		case <-tick.C:
		}
	}
}

func terminate(ctx context.Context, c *ec2.Client, id string) error {
	_, err := c.TerminateInstances(ctx, &ec2.TerminateInstancesInput{
		InstanceIds: []string{id},
	})
	return err
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func rfc3339Plus(d time.Duration) string {
	return time.Now().Add(d).UTC().Format(time.RFC3339)
}

// minimalUserData returns a cloud-init that drops a sentinel file.
// Used as a fallback when the full cloud-init template renders blank
// (e.g., L2_BOOTSTRAP_MINIMAL=1 for fast smoke verifying provision +
// SSM ping without the full k0s + Kata bring-up).
func minimalUserData() string {
	var b bytes.Buffer
	b.WriteString("#cloud-config\n")
	b.WriteString("runcmd:\n")
	b.WriteString("  - touch /var/log/l2-bootstrap.READY\n")
	return b.String()
}

// userDataInputs are the template variables consumed by every
// distro's user-data template. Distro-specific templates may use
// only a subset; that's fine.
type userDataInputs struct {
	ArtifactBucket string
	ECRRegistry    string
	ImageTag       string
	RunID          string
	Distro         Distro
}

// renderCloudInit dispatches to the per-distro user-data renderer.
// Returns the minimal stub if L2_BOOTSTRAP_MINIMAL=1 (smoke-only,
// validates provision + SSM ping without the full bring-up).
func renderCloudInit(in userDataInputs) (string, error) {
	if os.Getenv("L2_BOOTSTRAP_MINIMAL") == "1" {
		return minimalUserData(), nil
	}
	if in.Distro == "" {
		in.Distro = ResolveDistro()
	}
	return in.Distro.UserData(in)
}

// repoFile resolves a path relative to the project root.
func repoFile(rel string) string {
	_, here, _, _ := runtime.Caller(0)
	dir := filepath.Dir(here)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return filepath.Join(dir, rel)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return rel
		}
		dir = parent
	}
}

// ensureRegion is a sanity check used by the test setup. Returns
// nil if AWS_REGION matches the pinned region (or is unset, which
// the SDK treats as "use config-default-which-is-also-pinned").
func ensureRegion() error {
	if r := os.Getenv("AWS_REGION"); r != "" && r != requiredRegion {
		return fmt.Errorf("AWS_REGION=%q; L2 only supports %q", r, requiredRegion)
	}
	return nil
}

// envOrDefault returns os.Getenv(k) or fallback when empty.
func envOrDefault(k, fallback string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return fallback
}

// silence imports if a future refactor drops one.
var (
	_ = strings.HasPrefix
	_ = net.ResolveTCPAddr
	_ shared.Caps
)
