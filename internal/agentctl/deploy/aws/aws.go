// Package aws implements `agentctl deploy --target=aws`: provision an EC2 host
// + security group, bootstrap a single-node k0s with cloud-init, then delegate
// to the k8s target to install the operator stack against the fetched
// kubeconfig.
//
// Resources are tagged `agentctl-deployment=<name>` for tag-based idempotency:
// re-running deploy reuses an existing instance + SG with the same tag; teardown
// finds and removes them. Default networking uses the account's default VPC
// (first subnet); pass --subnet-id to pin a specific one.
package aws

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"

	awsv2 "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"

	"github.com/smol-platform/smol-agents/internal/agentctl/deploy"
	"github.com/smol-platform/smol-agents/internal/agentctl/deploy/cloud"
	"github.com/smol-platform/smol-agents/internal/agentctl/deploy/k8s"
)

// tagKey is the discriminator that ties every resource we create back to the
// logical deployment name (--name). Use a tag (not a name prefix) so users
// can run several agentctl deployments side-by-side in the same account.
const tagKey = "agentctl-deployment"

// SSM parameter that resolves to the latest AL2023 amd64 AMI in the region.
// AL2023 includes systemd, dnf, and a recent kernel — fine for the k0s bootstrap.
// ARM users override with --ami.
const al2023AMD64SSM = "/aws/service/ami-amazon-linux-latest/al2023-ami-kernel-default-x86_64"

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
		return fmt.Errorf("--instance-type is required (e.g., t3.small for amd64, t4g.small for arm64+--ami)")
	}
	if o.AWS.KeyName == "" {
		return fmt.Errorf("--aws-key-name is required (an EC2 key pair name registered in your account)")
	}
	if o.AWS.SSHKey == "" {
		return fmt.Errorf("--ssh-key is required (local path to the private key matching --aws-key-name)")
	}
	if _, err := os.Stat(o.AWS.SSHKey); err != nil {
		return fmt.Errorf("--ssh-key %s: %w", o.AWS.SSHKey, err)
	}
	return nil
}

// Deploy provisions (or reuses) the instance, bootstraps k0s, fetches the
// kubeconfig, and runs the shared platform install.
func (*Target) Deploy(ctx context.Context, o *deploy.Options) error {
	out := o.Common.Out
	fmt.Fprintf(out, "==> target=aws profile=%s region=%s instance=%s\n",
		o.AWS.Profile, o.AWS.Region, o.AWS.InstanceType)

	awsCfg, err := config.LoadDefaultConfig(ctx,
		config.WithSharedConfigProfile(o.AWS.Profile),
		config.WithRegion(o.AWS.Region),
	)
	if err != nil {
		return fmt.Errorf("load aws config (profile=%s region=%s): %w", o.AWS.Profile, o.AWS.Region, err)
	}
	ec2c := ec2.NewFromConfig(awsCfg)
	ssmc := ssm.NewFromConfig(awsCfg)

	// 0. Networking plan: decides whether the SG opens 6443 and what URL the
	// rewritten kubeconfig will point at after the bootstrap finishes.
	plan, err := cloud.PlanAPIEndpoint(cloud.APIEndpointInput{
		CloudflareTunnelToken: o.Common.CloudflareTunnelToken,
		APIHostname:           o.Common.APIHostname,
		WireGuardConfig:       o.Common.WireGuardConfig,
	})
	if err != nil {
		return fmt.Errorf("networking plan: %w", err)
	}
	fmt.Fprintf(out, "==> Networking plan: mode=%s expose-api=%v\n", plan.Mode, plan.ExposeAPIPublicly)

	// 1. Resolve AMI (caller-supplied or SSM lookup).
	ami, err := resolveAMI(ctx, ssmc, o.AWS.AMI)
	if err != nil {
		return fmt.Errorf("resolve ami: %w", err)
	}
	fmt.Fprintf(out, "==> AMI %s\n", ami)

	// 2. Resolve subnet (caller-supplied or default VPC).
	subnet, vpcID, err := resolveSubnet(ctx, ec2c, o.AWS.SubnetID)
	if err != nil {
		return fmt.Errorf("resolve subnet: %w", err)
	}
	fmt.Fprintf(out, "==> Subnet %s (VPC %s)\n", subnet, vpcID)

	// 3. Find-or-create the security group (tagged for idempotency).
	sgID, err := ensureSecurityGroup(ctx, ec2c, out, vpcID, o.Common.Name, plan.ExposeAPIPublicly)
	if err != nil {
		return fmt.Errorf("ensure SG: %w", err)
	}

	// 4. Find-or-launch the EC2 instance.
	instance, reused, err := ensureInstance(ctx, ec2c, out, ensureInstanceInput{
		Name:         o.Common.Name,
		Region:       o.AWS.Region,
		AMI:          ami,
		InstanceType: o.AWS.InstanceType,
		SubnetID:     subnet,
		KeyName:      o.AWS.KeyName,
		SGID:         sgID,
		CloudInit: cloud.CloudInitOptions{
			Hostname:              "agentctl-" + o.Common.Name,
			CloudflareTunnelToken: o.Common.CloudflareTunnelToken,
			WireGuardConfig:       plan.WGContent,
		},
	})
	if err != nil {
		return fmt.Errorf("ensure instance: %w", err)
	}
	if reused {
		fmt.Fprintf(out, "==> Reusing existing instance %s (tag %s=%s)\n", *instance.InstanceId, tagKey, o.Common.Name)
	}

	if o.Common.DryRun {
		fmt.Fprintf(out, "==> dry-run: provisioning skipped past this point\n")
		return nil
	}

	// 5. Wait for the public IP (RunInstances returns before it's assigned).
	publicIP, err := waitPublicIP(ctx, ec2c, *instance.InstanceId, 3*time.Minute)
	if err != nil {
		return fmt.Errorf("wait public IP: %w", err)
	}
	fmt.Fprintf(out, "==> Instance %s @ %s — waiting for k0s bootstrap\n", *instance.InstanceId, publicIP)

	// 6. SSH-wait for the cloud-init sentinel (k0s up).
	sshCfg, err := cloud.SSHConfig("ec2-user", o.AWS.SSHKey)
	if err != nil {
		return fmt.Errorf("ssh config: %w", err)
	}
	wctx, cancel := context.WithTimeout(ctx, 8*time.Minute)
	defer cancel()
	if err := cloud.WaitForSentinel(wctx, net.JoinHostPort(publicIP, "22"), sshCfg, cloud.DefaultSentinelPath, 5*time.Second); err != nil {
		return fmt.Errorf("wait sentinel: %w", err)
	}
	fmt.Fprintf(out, "==> k0s bootstrap complete\n")

	// 7. Fetch + rewrite the kubeconfig. The server URL depends on the
	// networking plan: CF tunnel hostname (real TLS), WG IP (skip verify),
	// or the public IP (skip verify).
	raw, err := cloud.FetchFile(ctx, net.JoinHostPort(publicIP, "22"), sshCfg, cloud.DefaultKubeconfigPath)
	if err != nil {
		return fmt.Errorf("fetch kubeconfig: %w", err)
	}
	server, skipVerify, err := plan.FinalizeServer(publicIP)
	if err != nil {
		return fmt.Errorf("finalize server: %w", err)
	}
	rewritten, err := cloud.RewriteKubeconfig(raw, cloud.KubeconfigRewrite{Server: server, InsecureSkipTLSVerify: skipVerify})
	if err != nil {
		return fmt.Errorf("rewrite kubeconfig: %w", err)
	}
	kcPath, err := cloud.WriteKubeconfigTemp(o.Common.Name, rewritten)
	if err != nil {
		return fmt.Errorf("write kubeconfig: %w", err)
	}
	defer func() { _ = os.RemoveAll(kcPath) }()
	fmt.Fprintf(out, "==> Kubeconfig staged at %s (server=%s skipVerify=%v)\n", kcPath, server, skipVerify)

	// 8. Wait for the API to actually answer via the chosen URL (CF tunnel
	// can take a few seconds to propagate; WG/public are usually instant).
	fmt.Fprintf(out, "==> Waiting for API at %s\n", server)
	if err := cloud.WaitForAPI(ctx, kcPath, 90*time.Second); err != nil {
		return fmt.Errorf("api not reachable: %w", err)
	}

	// 9. Delegate to the k8s target with the fetched kubeconfig.
	k8sOpts := *o
	k8sOpts.K8s.Kubeconfig = kcPath
	k8sOpts.K8s.Context = ""
	if err := k8s.New().Deploy(ctx, &k8sOpts); err != nil {
		return fmt.Errorf("platform install on %s: %w", *instance.InstanceId, err)
	}

	fmt.Fprintf(out, "==> aws deploy complete — instance=%s ip=%s mode=%s\n", *instance.InstanceId, publicIP, plan.Mode)
	fmt.Fprintf(out, "    SSH:        ssh -i %s ec2-user@%s\n", o.AWS.SSHKey, publicIP)
	fmt.Fprintf(out, "    API:        %s\n", server)
	fmt.Fprintf(out, "    kubeconfig: keep %s while you use the cluster; agentctl will remove it on exit\n", kcPath)
	return nil
}

// Teardown finds resources tagged for this deployment and removes them in
// dependency order (instance → SG).
func (*Target) Teardown(ctx context.Context, o *deploy.Options) error {
	out := o.Common.Out
	awsCfg, err := config.LoadDefaultConfig(ctx,
		config.WithSharedConfigProfile(o.AWS.Profile),
		config.WithRegion(o.AWS.Region),
	)
	if err != nil {
		return fmt.Errorf("load aws config: %w", err)
	}
	ec2c := ec2.NewFromConfig(awsCfg)

	insts, err := findInstancesByTag(ctx, ec2c, o.Common.Name)
	if err != nil {
		return fmt.Errorf("find instances: %w", err)
	}
	var ids []string
	for _, i := range insts {
		ids = append(ids, *i.InstanceId)
	}
	if len(ids) > 0 {
		fmt.Fprintf(out, "==> Terminating instances: %s\n", strings.Join(ids, ", "))
		_, err := ec2c.TerminateInstances(ctx, &ec2.TerminateInstancesInput{InstanceIds: ids})
		if err != nil {
			return fmt.Errorf("terminate: %w", err)
		}
		if err := waitInstancesGone(ctx, ec2c, ids, 5*time.Minute); err != nil {
			return fmt.Errorf("wait terminated: %w", err)
		}
	} else {
		fmt.Fprintf(out, "==> No tagged instances found\n")
	}

	sgs, err := findSGsByTag(ctx, ec2c, o.Common.Name)
	if err != nil {
		return fmt.Errorf("find SGs: %w", err)
	}
	for _, sg := range sgs {
		fmt.Fprintf(out, "==> Deleting SG %s\n", *sg.GroupId)
		_, err := ec2c.DeleteSecurityGroup(ctx, &ec2.DeleteSecurityGroupInput{GroupId: sg.GroupId})
		if err != nil {
			fmt.Fprintf(out, "    delete SG: %v (may be retained by other resources)\n", err)
		}
	}
	fmt.Fprintf(out, "==> aws teardown complete\n")
	return nil
}

// resolveAMI returns user-supplied AMI or looks up the AL2023 amd64 latest via SSM.
func resolveAMI(ctx context.Context, ssmc *ssm.Client, override string) (string, error) {
	if override != "" {
		return override, nil
	}
	out, err := ssmc.GetParameter(ctx, &ssm.GetParameterInput{Name: awsv2.String(al2023AMD64SSM)})
	if err != nil {
		return "", fmt.Errorf("ssm get %s: %w", al2023AMD64SSM, err)
	}
	if out.Parameter == nil || out.Parameter.Value == nil {
		return "", fmt.Errorf("ssm %s returned empty value", al2023AMD64SSM)
	}
	return *out.Parameter.Value, nil
}

// resolveSubnet returns the user-supplied subnet, or the first subnet of the
// account's default VPC. Returns (subnetID, vpcID, err).
func resolveSubnet(ctx context.Context, ec2c *ec2.Client, override string) (string, string, error) {
	if override != "" {
		out, err := ec2c.DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{SubnetIds: []string{override}})
		if err != nil {
			return "", "", fmt.Errorf("describe subnet %s: %w", override, err)
		}
		if len(out.Subnets) == 0 {
			return "", "", fmt.Errorf("subnet %s not found", override)
		}
		return *out.Subnets[0].SubnetId, *out.Subnets[0].VpcId, nil
	}
	dv, err := ec2c.DescribeVpcs(ctx, &ec2.DescribeVpcsInput{
		Filters: []ec2types.Filter{{Name: awsv2.String("is-default"), Values: []string{"true"}}},
	})
	if err != nil {
		return "", "", fmt.Errorf("describe default vpc: %w", err)
	}
	if len(dv.Vpcs) == 0 {
		return "", "", fmt.Errorf("no default VPC in this region; pass --subnet-id")
	}
	vpcID := *dv.Vpcs[0].VpcId
	ds, err := ec2c.DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{
		Filters: []ec2types.Filter{{Name: awsv2.String("vpc-id"), Values: []string{vpcID}}},
	})
	if err != nil {
		return "", "", fmt.Errorf("describe subnets of %s: %w", vpcID, err)
	}
	if len(ds.Subnets) == 0 {
		return "", "", fmt.Errorf("default VPC %s has no subnets", vpcID)
	}
	return *ds.Subnets[0].SubnetId, vpcID, nil
}

// ensureSecurityGroup finds the tagged SG or creates one. SSH (tcp/22) is
// always open (bootstrap needs it); the k8s API (tcp/6443) is open ONLY when
// exposeAPI is true — i.e. when neither Cloudflare Tunnel nor WireGuard is
// configured, since in those cases the API is reachable via the tunnel/mesh
// instead of a public listener.
func ensureSecurityGroup(ctx context.Context, ec2c *ec2.Client, out io.Writer, vpcID, name string, exposeAPI bool) (string, error) {
	existing, err := findSGsByTag(ctx, ec2c, name)
	if err != nil {
		return "", err
	}
	if len(existing) > 0 {
		return *existing[0].GroupId, nil
	}

	sgName := "agentctl-" + name
	desc := "agentctl deploy " + name + " — SSH"
	if exposeAPI {
		desc += " + k8s API"
	}
	cr, err := ec2c.CreateSecurityGroup(ctx, &ec2.CreateSecurityGroupInput{
		GroupName:   awsv2.String(sgName),
		Description: awsv2.String(desc),
		VpcId:       awsv2.String(vpcID),
		TagSpecifications: []ec2types.TagSpecification{{
			ResourceType: ec2types.ResourceTypeSecurityGroup,
			Tags:         tagsFor(name, sgName),
		}},
	})
	if err != nil {
		return "", fmt.Errorf("create SG: %w", err)
	}
	id := *cr.GroupId
	fmt.Fprintf(out, "==> Created SG %s (%s)\n", id, sgName)

	perms := []ec2types.IpPermission{permission("tcp", 22, "ssh")}
	if exposeAPI {
		perms = append(perms, permission("tcp", 6443, "k8s API"))
	}
	if _, err := ec2c.AuthorizeSecurityGroupIngress(ctx, &ec2.AuthorizeSecurityGroupIngressInput{
		GroupId:       awsv2.String(id),
		IpPermissions: perms,
	}); err != nil {
		return "", fmt.Errorf("authorize ingress: %w", err)
	}
	if exposeAPI {
		fmt.Fprintf(out, "    ingress: tcp/22 + tcp/6443 from 0.0.0.0/0 — restrict via the AWS console after install if needed\n")
	} else {
		fmt.Fprintf(out, "    ingress: tcp/22 only (k8s API reachable via Cloudflare Tunnel / WireGuard)\n")
	}
	return id, nil
}

func permission(proto string, port int32, desc string) ec2types.IpPermission {
	return ec2types.IpPermission{
		IpProtocol: awsv2.String(proto),
		FromPort:   awsv2.Int32(port),
		ToPort:     awsv2.Int32(port),
		IpRanges:   []ec2types.IpRange{{CidrIp: awsv2.String("0.0.0.0/0"), Description: awsv2.String(desc)}},
	}
}

type ensureInstanceInput struct {
	Name, Region, AMI, InstanceType, SubnetID, KeyName, SGID string
	CloudInit                                                cloud.CloudInitOptions
}

func ensureInstance(ctx context.Context, ec2c *ec2.Client, out io.Writer, in ensureInstanceInput) (*ec2types.Instance, bool, error) {
	existing, err := findInstancesByTag(ctx, ec2c, in.Name)
	if err != nil {
		return nil, false, err
	}
	for _, i := range existing {
		switch i.State.Name {
		case ec2types.InstanceStateNameRunning, ec2types.InstanceStateNamePending:
			return &i, true, nil
		}
	}

	userData, err := cloud.CloudInitScript(in.CloudInit)
	if err != nil {
		return nil, false, err
	}
	// EC2 user-data is base64 in the JSON wire format; the SDK encodes for us.
	fmt.Fprintf(out, "==> Launching %s in %s (subnet=%s sg=%s)\n", in.InstanceType, in.Region, in.SubnetID, in.SGID)
	r, err := ec2c.RunInstances(ctx, &ec2.RunInstancesInput{
		ImageId:          awsv2.String(in.AMI),
		InstanceType:     ec2types.InstanceType(in.InstanceType),
		MinCount:         awsv2.Int32(1),
		MaxCount:         awsv2.Int32(1),
		KeyName:          awsv2.String(in.KeyName),
		SubnetId:         awsv2.String(in.SubnetID),
		SecurityGroupIds: []string{in.SGID},
		UserData:         awsv2.String(userData),
		TagSpecifications: []ec2types.TagSpecification{{
			ResourceType: ec2types.ResourceTypeInstance,
			Tags:         tagsFor(in.Name, "agentctl-"+in.Name),
		}},
	})
	if err != nil {
		return nil, false, fmt.Errorf("RunInstances: %w", err)
	}
	if len(r.Instances) == 0 {
		return nil, false, fmt.Errorf("RunInstances returned no instances")
	}
	return &r.Instances[0], false, nil
}

func waitPublicIP(ctx context.Context, ec2c *ec2.Client, id string, deadline time.Duration) (string, error) {
	dctx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	for {
		out, err := ec2c.DescribeInstances(dctx, &ec2.DescribeInstancesInput{InstanceIds: []string{id}})
		if err != nil {
			return "", err
		}
		for _, r := range out.Reservations {
			for _, i := range r.Instances {
				if i.PublicIpAddress != nil && *i.PublicIpAddress != "" {
					return *i.PublicIpAddress, nil
				}
			}
		}
		select {
		case <-dctx.Done():
			return "", fmt.Errorf("public IP not assigned to %s: %w", id, dctx.Err())
		case <-time.After(5 * time.Second):
		}
	}
}

func waitInstancesGone(ctx context.Context, ec2c *ec2.Client, ids []string, deadline time.Duration) error {
	dctx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	for {
		out, err := ec2c.DescribeInstances(dctx, &ec2.DescribeInstancesInput{InstanceIds: ids})
		if err != nil {
			return err
		}
		allGone := true
		for _, r := range out.Reservations {
			for _, i := range r.Instances {
				if i.State.Name != ec2types.InstanceStateNameTerminated && i.State.Name != ec2types.InstanceStateNameShuttingDown {
					allGone = false
				}
			}
		}
		if allGone {
			return nil
		}
		select {
		case <-dctx.Done():
			return dctx.Err()
		case <-time.After(10 * time.Second):
		}
	}
}

func findInstancesByTag(ctx context.Context, ec2c *ec2.Client, name string) ([]ec2types.Instance, error) {
	out, err := ec2c.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		Filters: []ec2types.Filter{
			{Name: awsv2.String("tag:" + tagKey), Values: []string{name}},
			{Name: awsv2.String("instance-state-name"), Values: []string{"pending", "running", "stopping", "stopped"}},
		},
	})
	if err != nil {
		return nil, err
	}
	var out2 []ec2types.Instance
	for _, r := range out.Reservations {
		out2 = append(out2, r.Instances...)
	}
	return out2, nil
}

func findSGsByTag(ctx context.Context, ec2c *ec2.Client, name string) ([]ec2types.SecurityGroup, error) {
	out, err := ec2c.DescribeSecurityGroups(ctx, &ec2.DescribeSecurityGroupsInput{
		Filters: []ec2types.Filter{{Name: awsv2.String("tag:" + tagKey), Values: []string{name}}},
	})
	if err != nil {
		return nil, err
	}
	return out.SecurityGroups, nil
}

func tagsFor(deployName, resourceName string) []ec2types.Tag {
	return []ec2types.Tag{
		{Key: awsv2.String(tagKey), Value: awsv2.String(deployName)},
		{Key: awsv2.String("Name"), Value: awsv2.String(resourceName)},
	}
}
