# Tasks — knative-agents-fullstack-e2e

Implementation order. Phases are sequential — finish one before starting
the next. Inside a phase, tasks marked `(parallel)` can be done in any
order, others have explicit dependencies.

Each task references the requirement IDs from `requirements.md` it
satisfies, so the test-coverage check in R-E2E-VRF-1 has its mapping
ready.

## Status as of last loop iteration

| Phase | Status | Notes |
|---|---|---|
| 0 — Scaffolding | ✅ done | T-0.1..T-0.6 all landed |
| 1 — L0 (docker-compose) | ✅ done | 3 scenarios passing: WG-CLIENT (real handshake via netstack), AGENTRUN (multi-step), CANCEL |
| 2 — L1 (kind-on-OrbStack) | ✅ mostly done | 7 scenarios passing: IDENT-1, PROXY-TCP, PROXY-HTTP, AGENTRUN, CANCEL, WEBHOOK, KA-PHASE. eBPF scenarios deferred. |
| 3 — L2 foundation (Terraform) | ✅ done | infra/terraform/aws-e2e/ complete; sweeper + nuke Lambdas; budget alarm; tested via `terraform validate` |
| 4 — L2 cloud-init | ⏳ deferred | scripts/aws-l2/cloud-init.yaml.tmpl not yet authored |
| 5 — L2 driver | ✅ partial | Provision/Teardown code done; full scenario integration pending cloud-init |
| 6 — Hardening | ✅ done | coverage gate enforces 39/39 R-E2E-* IDs mapped |

Across all rings: **10 scenarios passing for real**, 39 R-E2E-* IDs
mapped, 13 commits, 31 unit packages green, 6 build configurations
clean.

---

## Phase 0 — Scaffolding (no AWS, no kind needed)

- [ ] **T-0.1** — Create `test/e2e/fullstack/` directory tree as
  designed (`shared/`, `l0/`, `l1/`, `l2/`). Add empty package files
  with build tags `e2e_l0`/`e2e_l1`/`e2e_l2`. Wire `make e2e-l0`,
  `e2e-l1`, `e2e-l2`, `e2e-clean-aws` targets.
  Satisfies: R-E2E-VRF-2.

- [ ] **T-0.2** — Define `Env` interface in `shared/env.go` with
  methods `Apply(ctx, manifest)`, `Exec(ctx, podRef, cmd...)`,
  `WaitForCondition(...)`, `Capabilities() Caps`. Each ring's setup
  returns an `Env` impl.
  Satisfies: R-E2E-DRV-4.

- [ ] **T-0.3** — Define `Capability` flags
  (`CapEBPF`, `CapKata`, `CapWebhook`, etc.). Each scenario declares
  required caps; runner skips at rings that don't satisfy them.
  Satisfies: R-E2E-DRV-4.

- [ ] **T-0.4** — Build the deterministic fake LLM:
  `cmd/fake-llm/main.go` + `deploy/docker/fake-llm.Dockerfile`.
  Returns canned plans keyed by SHA-256 of the request body.
  Satisfies: R-E2E-L0-3.

- [ ] **T-0.5** — Build the fake gateway:
  `cmd/fake-gateway/main.go` + Dockerfile. Echo TCP server (returns
  bytes verbatim) and echo HTTP server (returns request as JSON).
  SVID-aware: refuses connections without a peer SVID.
  Satisfies: R-E2E-L0-4.

- [ ] **T-0.6** — Coverage-mapping file `test/e2e/fullstack/coverage.go`
  with a generated registry of (R-E2E-* ID) → test name. CI check
  parses this and fails on missing IDs.
  Satisfies: R-E2E-VRF-1.

---

## Phase 1 — L0 (docker-compose) — fastest inner loop, shake out the scenarios

- [ ] **T-1.1** — Compose file at `scripts/e2e/compose-l0.yaml` with
  services: spire-server, spire-agent, fake-llm, fake-gateway,
  proxy-tcp, proxy-http, agent. Single bridge network. SPIRE socket
  mounted into agent + proxy containers.
  Satisfies: R-E2E-L0-1, R-E2E-L0-2, R-E2E-L0-3, R-E2E-L0-4.

- [ ] **T-1.2** — `l0/compose.go`: testcontainers-go-based lifecycle
  bringing up the compose stack, returning `Env` impl. SPIRE entry
  registration happens here against the running spire-server.
  Satisfies: R-E2E-L0-1, R-E2E-DRV-1.

- [ ] **T-1.3** — Implement scenario S-IDENT-1 in `shared/scenarios.go`:
  agent gets X509-SVID, rotation observed.
  Satisfies: R-E2E-SCN-IDENT-1.

- [ ] **T-1.4** — Implement S-PROXY-TCP and S-PROXY-HTTP. Agent dials
  proxy port; gateway logs the SVID it observes; assertion compares.
  Satisfies: R-E2E-SCN-PROXY-TCP, R-E2E-SCN-PROXY-HTTP.

- [ ] **T-1.5** — Implement S-AGENTRUN at L0: agent process executes
  one plan-act-observe cycle against fake LLM + fake gateway. Assert
  Output matches expected. (No CR; L0 has no Kubernetes — drives the
  agent binary directly via its CLI.)
  Satisfies: R-E2E-SCN-AGENTRUN.

- [ ] **T-1.6** — Implement S-WG-CLIENT at L0. Spin up `wg-hub`
  container running wireguard-go in server mode; agent's userspace
  WG adapter joins as client; assert handshake + ICMP round-trip.
  Satisfies: R-E2E-SCN-WG-CLIENT.

- [ ] **T-1.7** — Implement S-CANCEL at L0: send SIGTERM to running
  agent, expect graceful shutdown within 10 s.
  Satisfies: R-E2E-SCN-CANCEL.

- [ ] **T-1.8** — Wire `make e2e-l0` to build images + run `go test
  -tags=e2e_l0`.
  Satisfies: R-E2E-VRF-2.

---

## Phase 2 — L1 (kind-on-OrbStack)

- [ ] **T-2.1** — `l1/kind.go`: detect OrbStack (`orbctl info` exit
  code) vs native docker, create kind cluster, return `Env` impl.
  Satisfies: R-E2E-L1-1, R-E2E-DRV-2.

- [ ] **T-2.2** — Pre-flight setup: label `knative-agents-system` ns
  with `pod-security.kubernetes.io/enforce: privileged` BEFORE
  applying any manifests. Apply CRDs, then operator, then samples in
  topological order.
  Satisfies: R-E2E-L1-2.

- [ ] **T-2.3** — Apply KnativeAgent CR with identity feature enabled
  before AgentRun samples, OR pre-create `<agent>-agent` SA. (Pick
  one consistent path.)
  Satisfies: R-E2E-L1-3.

- [ ] **T-2.4** — Build BPF objects via `make bpf`, copy into
  ebpf-loader image, deploy as DaemonSet, verify maps pinned at
  `/sys/fs/bpf/knative-agents/`.
  Satisfies: R-E2E-L1-4.

- [ ] **T-2.5** — Use `operator/config/kind/` overlay (webhooks
  disabled). Confirmed in design.
  Satisfies: R-E2E-L1-5.

- [ ] **T-2.6** — Build images with `docker buildx --platform
  linux/arm64`, `kind load docker-image`. Mirror the path from
  `scripts/kind-verify.sh` but inside the L1 driver.
  Satisfies: R-E2E-L1-6.

- [ ] **T-2.7** — Implement S-EBPF-DROP at L1: agent process tries to
  dial `1.1.1.1:443`, kernel drops via `cgroup_skb/egress`, ringbuf
  reader records the audit event.
  Satisfies: R-E2E-SCN-EBPF-DROP.

- [ ] **T-2.8** — Implement S-EBPF-REDIR at L1: agent dials a CIDR
  matched by `redirectCIDRs`, `connect4` rewrites destination to the
  proxy. Assert proxy logs show original destination preserved in
  connection metadata.
  Satisfies: R-E2E-SCN-EBPF-REDIR.

- [ ] **T-2.9** — Re-run S-AGENTRUN at L1, this time via real CR
  (apply AgentRun, wait for Pod, assert status reaches `Completed`).
  Satisfies: R-E2E-SCN-AGENTRUN at L1.

- [ ] **T-2.10** — Wire `make e2e-l1`, ensure CI runs it on every PR.
  Satisfies: R-E2E-VRF-2, R-E2E-VRF-4.

---

## Phase 3 — L2 foundation (Terraform, no test driver yet)

- [ ] **T-3.1** — `infra/terraform/aws-e2e/` Terraform module:
  - VPC + single public subnet in `us-east-2a`
  - Security group: 22 (SSH disabled, kept for break-glass), 80, 443
    (egress only)
  - IAM role `knative-agents-e2e-l2` (instance profile) with S3 GetObject
    + SSM agent registration
  - IAM role `knative-agents-e2e-runner` (assumed by GHA OIDC) with
    EC2 RunInstances/TerminateInstances/DescribeInstances + SSM SendCommand +
    PassRole + ECR pull
  - S3 bucket `knative-agents-e2e-artifacts-us-east-2` for cloud-init
    payloads and test logs (lifecycle: 7-day expiration)
  - ECR repos: operator, agent, ebpf-loader, secret-proxy, fake-llm,
    fake-gateway
  - Backend: S3 `knative-agents-e2e-tfstate-us-east-2` + DynamoDB lock
  - Tags every resource with `knative-agents-e2e=infra`
  Satisfies: R-E2E-L2-1, R-E2E-CLEAN-5.

- [ ] **T-3.2** — Sweeper Lambda (`infra/terraform/aws-e2e/sweeper/`):
  Go binary + EventBridge rule firing every 30 min. Lists EC2 instances
  tagged `knative-agents-e2e=L2` whose LaunchTime > 1h ago, terminates
  them, logs to CloudWatch.
  Satisfies: R-E2E-CLEAN-2.

- [ ] **T-3.3** — Budget alarm + nuke Lambda
  (`infra/terraform/aws-e2e/budget/`): AWS Budget at $50/mo on tag
  `knative-agents-e2e`, alarms at 50/80/100%. 100% triggers SNS →
  Lambda → terminate ALL `knative-agents-e2e=*` tagged instances.
  Satisfies: R-E2E-COST-4, R-E2E-CLEAN-3.

- [ ] **T-3.4** — `make terraform-init` + `make terraform-apply`
  targets that source `--profile stigen` and refuse to apply outside
  `us-east-2`.
  Satisfies: R-E2E-L2-1.

- [ ] **T-3.5** — Run `terraform apply` once, confirm the
  infrastructure (VPC, IAM, ECR, S3, sweeper, budget) is healthy.
  Satisfies: T-3.1 through T-3.4.

---

## Phase 4 — L2 cloud-init + bootstrap

- [ ] **T-4.1** — `scripts/aws-l2/cloud-init.yaml.tmpl`: containerd
  config registering `io.containerd.kata-fc.v2`, k0s install (single
  controller mode), bpf FS mount, manifest watcher pre-load. Template
  variables: `ARTIFACT_BUCKET`, `IMAGE_TAG`.
  Satisfies: R-E2E-L2-4, R-E2E-L2-7.

- [ ] **T-4.2** — Manifest bundle build: kustomize-render
  cert-manager + SPIRE + operator + samples → tarball, upload to S3.
  Satisfies: R-E2E-L2-6.

- [ ] **T-4.3** — Image bundle build: `docker buildx --platform linux/arm64`
  for all binaries, push to ECR with tag `<git-sha>`.
  Satisfies: R-E2E-L2-9.

- [ ] **T-4.4** — Spot-interruption handler: cloud-init writes a
  systemd unit watching `/spotitn` (the IMDS endpoint); on interrupt
  notice it flushes `/var/log/k0s-bootstrap.READY` and tarballs
  `/var/log/*.log` to S3 before the 2-min deadline.
  Satisfies: R-E2E-CLEAN-4.

- [ ] **T-4.5** — Smoke-test the cloud-init by running
  `aws ec2 run-instances` manually with a test user-data, SSM-exec into
  the instance, verify `/var/log/k0s-bootstrap.READY` appears within
  5 min and `kubectl get nodes` returns Ready.
  Satisfies: R-E2E-L2-3, R-E2E-L2-4.

---

## Phase 5 — L2 test driver

- [ ] **T-5.1** — `l2/cluster.go` `Provision()` per design.md: assumes
  stigen profile, refuses non-`us-east-2`, counts active L2 instances
  before provisioning (refuses if > 3), launches Spot, waits for
  SSM-ready, fetches kubeconfig via SSM. Returns `Env` impl.
  Satisfies: R-E2E-L2-1, R-E2E-L2-2, R-E2E-L2-3, R-E2E-L2-8.

- [ ] **T-5.2** — `l2/cluster.go` `Teardown()`: registered via
  `t.Cleanup`, calls `TerminateInstances`, waits for state ==
  `terminated`, asserts no orphan resources by tag.
  Satisfies: R-E2E-L2-5, R-E2E-CLEAN-1, R-E2E-CLEAN-5.

- [ ] **T-5.3** — `Env` impl wraps the kubeconfig from
  `Provision()`; `Apply`/`Exec` route through `kubectl` against the
  remote cluster.
  Satisfies: R-E2E-DRV-3, R-E2E-DRV-4.

- [ ] **T-5.4** — Re-run all scenarios at L2; gate
  S-WEBHOOK and S-KATA to L2 only via Capability flags. Add a
  cross-check that `uname -r` inside the AgentRun Pod ≠ host kernel.
  Satisfies: R-E2E-SCN-WEBHOOK, R-E2E-SCN-KATA, R-E2E-L2-7.

- [ ] **T-5.5** — Wire `make e2e-l2`. CI workflow `.github/workflows/
  e2e-l2.yml`: triggers on `/test-l2` PR comment, push to main,
  nightly. Uses GHA OIDC to assume the runner role.
  Satisfies: R-E2E-VRF-2, R-E2E-VRF-4.

- [ ] **T-5.6** — `make e2e-clean-aws`: sweeps every
  `knative-agents-e2e=*` tagged resource. Manual escape hatch when
  CI/sweeper failed both belts.
  Satisfies: R-E2E-VRF-3.

---

## Phase 6 — Hardening + handoff

- [ ] **T-6.1** — Coverage gate: CI parses `coverage.go`, fails if any
  R-E2E-* requirement ID is unreferenced.
  Satisfies: R-E2E-VRF-1.

- [ ] **T-6.2** — Flake-rate dashboard: structured test output
  (TAP/JSON) → CloudWatch metric → alarm if rolling 7-day flake rate
  > 1%.
  Satisfies: success criteria from product.md.

- [ ] **T-6.3** — Runbook `docs/runbooks/e2e-stranded-resources.md`
  with the sequence of recovery actions if the sweeper Lambda fails
  AND the budget alarm fails (paranoid third belt for humans).
  Satisfies: R-E2E-CLEAN-1.

- [ ] **T-6.4** — Document the spec graduation: "fullstack-e2e
  graduates from `pending` to `accepted` once L0+L1+L2 have been
  green for 7 consecutive days."

- [ ] **T-6.5** — Migrate the existing `scripts/kind-verify.sh` to be
  a thin wrapper that invokes `go test -tags=e2e_l1 -run TestSmoke`.
  Keep it as the per-commit smoke check; full L1 runs on PR.

---

## Estimated effort

| Phase | Cost | Notes |
|---|---|---|
| 0 — Scaffolding | 0.5 day | Mostly skeletons + interfaces |
| 1 — L0 | 1 day | Most scenarios first land here |
| 2 — L1 | 1.5 days | eBPF integration is the hard part |
| 3 — L2 foundation (Terraform) | 1 day | One-time AWS setup |
| 4 — L2 cloud-init | 1 day | Iteration on bare-metal AMI quirks |
| 5 — L2 test driver | 1 day | mostly mechanical once L1 works |
| 6 — Hardening | 0.5 day | coverage gate + runbook |
| **Total** | **~6.5 days** | spread over ~2 weeks at solo pace |

## Acceptance

The spec is "done" when:

1. `make e2e-l0`, `e2e-l1`, `e2e-l2` all exit 0 on a clean checkout.
2. CI runs L0+L1 on every PR; L2 on the documented triggers.
3. `make e2e-clean-aws` is verified to actually nuke any stranded
   resource.
4. Sweeper Lambda has been observed terminating a wedged test
   instance at least once (proven, not just deployed).
5. Budget alarm has been verified end-to-end by deliberately
   provisioning enough to cross 50% threshold and confirming the
   notification fires.
6. Coverage gate is green: every R-E2E-* ID maps to a test.
