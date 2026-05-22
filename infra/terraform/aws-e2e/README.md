# aws-e2e — Terraform module for the L2 e2e ring

One-time AWS infrastructure for the fullstack-e2e L2 ring.
Per-run resources (the actual bare-metal Spot instances) are NOT
managed here — those are created/destroyed by the L2 test driver
in Go. This module provides the long-lived plumbing that the test
driver needs: VPC, IAM, S3, ECR, sweeper Lambda, budget alarm.

## Pinned

- **Account**: `smol-agents` sandbox
- **Region**: `us-east-2` (Ohio)
- **Monthly cap**: $50 — enforced by AWS Budget; 80% notify, 100% nuke
- **Instance type**: `c6gd.metal` only (cheapest bare-metal arm64)

These are validated at the Terraform variable level; changing them
requires editing `variables.tf`'s validation blocks.

## Bootstrap (one-time)

The S3 backend is a chicken-and-egg: Terraform stores its state in
the bucket but the bucket is created by Terraform. To resolve:

1. Comment out the `backend "s3"` block in `versions.tf`.
2. Build the Lambda binaries (see below).
3. `terraform init`
4. `terraform apply` — creates the bucket + lock table + everything else.
5. Uncomment the backend block.
6. `terraform init -migrate-state` — migrates local state to S3.

## Building the Lambda binaries

The sweeper + nuke Lambdas are Go programs cross-compiled to arm64
Linux:

```bash
cd sweeper
GOOS=linux GOARCH=arm64 go build -o bootstrap ./
cd ../budget
GOOS=linux GOARCH=arm64 go build -o bootstrap ./
```

`archive_file` packages each `bootstrap` into a zip that's uploaded
to Lambda. `terraform apply` re-builds + re-uploads when source
hashes change.

## Apply

```bash
make -C infra/terraform/aws-e2e build-lambdas
terraform -chdir=infra/terraform/aws-e2e init
terraform -chdir=infra/terraform/aws-e2e plan
terraform -chdir=infra/terraform/aws-e2e apply
```

## Tear down

```bash
terraform -chdir=infra/terraform/aws-e2e destroy
```

The sweeper + nuke Lambdas terminate any active L2 instances, so
this won't strand anything. The S3 bucket has a 7-day expiration on
all objects so reapplying within a week reuses cached artifacts;
beyond a week artifacts re-upload from CI.

## Acceptance criteria mapped

- R-E2E-CLEAN-2 → `sweeper.tf`
- R-E2E-CLEAN-3 → `budget.tf` (nuke Lambda)
- R-E2E-COST-3 → `network.tf` single-AZ, no NAT
- R-E2E-COST-4 → `budget.tf` ($50 cap, 50/80/100% thresholds)
- R-E2E-L2-1 → `variables.tf` profile/region locks + `providers.tf`
- R-E2E-L2-9 → `storage.tf` (ECR repos)
