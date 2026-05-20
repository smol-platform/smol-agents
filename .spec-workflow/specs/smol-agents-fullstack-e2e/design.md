# Design — smol-agents-fullstack-e2e

This document covers all three rings; the bulk is L2 (single-EC2 +
k0s) since it's the new infrastructure. L0 and L1 are sketched at the
bottom — both reuse existing patterns from
`scripts/kind-verify.sh` and `test/integration/`.

## L2: Single-EC2 + k0s on AWS Spot bare-metal

### Topology

```
                       ┌──────────────────────────────────────────┐
  Mac / GHA Runner     │              EC2 c6gd.metal              │
  ─────────────────    │   (Amazon Linux 2023, arm64, Spot)        │
   go test             │                                          │
   (testcontainers     │  cloud-init                              │
    + ssm-runner)      │   └─ install: containerd + kata + k0s    │
        │              │   └─ register kata runtime in containerd │
        │  AWS API     │   └─ start k0s controller --single        │
        ├──────────────│   └─ apply CRDs + operator + samples      │
        │  SSM Run     │                                          │
        ├──────────────▶  k0s single-node cluster                  │
        │   send-      │   ┌──────────────────────────────────┐   │
        │   command    │   │ smol-agents-system            │   │
        │              │   │  - operator deployment           │   │
        │  poll for    │   │  - ebpf-loader DaemonSet (priv)  │   │
        │  results     │   │  - SPIRE server + agent          │   │
        │              │   ├──────────────────────────────────┤   │
        │              │   │ tenant-a                         │   │
        │              │   │  - ModelProvider, Tool, Agent    │   │
        │              │   │  - AgentRun (Pod uses kata-fc)   │   │
        │              │   │  - AgentNetwork (proxy + WG)     │   │
        │              │   │  - fake-gateway, fake-llm        │   │
        │              │   └──────────────────────────────────┘   │
        │              └──────────────────────────────────────────┘
        │
        ▼
   exit-trap → terminate-instances
   sweeper Lambda (TTL > 1h) ──► terminate
   budget alarm ──► nuke all *-e2e=*
```

### Components

#### Provisioner (`scripts/aws-l2/up.sh` + Go test setup)

The Go test driver does this work directly via `aws-sdk-go-v2` so the
test owns its lifecycle. A bash wrapper exists for humans but isn't
the primary entry point.

```go
// test/e2e/fullstack/l2/cluster.go
type L2Cluster struct {
    InstanceID string
    PublicDNS  string
    EndpointURL string
    Kubeconfig []byte
}

func Provision(t *testing.T, ctx context.Context) *L2Cluster {
    t.Helper()
    cfg, _ := config.LoadDefaultConfig(ctx)
    ec2c := ec2.NewFromConfig(cfg)
    ssmc := ssm.NewFromConfig(cfg)

    runID := os.Getenv("E2E_RUN_ID")
    if runID == "" { runID = randHex(6) }

    out, err := ec2c.RunInstances(ctx, &ec2.RunInstancesInput{
        InstanceType: types.InstanceTypeC6gdMetal,
        ImageId:      aws.String(amazonLinux2023ARM64("us-east-2")),
        InstanceMarketOptions: &types.InstanceMarketOptionsRequest{
            MarketType: types.MarketTypeSpot,
        },
        IamInstanceProfile: &types.IamInstanceProfileSpecification{
            Name: aws.String("smol-agents-e2e-l2"),
        },
        UserData: aws.String(base64.StdEncoding.EncodeToString(cloudInitYAML)),
        TagSpecifications: []types.TagSpecification{{
            ResourceType: types.ResourceTypeInstance,
            Tags: []types.Tag{
                {Key: aws.String("smol-agents-e2e"), Value: aws.String("L2")},
                {Key: aws.String("run-id"),             Value: aws.String(runID)},
                {Key: aws.String("expires-at"),         Value: aws.String(rfc3339Plus("1h"))},
            },
        }},
        MinCount: aws.Int32(1), MaxCount: aws.Int32(1),
    })
    require.NoError(t, err)

    inst := out.Instances[0]
    t.Cleanup(func() { terminate(ctx, ec2c, *inst.InstanceId) })

    waitForSSMReady(ctx, ssmc, *inst.InstanceId, 5*time.Minute)
    kc := fetchKubeconfigViaSSM(ctx, ssmc, *inst.InstanceId)

    return &L2Cluster{
        InstanceID: *inst.InstanceId,
        PublicDNS:  *inst.PublicDnsName,
        Kubeconfig: kc,
    }
}
```

#### cloud-init (`scripts/aws-l2/cloud-init.yaml`)

Single-shot bootstrap. Idempotent (the script no-ops if already done)
so SSM commands can re-run safely.

```yaml
#cloud-config
write_files:
  - path: /etc/containerd/config.toml
    content: |
      version = 2
      [plugins."io.containerd.grpc.v1.cri".containerd.runtimes.kata-fc]
        runtime_type = "io.containerd.kata-fc.v2"
        privileged_without_host_devices = true
        [plugins."io.containerd.grpc.v1.cri".containerd.runtimes.kata-fc.options]
          ConfigPath = "/etc/kata-containers/configuration-fc.toml"
  - path: /etc/k0s/k0s.yaml
    content: |
      apiVersion: k0s.k0sproject.io/v1beta1
      kind: ClusterConfig
      spec:
        api:
          address: 0.0.0.0
        network:
          provider: kuberouter
        installConfig:
          users: { etcdUser: etcd, kineUser: kube-apiserver,
                   konnectivityUser: konnectivity-server,
                   kubeAPIserverUser: kube-apiserver,
                   kubeSchedulerUser: kube-scheduler }

runcmd:
  # 1. Install containerd, runc, kata, helm, k0s
  - dnf install -y containerd runc cri-tools jq curl tar
  - curl -sSL https://github.com/kata-containers/kata-containers/releases/download/3.2.0/kata-static-3.2.0-arm64.tar.xz | tar -xJ -C /
  - curl -sSf https://get.k0s.sh | sh
  # 2. Mount BPF FS + cgroup v2 prerequisites (AL2023 has them by default)
  - mount bpffs /sys/fs/bpf -t bpf || true
  # 3. Start k0s in single-node mode
  - k0s install controller --single -c /etc/k0s/k0s.yaml
  - k0s start
  - timeout 120 sh -c 'until k0s status >/dev/null 2>&1; do sleep 2; done'
  # 4. Apply our manifests via k0s manifest watcher
  - mkdir -p /var/lib/k0s/manifests/{spire,operator,samples}
  - aws s3 cp s3://${ARTIFACT_BUCKET}/manifests.tar.gz - | tar -xz -C /var/lib/k0s/manifests/
  # 5. Pull operator + agent + ebpf-loader images into containerd
  - aws s3 cp s3://${ARTIFACT_BUCKET}/images.tar - | k0s ctr images import -
  # 6. Sentinel
  - touch /var/log/k0s-bootstrap.READY
```

The `${ARTIFACT_BUCKET}` substitution happens at user-data render time;
the bucket holds the kustomize-rendered manifests and a tar of pre-
built images. CI builds these once per workflow run and uploads.

#### Test driver structure

```
test/e2e/fullstack/
├── shared/                  # used by all rings
│   ├── scenarios.go         # the actual assertions (ring-agnostic)
│   ├── samples.go           # CR fixtures
│   ├── fakes/
│   │   ├── llm.go           # deterministic LLM (returns canned plans)
│   │   └── gateway.go       # echo TCP + echo HTTP gateway
│   └── spiffe.go            # SPIRE registration helpers
├── l0/
│   └── compose_test.go      # docker-compose; build tag e2e_l0
├── l1/
│   └── kind_test.go         # OrbStack kind; build tag e2e_l1
└── l2/
    ├── cluster.go           # provision/teardown EC2
    ├── cloudinit.yaml       # embedded
    └── ec2_test.go          # build tag e2e_l2
```

`shared/scenarios.go` is the gold: each scenario is a function
`func Run(t *testing.T, env Env)` where `Env` is an interface
satisfied by the L0/L1/L2 setup. Same assertions run at every ring.

### Scenarios (cross-ring)

Each scenario asserts a specific cross-component invariant.

| ID | Scenario | What it proves |
|---|---|---|
| **S-IDENT-1** | Agent gets X509-SVID rotated mid-run | SPIRE binding + SVID rotation |
| **S-PROXY-TCP** | Agent dials `127.0.0.1:5432`; gateway sees mTLS with agent SVID | TCP proxy + SPIFFE auth |
| **S-PROXY-HTTP** | Agent calls HTTP sidecar; gateway validates JWT-SVID with right audience | HTTP proxy + JWT minting |
| **S-EBPF-DROP** | Agent dials `1.1.1.1:443`; cgroup_skb drops; ringbuf event recorded | eBPF allow-list enforcement |
| **S-EBPF-REDIR** | Agent dials `10.42.0.0/16`; cgroup/connect4 rewrites to sidecar | eBPF transparent redirect |
| **S-WG-CLIENT** | Agent's WG adapter completes handshake with peer; ICMP through tunnel | WireGuard userspace |
| **S-AGENTRUN** | Real agent runs plan-act-observe loop with fake LLM + tool | Runtime + Pod + reconciler |
| **S-CANCEL** | Set `spec.cancel=true`; AgentRun reaches `Cancelled` | Cancellation path |
| **S-WEBHOOK** | Apply invalid AgentNetwork (both transports set); rejected | Webhook admission (L1+L2 only) |
| **S-KATA** | AgentRun Pod with `runtimeClass: kata-fc` boots a microVM (uname -r differs from host) | Kata sandbox (L2 only) |

Each scenario tagged with the rings it runs at via Go build constraints
or runtime gating in `Env.Capabilities()`.

### Cleanup

Inline (Go test):
```go
t.Cleanup(func() {
    _, _ = ec2c.TerminateInstances(ctx, &ec2.TerminateInstancesInput{
        InstanceIds: []string{inst.InstanceID},
    })
})
```

Sweeper (Lambda, deployed via Terraform):
- Trigger: EventBridge rule, every 30 min
- Function: `aws ec2 describe-instances --filters Name=tag:smol-agents-e2e,Values=L2 --query 'Reservations[].Instances[?LaunchTime<=`$(date -d "1 hour ago" --iso-8601=seconds)`].InstanceId'`
- Action: `terminate-instances` on each match
- Logs to CloudWatch + sends Slack/email if any termination happened

Budget alarm:
- AWS Budget at $50/month on tag `smol-agents-e2e`
- 80% threshold → SNS notification
- 100% threshold → SNS → Lambda → terminate ALL `smol-agents-e2e=*` instances

### AWS account / region / profile

- **Account**: `stigen` sandbox (assume role via OIDC in CI; `aws
  --profile stigen` locally).
- **Region**: `us-east-2`.
- **All AWS SDK calls** must explicitly set region; no implicit `default`
  region usage. Test driver fails loudly if `AWS_REGION` ≠ `us-east-2`
  to prevent accidental cross-region cost.
- **Terraform backend**: S3 bucket `smol-agents-e2e-tfstate-us-east-2`
  in the stigen account, with DynamoDB lock table.

### IAM

Two roles:

1. **`smol-agents-e2e-runner`** — assumed by GHA via OIDC. Permissions:
   - `ec2:RunInstances` (with conditions limiting to `c6gd.metal` + Spot + tag mandatory)
   - `ec2:TerminateInstances` (only on tag `smol-agents-e2e`)
   - `ec2:DescribeInstances`
   - `ssm:SendCommand`, `ssm:GetCommandInvocation`
   - `s3:GetObject` on artifact bucket
   - `iam:PassRole` to `smol-agents-e2e-l2` (the instance profile)

2. **`smol-agents-e2e-l2`** — instance profile attached to EC2. Permissions:
   - `s3:GetObject` on artifact bucket
   - `ssm:UpdateInstanceInformation` (for agent registration)

GHA workflow uses the OIDC trust policy to assume role 1 without
static credentials.

### Cost guardrails

- **Hard cap**: $50/month on tag `smol-agents-e2e`. AWS Budget at
  80% → SNS notify; at 100% → Lambda terminates all matching instances.
- **Per-run terminate** (no warm pool). Confirmed simpler-and-safer
  trade vs ~3 min boot cost per run.
- Spot + smallest viable bare-metal (`c6gd.metal` ≈ $0.90/hr Spot in
  us-east-2; on-demand $3.06/hr).
- Single AZ in us-east-2, no NAT gateway, no EBS provisioning (uses
  instance-store NVMe — ephemeral by design, free).
- 1-hour TTL forces cleanup even on wedged tests.
- Pre-flight refuses to start if > 3 active L2 instances exist
  (catches runaway CI loops; tighter than the original 5 since we
  don't have a team).
- Estimated capacity at $50/mo cap: ~$0.22/run × 220 runs/month =
  fits comfortably with headroom for occasional `c6gd.metal` price
  spikes.

## L1: kind-on-OrbStack

Reuses `scripts/kind-verify.sh` patterns. Key additions over what
exists today:

1. Apply SPIRE manifests (Helm chart `spire-server` + DaemonSet for
   `spire-agent`).
2. Pre-create the `<agent>-agent` ServiceAccount per the bootstrap
   pitfalls memory (or apply a SmolAgent CR with identity feature
   enabled and wait for SA materialization).
3. Build agent image with `docker buildx build --platform linux/arm64`,
   `kind load docker-image`.
4. Run the same `shared/scenarios.go` cross-ring assertions, gated to
   ones that don't require real Kata.

## L0: docker-compose

Pure userland. testcontainers-go drives the lifecycle. No Kubernetes —
the agent runtime, identity proxy, and SPIRE all run as plain
containers. WireGuard userspace works here too because it's pure Go +
gVisor netstack.

`scripts/e2e/compose-l0.yaml`:
```yaml
services:
  spire-server:    image: ghcr.io/spiffe/spire-server:1.10
  spire-agent:     image: ghcr.io/spiffe/spire-agent:1.10
  fake-llm:        image: smol-agents/fake-llm:dev
  fake-gateway:    image: smol-agents/fake-gateway:dev
  proxy-tcp:       image: smol-agents/proxy:dev
  proxy-http:      image: smol-agents/proxy:dev
  agent:           image: smol-agents/agent:dev
```

The L0 test uses the same `shared/scenarios.go` for every applicable
scenario (S-IDENT-1, S-PROXY-TCP, S-PROXY-HTTP, S-AGENTRUN, S-WG-CLIENT,
S-CANCEL).

## Bootstrap pitfalls applied

Pulled from `bootstrap_pitfalls.md`:

- **`<agent>-agent` SA**: L1 + L2 cloud-init explicitly creates the SA
  before applying the AgentRun CR, OR applies a SmolAgent CR first
  and waits for the identity feature to materialize the SA.
- **Cross-CR watches**: AgentReconciler now watches ModelProvider/Tool;
  AgentNetworkReconciler watches Secrets. The L2 cloud-init can apply
  resources in any order without forced re-reconciles.
- **Webhooks**: L1 uses the `operator/config/kind` overlay (no
  webhook). L2 will install cert-manager and run with webhooks
  enabled — that's a real test difference.
- **PodSecurity**: cloud-init labels `smol-agents-system` as
  `privileged` BEFORE applying manifests.
- **Image loading**: L2 uses `k0s ctr images import` after `aws s3 cp`.
  L1 keeps `kind load`.
- **No kubelet on envtest**: doesn't apply at L1+L2 (real kubelet).
  L0 has no Pod concept so the test driver is responsible for
  starting/stopping containers explicitly.

## Trade-offs and out-of-scope confirmations

- **No Knative Serving**: deferred. The deploymentKind=knative path
  isn't critical for the agent runtime+sandbox+network testing focus.
- **No multi-tenant chaos**: deferred. Each test uses a fresh
  namespace; multi-tenant adversarial scenarios belong in a separate
  spec.
- **No real LLM**: deterministic fake. A real-LLM smoke test could
  live in a `nightly-llm-smoke` spec but is out of scope here.
- **No real cloud egress targets**: tests dial only fakes inside the
  cluster; egress to `1.1.1.1` is for the eBPF drop test specifically
  (negligible cost, real kernel path).
- **No multi-region / DR**: single region (us-east-2, stigen sandbox
  account). Region/account hard-coded; switching is a deliberate
  change, not a config flag.
