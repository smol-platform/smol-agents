# L2 e2e runbook

The L2 ring runs the full cross-ring scenario set against a real
AWS Spot bare-metal instance — the only place where Kata-fc microVMs
actually boot — and tears the instance down on test exit.

This document covers running L2 locally, troubleshooting the most
common failure modes, and dealing with stranded resources when
something goes wrong.

## Prerequisites

| Requirement | Notes |
|---|---|
| `AWS_PROFILE=stigen` | the sandbox account; set in the shell before any `make e2e-l2*` |
| `AWS_REGION=us-east-2` | enforced by both Make and the L2 driver — wrong region fails fast |
| Terraform module applied | `cd infra/terraform/aws-e2e && terraform apply` once per account; provisions the IAM role, S3 bucket, ECR registry, sweeper Lambda, and budget alarm |
| Manifest + image bundles | `make l2-bundle` rebuilds both; cloud-init pulls them at instance-start |

The Terraform outputs feed two env vars the bundle target needs:

```bash
export L2_ARTIFACT_BUCKET=$(terraform -chdir=infra/terraform/aws-e2e output -raw artifact_bucket)
export L2_ECR_REGISTRY=$(terraform -chdir=infra/terraform/aws-e2e output -raw ecr_registry)
```

## Running L2

| Make target | What runs | Wall time | Cost |
|---|---|---|---|
| `make e2e-l2-smoke` | Provision + sentinel-wait + Teardown (no scenarios) | ~6 min | ~$0.10 |
| `make e2e-l2` | Full Provision + `shared.RunAll` + Teardown | ~12 min | ~$0.22 |
| `make e2e-clean-aws` | Manual sweeper — terminates every `smol-agents-e2e` tagged instance | ~30 sec | $0 |

Smoke is the cheap CI gate for cloud-init drift. Full L2 is the
release-gate variant that exercises every scenario the L1 ring
covers, plus the Kata-isolation scenario that needs a real
bare-metal host.

## Cost guardrails

The Terraform module in `infra/terraform/aws-e2e/` enforces three
defenses:

1. **Sweeper Lambda** — runs every 30 minutes, terminates any
   `smol-agents-e2e` tagged instance older than 1 hour
2. **Nuke Lambda** — fires when the AWS Budget alarm reports >$50
   spent in the current month, terminates every tagged instance
3. **Region pin** — both Make and the Go driver refuse `AWS_REGION`
   other than `us-east-2`, so a typo can't accidentally spawn in
   a region without these guardrails

If budget approaches the cap, the alarm pages via SNS to the email
on the budget. Inspect with `aws budgets describe-budgets --account-id $(aws sts get-caller-identity --query Account --output text)`.

## Troubleshooting

### Sentinel never appears (8-min timeout)

The driver waits up to 8 min for `/var/log/l2-bootstrap.{READY,FAILED}`.
If neither materializes, cloud-init is stuck. From the test log,
grab the InstanceID and:

```bash
aws --profile stigen --region us-east-2 ssm start-session --target i-XXXXXXX
sudo journalctl -u cloud-final -b
sudo tail /var/log/cloud-init-output.log
```

Common causes:
- **kata-static download timed out** — the GitHub release host
  can be slow. The cloud-init currently single-shots the curl;
  retry-on-failure isn't worth wiring until this becomes a real
  flake source.
- **k0s install script flaked** — re-run `curl get.k0s.sh | sh`
  manually to confirm.

### Sentinel reports FAILED

The driver dumps `/var/log/l2-bootstrap.log` on FAILED. The first
`BOOTSTRAP FAILED:` line names the gate that timed out:

| Failed gate | Likely cause |
|---|---|
| `cert-manager deploys` | cert-manager.yaml manifest URL changed; bump the pinned version in `cloud-init.yaml.tmpl` |
| `spire-server pod` | image pull failed (ECR auth) — check `k0s ctr image pull` lines in cloud-init-output.log |
| `spire-agent pods` | psat NodeAttestor mis-configured against the k0s cluster; spire-agent ClusterRole likely missing `nodes/proxy` |
| `operator deploy` | webhook ValidatingWebhookConfiguration references an Issuer cert-manager hasn't materialized yet — usually a race fixed by retry; if persistent, check the kind-webhook overlay's Certificate dnsNames |
| `CRDs established` | controller-gen output drifted from the manifest tarball; rebuild with `make l2-manifests` |

### Stranded instance after test failure

If a panic kills the Go test process before `t.Cleanup` runs, the
instance stays up until the sweeper Lambda terminates it (≤30 min).

To clean immediately:

```bash
make e2e-clean-aws
# or, scoped to one instance:
aws --profile stigen --region us-east-2 ec2 terminate-instances --instance-ids i-XXXXXXX
```

The sweeper logs every termination to CloudWatch under
`/aws/lambda/smol-agents-e2e-sweeper` so you can confirm.

### Spot interruption mid-run

The cloud-init starts a watcher daemon listening on IMDS for
`/spot/instance-action`. On interrupt notice it tarballs `/var/log`
and uploads to `s3://${L2_ARTIFACT_BUCKET}/spot-interrupt-logs/${RUN_ID}.tgz`
before AWS reclaims the instance.

If your test failed with no log on the test host, fetch:

```bash
aws --profile stigen s3 cp s3://${L2_ARTIFACT_BUCKET}/spot-interrupt-logs/${RUN_ID}.tgz - \
  | tar -tz | head
```

`${RUN_ID}` is logged by the driver as `run_id=...` immediately
after Provision succeeds.

## CI integration

The L2 ring is intentionally **not** wired into the per-PR check
suite — the cost adds up across active PRs. Recommended cadence:

- `make e2e-l2-smoke` on every PR that touches
  `scripts/aws-l2/cloud-init.*` or `test/e2e/fullstack/l2/**`
- `make e2e-l2` on `main` once per day (cron-driven workflow)
- `make e2e-l2` manually before tagging a release

The GitHub workflow lives in `.github/workflows/e2e.yml` (the `l2` and
`l2-smoke` jobs).

### Required GitHub Actions configuration

The `l2` / `l2-smoke` jobs assume the AWS workload-identity + bundle
infrastructure exists **and** that the repository (or its org) exposes the
following under **Settings → Secrets and variables → Actions**. These do
**not** transfer when a repo is migrated or forked — a freshly moved repo
must set them again:

| Kind | Name | Value | Source |
|---|---|---|---|
| Secret | `STIGEN_AWS_ACCOUNT_ID` | 12-digit AWS account id of the sandbox account | the account hosting the e2e role |
| Variable | `L2_ARTIFACT_BUCKET` | S3 bucket for manifest bundles | `terraform -chdir=infra/terraform/aws-e2e output -raw artifact_bucket` |
| Variable | `L2_ECR_REGISTRY` | ECR registry host | `terraform -chdir=infra/terraform/aws-e2e output -raw ecr_registry` |

The jobs also assume an IAM role **`smol-agents-e2e-runner`** in that
account whose trust policy allows this repo via GitHub OIDC
(`token.actions.githubusercontent.com`, `sub: repo:<org>/<repo>:*`). The
Terraform module in `infra/terraform/aws-e2e/` provisions the role, bucket,
ECR registry, sweeper Lambda, and budget alarm — apply it once per account.

Symptom when unset: the `l2` job fails at **"configure AWS credentials via
OIDC"** with an empty role ARN (`arn:aws:iam:::role/smol-agents-e2e-runner`).
The `l0`, `l1`, and `coverage-gate` jobs need none of this AWS config.

## When to drop down to L1

If a scenario passes at L0 + L1 but fails at L2, the divergence is
one of:
- **bare-metal-only**: Kata-fc requires `/dev/kvm`; eBPF programs
  may behave differently with kernel 5.15 (kind ships) vs 6.5 (Amazon
  Linux 2023 on c6gd.metal)
- **container-runtime divergence**: kind uses runc, L2 uses
  containerd + Kata for tenant Pods; image-pull layer ordering can
  differ

For these, debug at L2 first (`ssm start-session` + `kubectl logs`)
rather than trying to repro at L1 — the difference is real and
won't surface in kind.
