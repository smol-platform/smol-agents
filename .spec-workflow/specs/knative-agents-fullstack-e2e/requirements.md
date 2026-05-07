# Requirements — knative-agents-fullstack-e2e

EARS-style. Each requirement gets a stable ID for cross-reference from
`tasks.md` and from test scenario filenames.

## Driver / Orchestration (R-E2E-DRV-*)

**R-E2E-DRV-1** — When the user invokes `go test -tags=e2e_l0 ./test/e2e/fullstack/l0/...`, the system **shall** spin up the docker-compose stack, run all applicable scenarios, and tear down within a single test process.

**R-E2E-DRV-2** — When the user invokes `go test -tags=e2e_l1`, the system **shall** detect the local container runtime (OrbStack vs native docker), bring up a kind cluster inside it, and run all L0+L1 scenarios.

**R-E2E-DRV-3** — When the user invokes `go test -tags=e2e_l2`, the system **shall** assume the `stigen` AWS profile, provision a single Spot bare-metal EC2 in `us-east-2`, bootstrap k0s + Kata, run all scenarios including L2-only ones, and terminate the instance regardless of test outcome.

**R-E2E-DRV-4** — Each scenario in `test/e2e/fullstack/shared/scenarios.go` **shall** run unmodified at every ring whose `Env.Capabilities()` advertises the scenario's required capabilities (no copy-pasted scenario logic across rings).

**R-E2E-DRV-5** — If a test driver invocation crashes mid-run, the system **shall** still emit a structured failure (TAP/JSON) so CI knows the test failed rather than timing out silently.

## L0 — docker-compose (R-E2E-L0-*)

**R-E2E-L0-1** — When L0 starts, the system **shall** run real `spire-server` + `spire-agent` containers (no socket mocking) and register every workload that participates.

**R-E2E-L0-2** — When L0 starts, the system **shall** run the project's actual `agent`, `proxy-tcp`, `proxy-http`, and `secret-broker` binaries unmodified (only configuration and secrets differ from production).

**R-E2E-L0-3** — When L0 starts, the system **shall** run a deterministic fake LLM that returns canned plans keyed by request hash, so every Run is reproducible bit-for-bit.

**R-E2E-L0-4** — When L0 starts, the system **shall** run a fake gateway (echo TCP + echo HTTP) authenticated by its own SVID so the proxies have a real upstream to authenticate against.

**R-E2E-L0-5** — While L0 is running, no scenario **shall** require Linux kernel features (eBPF, cgroup v2, /dev/kvm). Scenarios needing them are gated to L1+ via capability flags.

## L1 — kind-on-OrbStack (R-E2E-L1-*)

**R-E2E-L1-1** — When L1 starts on macOS, the system **shall** detect OrbStack and create a kind cluster inside the OrbStack Linux VM. On Linux dev boxes the same code path uses native docker.

**R-E2E-L1-2** — When L1 applies manifests, the system **shall** label the `knative-agents-system` namespace `pod-security.kubernetes.io/enforce: privileged` BEFORE applying the operator and ebpf-loader DaemonSet.

**R-E2E-L1-3** — When L1 applies the AgentRun sample, the system **shall** ensure the `<agent>-agent` ServiceAccount exists (either via a KnativeAgent CR's identity feature or pre-created explicitly), preventing the "SA not found" admission failure.

**R-E2E-L1-4** — When L1 starts, the system **shall** load eBPF programs from `bpf/build/*.bpf.o` into the kernel and pin maps under `/sys/fs/bpf/knative-agents/`.

**R-E2E-L1-5** — While L1 is running, the operator's webhook **shall remain disabled** (kustomize overlay `operator/config/kind`) — webhook fidelity moves to L2.

**R-E2E-L1-6** — When L1 builds images, the system **shall** target `linux/arm64` and use `kind load docker-image` to make them available to the cluster.

## L2 — single-EC2 + k0s on AWS (R-E2E-L2-*)

**R-E2E-L2-1** — When L2 starts, the system **shall** assume the `stigen` AWS profile (or assume role via OIDC in CI) and operate exclusively in `us-east-2`. If `AWS_REGION` is set to anything else, the driver **shall** abort with a hard error.

**R-E2E-L2-2** — When L2 provisions, the system **shall** request a Spot `c6gd.metal` instance, tagged with `knative-agents-e2e=L2`, `run-id=<uuid>`, and `expires-at=<rfc3339+1h>`.

**R-E2E-L2-3** — When the EC2 instance reaches `running`, the system **shall** wait for SSM agent registration before issuing any commands (no SSH).

**R-E2E-L2-4** — When cloud-init bootstrap completes, the system **shall** verify a sentinel file (`/var/log/k0s-bootstrap.READY`) before running test scenarios.

**R-E2E-L2-5** — When the test process exits (success, failure, or panic), the system **shall** call `TerminateInstances` for the run's instance ID via a deferred / `t.Cleanup` hook.

**R-E2E-L2-6** — When L2 deploys the operator, the system **shall** include cert-manager and run with the validating webhook enabled (the L1→L2 fidelity gain).

**R-E2E-L2-7** — While L2 is running, the AgentRun Pod **shall** use `runtimeClass: kata-fc`, and the test **shall** verify the running container's kernel version differs from the host's (proves Kata actually launched a microVM, not silent runc fallback).

**R-E2E-L2-8** — Before provisioning, the L2 driver **shall** count active EC2 instances tagged `knative-agents-e2e=L2` in `us-east-2`. If the count exceeds 3, the driver **shall** refuse to start a new run (catches runaway CI loops).

**R-E2E-L2-9** — When L2 needs operator/agent images, the system **shall** pull them from ECR (`<account>.dkr.ecr.us-east-2.amazonaws.com/knative-agents/*`) — built in CI from the same commit being tested.

## Cleanup (R-E2E-CLEAN-*)

**R-E2E-CLEAN-1** — Every L2 EC2 instance **shall** be terminable by tag alone (`knative-agents-e2e=L2`); no other source-of-truth is required for cleanup.

**R-E2E-CLEAN-2** — The system **shall** deploy a sweeper Lambda triggered every 30 minutes by EventBridge that terminates all `knative-agents-e2e=L2` instances older than 1 hour. The Lambda **shall** be idempotent and **shall** log any termination.

**R-E2E-CLEAN-3** — The system **shall** deploy a budget-alarm Lambda triggered when the AWS Budget for tag `knative-agents-e2e` reaches 100% of $50/month. The Lambda **shall** terminate every instance with a `knative-agents-e2e=*` tag.

**R-E2E-CLEAN-4** — When a Spot interruption notice is delivered to the running instance, cloud-init **shall** run a graceful-shutdown hook that flushes test logs to S3 before the 2-minute deadline.

**R-E2E-CLEAN-5** — The driver **shall** never leave behind: VPCs, security groups, EBS volumes (instance store only), IAM roles (provisioned once via Terraform, reused), or S3 objects (other than test logs under TTL).

## Cost Guardrails (R-E2E-COST-*)

**R-E2E-COST-1** — When provisioning, the L2 driver **shall** request only Spot capacity (`MarketType=spot`). On-demand requests are forbidden.

**R-E2E-COST-2** — The L2 driver **shall** target only `c6gd.metal`. Other instance types are forbidden without a spec amendment.

**R-E2E-COST-3** — The instance profile **shall** be configured with no NAT gateway, no EBS root volume, and a single-AZ subnet. Outbound traffic uses the public IP directly.

**R-E2E-COST-4** — The AWS Budget for tag `knative-agents-e2e` **shall** notify at 50% (informational), 80% (warning), and trigger termination at 100% of the $50/month cap.

## Scenario Invariants (R-E2E-SCN-*)

These cross-cutting invariants are asserted by the shared scenarios at
every ring that has the required capability.

**R-E2E-SCN-IDENT-1** — When the agent boots, it **shall** receive an X509-SVID from SPIRE within 30 s and rotate before expiry.

**R-E2E-SCN-PROXY-TCP** — When the agent dials the local TCP proxy port, the upstream gateway **shall** observe an mTLS handshake whose client cert is the agent's SVID.

**R-E2E-SCN-PROXY-HTTP** — When the agent makes an HTTP request through the proxy, the upstream **shall** observe a JWT-SVID Bearer token whose `aud` matches the configured `jwtAudience`.

**R-E2E-SCN-EBPF-DROP** (L1+) — When the agent dials an IP outside the allow-list (e.g. `1.1.1.1:443`), the eBPF `cgroup_skb/egress` program **shall** drop the packet AND emit a ringbuf audit event.

**R-E2E-SCN-EBPF-REDIR** (L1+) — When the agent dials an IP inside `redirectCIDRs`, the `cgroup/connect4` program **shall** rewrite the destination to the local proxy and the proxy **shall** see the original IP in connection metadata.

**R-E2E-SCN-WG-CLIENT** — When the AgentNetwork is `wireguardMesh client`, the userspace device **shall** complete a handshake with the configured peer within 5 s, and an ICMP packet to a peer-AllowedIP **shall** round-trip.

**R-E2E-SCN-AGENTRUN** — When an AgentRun CR is applied, the operator **shall** create a Pod, the agent runtime **shall** execute one Plan→Tool→Observation→Final cycle, and the AgentRun status **shall** transition `Pending → Running → Completed` with a non-empty `Output`.

**R-E2E-SCN-CANCEL** — When `spec.cancel=true` is set on a Running AgentRun, the AgentRun **shall** transition to `Cancelled` within 10 s and the Pod **shall** be deleted.

**R-E2E-SCN-WEBHOOK** (L2 only) — When an AgentNetwork is applied with both `identityProxy` and `wireguardMesh` set, the apiserver **shall** reject it via the validating webhook before the reconciler sees it.

**R-E2E-SCN-KATA** (L2 only) — When an AgentRun Pod runs with `runtimeClass: kata-fc`, `uname -r` inside the container **shall** report the Kata-shipped kernel, distinct from the host kernel reported by `uname -r` on the EC2 instance.

**R-E2E-SCN-KA-PHASE** (L1+) — When the operator reconciles a minimal `KnativeAgent` CR (e.g. `tenant-a/hello` from kind-verify), the CR's `Status.Phase` **shall** transition to `Ready` within 60 s. Verifies the operator's status reconciliation path works end-to-end against a live apiserver (CRDs admitted, conditions populated, aggregate Ready computed).

## Verification (R-E2E-VRF-*)

**R-E2E-VRF-1** — Every requirement in this document **shall** map to at least one Go test in `test/e2e/fullstack/`. CI **shall** fail if any R-E2E-* ID is unreferenced by tests (a `coverage.go` file maintains the mapping and the CI check parses it).

**R-E2E-VRF-2** — A `make e2e-l0` target **shall** exit zero only when every L0-applicable scenario passes. Same for `e2e-l1` and `e2e-l2`.

**R-E2E-VRF-3** — A `make e2e-clean-aws` target **shall** terminate every `knative-agents-e2e=*` resource in the stigen account (manual escape hatch).

**R-E2E-VRF-4** — CI **shall** run L0+L1 on every PR. L2 **shall** run on `/test-l2` PR comment, on push to `main`, and nightly at 02:00 UTC.
