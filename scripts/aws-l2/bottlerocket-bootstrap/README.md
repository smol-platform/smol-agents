# Bottlerocket bootstrap container — investigation notes

## Status

Bottlerocket smoke is **SSM-Online-only** (driver writes the
sentinel via SSM after Provision succeeds). Full bring-up
(cert-manager + SPIRE + operator + CRDs) on Bottlerocket is **not
implemented**, blocked on architectural mismatch documented below.

## What works

- Bottlerocket AMI launch with our IAM role.
- SSM control container reaches Online (when bootstrap-containers
  don't block boot).
- `apiclient exec admin <cmd>` works once `[settings.host-containers.admin]
  enabled = true` is in user-data.
- Bootstrap-container stdout/stderr go to the systemd journal,
  retrievable via:
  ```
  apiclient exec admin journalctl \
    -D /.bottlerocket/rootfs/var/log/journal/ \
    -u bootstrap-containers@<name>.service
  ```
- ECR-Public image source works for bootstrap-containers (no IAM
  needed); ECR-private also works if the instance role has
  `ecr:GetAuthorizationToken` + `ecr:BatchGetImage`.

## What blocks the full bring-up

**Bootstrap-containers are one-shot, not daemons.** The unit
`bootstrap-containers@l2.service` blocks subsequent boot units
(including the SSM control container's startup) until the
container exits. A `sleep infinity` after the sentinel-write
permanently hangs boot and SSM never comes Online.

The smoke needs k0s running as a long-lived process to satisfy
the health gate (cert-manager + SPIRE + operator + CRDs all
require an active control plane). The only Bottlerocket
mechanisms for long-running host workloads are:

1. **Custom host-containers** — define `[settings.host-containers.k0s]`
   in user-data. Runs as `host-containers@k0s.service`, persists,
   has access to `/.bottlerocket/rootfs`. This is the intended
   path for our use case but adds a non-trivial engineering
   effort: the container has to run k0s as PID 1 (no systemd
   inside), supervise its child processes (etcd, kube-apiserver,
   kubelet), and survive container restarts.

2. **Worker-joining-EKS pattern** — Bottlerocket's k8s variant
   joins an existing EKS control plane. Not a single-node smoke;
   would require provisioning an EKS cluster separately.

## What was tried

- **bootstrap-container with `sleep infinity`** (current
  Dockerfile + entrypoint.sh): hangs boot, SSM never reachable.
- **bootstrap-container exiting after sentinel write**: bootstrap
  succeeds but k0s dies with the container; health gate fails.
- **bootstrap-container as ECR-private source** without
  ecr:GetAuthorizationToken on the IAM role: pull fails before
  entrypoint runs (no captured output without admin enabled).
- **bootstrap-container as ECR-Public source**: pull succeeds,
  but same hang/exit dilemma.

## Files

- `Dockerfile`         — Ubuntu 24.04 base with AWS CLI + k0s
                         + entrypoint.sh
- `entrypoint.sh`      — current (one-shot) bootstrap script;
                         writes sentinel, exits 0 (so boot
                         proceeds); k0s does NOT persist across
                         container exit
- (this file)          — investigation notes

## Re-enabling investigation

To reproduce the debugging session:

```bash
# Enable admin container in TOML for SSH-via-SSM access:
# scripts/aws-l2/bottlerocket-userdata.toml.tmpl
[settings.host-containers.admin]
enabled = true

# Provision + wait for SSM-Online:
L2_DISTRO=bottlerocket L2_INSTANCE_TYPE=c7g.large \
  L2_INSTANCE_MARKET=on-demand L2_KEEP_INSTANCE=1 \
  go test -tags=e2e_l2 -run 'TestL2_Smoke$' -v ./test/e2e/fullstack/l2/...

# Once Online, get bootstrap-container logs:
aws ssm send-command --instance-ids <id> --document-name AWS-RunShellScript \
  --parameters 'commands=["apiclient exec admin journalctl -D /.bottlerocket/rootfs/var/log/journal/ -u bootstrap-containers@l2.service --no-pager"]'

# Or run logdog to bundle full diagnostics:
aws ssm send-command --instance-ids <id> --document-name AWS-RunShellScript \
  --parameters 'commands=["apiclient exec admin logdog"]'
# Output lands in /.bottlerocket/support/bottlerocket-logs.tar.gz
```

## Next steps (when ready to invest)

1. Convert from bootstrap-container to custom host-container
   (`[settings.host-containers.k0s]`) so k0s persists.
2. Build the host-container image to run k0s as PID 1 with a
   minimal supervisor (e.g., `tini` or k0s itself as the
   entrypoint).
3. Mount `/.bottlerocket/rootfs/var` so k0s's data dir persists
   on the host.
4. Drop the bootstrap-container entirely (or keep for one-shot
   setup like ECR auth file generation).
5. Validate the same health gate as AL2023/Ubuntu/Flatcar.

References:
- https://bottlerocket.dev/en/os/1.x/api/settings/host-containers/
- https://github.com/bottlerocket-os/bottlerocket/discussions/2168
- https://github.com/bottlerocket-os/bottlerocket/discussions/3034
