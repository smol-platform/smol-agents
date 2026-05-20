# Bottlerocket bootstrap container — investigation notes

## Status

Bottlerocket smoke is **SSM-Online-only**. Two approaches were
attempted to extend it to the full health gate; both hit
fundamental Bottlerocket constraints. The smoke remains at the
"AMI provisions + SSM reachable" level. AL2023 / Ubuntu / Flatcar
all pass the full health gate in their own variants of the smoke;
Bottlerocket's gap is a known + documented limitation.

### Approach 1: host-container running k0s control plane

See `scripts/aws-l2/bottlerocket-k0s-host/`. Built end-to-end;
each layer worked until pod scheduling: Bottlerocket's SELinux
super_t label (the most permissive label available to user-defined
host-containers) does not authorise nested containerd to mkdir
under its task-rootfs tree. Manifests as `mkdirat ...rootfs/proc:
permission denied`. Custom Bottlerocket AMI build is the only path.

### Approach 2: Bottlerocket aws-k8s worker joining external k0s

See `scripts/aws-l2/bottlerocket-worker.toml.tmpl` +
`test/e2e/fullstack/l2/bottlerocket_worker_test.go`. The worker
boots and reaches Bottlerocket's `pluto` settings-generator,
which times out at exactly 5 min with "Timed out retrieving
private DNS name from EC2: deadline has elapsed" — even with
ec2:DescribeInstances granted to the instance role and
explicit `[settings.aws.region]` + `node-ip` in user-data. Pluto
appears to require an EKS-cluster context (cluster tags, etc.)
that a self-hosted k0s control plane doesn't satisfy.

The worker test is committed but skipped by default (gated on
`L2_RUN_BOTTLEROCKET_WORKER=1`); the IAM grant for
ec2:DescribeInstances is left in terraform for any future
Bottlerocket-related integration.

### Approach 3: kubeadm bootstrap-container + standalone-mode (the AWS-recommended pattern)

Per bottlerocket-os/bottlerocket#4517, AWS's recommended pattern
for self-managed control planes on Bottlerocket is "static pods
+ standalone mode" via a kubeadm-driven bootstrap-container —
exactly what EKS-Anywhere's [bottlerocket-bootstrap](https://github.com/aws/eks-anywhere-build-tooling/tree/main/projects/aws/bottlerocket-bootstrap)
does. We implemented this end-to-end at
`scripts/aws-l2/bottlerocket-kubeadm/` (Dockerfile + entrypoint
that runs `kubeadm init phase ...`, stages static pods via
`apiclient set kubernetes.static-pods.<name>.manifest=<b64>`,
fires the `kubernetes.{api-server,cluster-certificate,
bootstrap-token,authentication-mode,standalone-mode}` cascade,
applies our manifests, runs the health gate).

**Result: also blocked by pluto.** Even on the kubeadm path with
`[settings.kubernetes.standalone-mode] = true` AND every
`kubernetes.*` setting pluto would compute pre-populated in
user-data (cluster-name, cluster-dns-ip, api-server, max-pods,
etc.), pluto STILL times out at 5 min. The pluto service has a
hardcoded EC2 DescribeInstances probe that runs regardless of
which kubernetes.* settings are pre-set; only its EKS-cluster
DescribeCluster lookup is gated on cluster-name.

EKS-Anywhere works around this by **shipping a custom Bottlerocket
build** with their own pluto patches — visible in their build
tooling at `aws/eks-anywhere-build-tooling`. Stock Bottlerocket
AMIs from `/aws/service/bottlerocket/aws-k8s-*/...` will not
boot without an EKS context.

The kubeadm bootstrap-container code, ECR-Public image
(`public.ecr.aws/f4r4b7z2/smol-agents-bottlerocket-kubeadm`),
and TOML template are preserved in the tree as a starting
point for any future investigation that involves a custom
Bottlerocket AMI build.

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

## Host-container k0s investigation (2026-05-09)

The host-container approach was implemented end-to-end at
`scripts/aws-l2/bottlerocket-k0s-host/`. Each layer of the stack
validates cleanly until the final pod-start step. The blocker is
in Bottlerocket's MAC policy.

### Working layers

| Layer | Status | Notes |
|---|---|---|
| `[settings.host-containers.k0s] superpowered=true` registers | ✅ | systemd `host-containers@k0s.service` created |
| Image pull from ECR Public | ✅ | host-containerd doesn't have ECR-private IAM, but Public works |
| Container starts; tini reaps zombies | ✅ | warning about not-PID-1 is benign |
| Wrapper script runs, sources user-data | ✅ | from `/.bottlerocket/host-containers/current/user-data` |
| AWS CLI + ECR creds | ✅ | IAM-via-IMDS works |
| S3 manifest tarball pull | ✅ | `aws s3 cp` succeeds |
| k0s install + start | ✅ | requires tmpfs over `/var/lib/k0s` because container overlayfs rejects `setxattr(security.selinux)` on extracted runc/containerd-shim binaries |
| etcd + kine + kube-apiserver come up | ✅ | apiserver initializes RBAC, accepts requests |
| kubelet starts | ✅ on k0s 1.33; ❌ on 1.34+ | k0s 1.34+ kubelet refuses cgroup v1; Bottlerocket exposes cgroup v1 to host-containers via `WithHostNamespace(PID)`. Pin k0s 1.33 (tolerates v1 with deprecation warning) and create `/dev/kmsg` (`mknod c 1 11`) — kubelet reads it for kmsg events |
| apiserver registers node, schedules pods | ✅ | kube-router daemonset gets scheduled |
| **k0s containerd creates pod rootfs** | **❌** | `error mounting "proc" to rootfs: mkdirat /run/k0s/containerd/.../rootfs/proc: permission denied`. The k0s-internal containerd is denied creating subdirs in its task-rootfs tree by Bottlerocket's SELinux policy (`super_t` doesn't extend to nested containerd's mount-namespace setup). |

### Why this is a hard wall, not a tweak

- `superpowered = true` in Bottlerocket grants all caps + all devices + host PID/net/cgroup namespaces, BUT the SELinux label `system_u:system_r:super_t:s0:c0.c1023` is the most permissive label Bottlerocket's policy ships with — it's still scoped to the host-container's role and doesn't authorize creating arbitrary nested-rootfs trees in /run/k0s/containerd.
- The actual fix would require modifying Bottlerocket's SELinux policy to permit nested-containerd, which means rebuilding Bottlerocket from source. Not an in-user-data option.
- This is implicitly why no public k0s-on-Bottlerocket project exists. The two stock Bottlerocket variants (`aws-k8s-*` for kubelet workers + `aws-ecs-*` for ECS workers) join an existing control plane; they don't host one.

### What works at the architecture level

The implementation is correct and reusable for distributions with looser MAC policies:

- `scripts/aws-l2/bottlerocket-k0s-host/Dockerfile` — minimal layer over `docker.io/k0sproject/k0s` with awscli + curl
- `scripts/aws-l2/bottlerocket-k0s-host/wrapper.sh` — full bootstrap (config, ECR auth via containerd `config_path` + `hosts.toml`, manifest fetch, k0s start, health gate, sentinel)
- `scripts/aws-l2/bottlerocket-userdata.toml.tmpl` — host-container TOML with `superpowered = true` and base64 user-data

If you re-enable the host-container path, the smoke gets through everything up to pod scheduling. If the workload doesn't need to schedule pods (e.g., a different control-plane shape), this might still be useful.

## Conclusion

The Bottlerocket smoke stays at "AMI launches + SSM-Online" until either:

1. **Drop the single-node-control-plane requirement.** Use the EKS Bottlerocket variant (`aws-k8s-1.30/1.31/...`), provision a separate EKS cluster (Terraform), and have the Bottlerocket node join as a worker. Different test, different scope.
2. **Custom Bottlerocket build with permissive SELinux policy.** Requires forking Bottlerocket and shipping our own AMI.

Both are out of scope for an L2 smoke whose purpose is "verify the AMI is provisionable + runs our cluster shape". For the cluster shape we already have AL2023, Ubuntu, and Flatcar all passing the full health gate — Bottlerocket as SSM-Online-only is an acceptable gap.

References:
- https://bottlerocket.dev/en/os/1.x/api/settings/host-containers/
- https://github.com/bottlerocket-os/bottlerocket/discussions/2168
- https://github.com/bottlerocket-os/bottlerocket/discussions/3034
- https://github.com/bottlerocket-os/bottlerocket/issues/4517 — self-managed control plane attempt
