# Runbook — Local kata-capable k0s cluster

How to stand up a single-node [k0s](https://k0sproject.io) cluster on a Linux
box and make it run **kata-fc** (Kata Containers + Firecracker microVMs) — the
isolation smol-agents uses for untrusted agent workloads. This mirrors the L2
cloud-init recipe (`scripts/aws-l2/cloud-init-*.tmpl`,
`operator/internal/builders/kata_recipe.go`) but for a long-lived dev node you
SSH into (e.g. a bare-metal box with `/dev/kvm`).

The hard part isn't k0s — it's the **devmapper thin-pool** kata-fc needs, and
making it survive reboots. Read [§3](#3-make-the-thin-pool-survive-reboots).

## Prerequisites

- Linux, root (or passwordless `sudo`).
- **`/dev/kvm`** — kata-fc boots a real microVM; needs bare metal or nested virt.
  Without KVM, use the `gvisor` runtimeClass instead.
- `lvm2` + `thin-provisioning-tools` (`thin_check`) — for the devmapper pool.
  The install script apt/dnf-installs these.
- A spare ~55 GB for the loop-backed thin-pool images (`50G` data + `5G` meta,
  sparse — actual usage grows with image layers).

Reference node (verified 2026-05-21): Ubuntu 25.10, k0s v1.34.2, containerd
1.7.29, kernel 6.17; kata-static 3.10.0.

## 1. Install k0s (single node)

`--single` makes one node both controller and worker (no taint), which is what
you want locally.

```bash
curl -sSf https://get.k0s.sh | sh
k0s install controller --single          # add `-c /etc/k0s/k0s.yaml` for custom config
systemctl start k0scontroller
# First boot extracts etcd + kube-apiserver and generates PKI — wait for it:
timeout 300 sh -c 'until k0s status >/dev/null 2>&1; do sleep 5; done'
timeout 120 sh -c 'until [ -f /var/lib/k0s/pki/admin.conf ]; do sleep 2; done'
```

The cluster client is `k0s kubectl ...` (admin kubeconfig is
`/var/lib/k0s/pki/admin.conf`; `export KUBECONFIG=$(k0s kubeconfig admin)` for a
plain `kubectl`). k0s's containerd socket is `/run/k0s/containerd.sock`, and any
`*.toml` in **`/etc/k0s/containerd.d/`** is auto-imported into its containerd
config — that's how kata is wired in below.

```bash
k0s kubectl get nodes        # STATUS Ready
```

## 2. Install the kata-fc runtime

Use the repo script — it mirrors the hardened L2 recipe and is idempotent:

```bash
sudo bash scripts/install-kata-k0s.sh
```

It performs:

1. **kata-static** bundle → `/opt/kata` (`firecracker`, `cloud-hypervisor`,
   `containerd-shim-kata-v2`), symlinked onto `PATH`.
2. **devmapper thin-pool** — `kata-fc` needs a *block-device* rootfs, but k0s's
   containerd ships `overlayfs` only. The script truncates two loopback images
   under `/var/lib/containerd/devmapper/`, attaches them with `losetup`, and
   `dmsetup create kata-thinpool`. (Why devmapper and not overlayfs: kata-fc
   passes the rootfs as a block device, so an overlayfs snapshotter makes kata
   pods fail with `FailedCreatePodSandBox … ENOENT rootfs`. devmapper is the
   canonical kata-fc setup; the nydus/tarfs route was tried and abandoned.)
3. **containerd drop-ins** in `/etc/k0s/containerd.d/`:
   - `devmapper.toml` registers the snapshotter (`pool_name = "kata-thinpool"`),
   - `kata-fc.toml` registers the `kata-fc` runtime (`io.containerd.kata.v2`,
     `snapshotter = "devmapper"`).
4. **restarts k0s** so containerd reloads the drop-ins — **this bounces every
   pod on the node**. Pick your moment on a busy box.

> If you reach the node through a tunnel that itself runs *in* this cluster
> (e.g. a `cloudflared`/warp connector pod), the k0s restart will **sever your
> SSH** for ~30–60 s. Run the script detached so it finishes regardless:
> `sudo systemd-run --unit=kata-install --collect --wait --pipe bash scripts/install-kata-k0s.sh`

## 3. Make the thin-pool survive reboots

**The loop-backed thin-pool is runtime-only state.** The `.img` files persist,
but the loop devices + the `kata-thinpool` dm target vanish on reboot. After a
reboot the devmapper snapshotter loads with `error`:

```
failed to query pool "/dev/mapper/kata-thinpool": no such device or address
```

and every kata-fc pod breaks. Fix it with a oneshot systemd unit ordered
**before** k0s. `scripts/fix-kata-gti.sh` installs exactly this (and re-runs
step 2 + registers the RuntimeClass + verifies):

```bash
sudo bash scripts/fix-kata-gti.sh
```

The unit it installs (`/etc/systemd/system/kata-thinpool.service`) recreates the
pool from the persisted images on every boot:

```ini
[Unit]
Description=Recreate loop-backed kata devmapper thin-pool before k0s
DefaultDependencies=no
After=local-fs.target systemd-modules-load.service
Before=k0scontroller.service
ConditionPathExists=/var/lib/containerd/devmapper/data.img
[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/usr/local/sbin/kata-thinpool-up.sh
[Install]
WantedBy=k0scontroller.service
```

```bash
sudo systemctl enable kata-thinpool.service   # fix-kata-gti.sh already does this
```

## 4. Register the kata-fc RuntimeClass

(The smol-agents operator can also create this; do it manually for a bare node.)

```bash
k0s kubectl apply -f - <<'YAML'
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata: { name: kata-fc }
handler: kata-fc
overhead:
  podFixed: { cpu: 250m, memory: 256Mi }
YAML
```

> If you just restarted k0s, the apiserver may not be up yet
> (`dial tcp 127.0.0.1:6443: connect: connection refused`). Wait for it:
> `until k0s kubectl get --raw /healthz >/dev/null 2>&1; do sleep 2; done`

## 5. Verify

```bash
# snapshotter must be "ok", not "error":
k0s ctr -a /run/k0s/containerd.sock -n k8s.io plugins ls | grep devmapper
dmsetup status kata-thinpool                 # active thin-pool line
/opt/kata/bin/kata-runtime check             # "System can currently create Kata Containers"
k0s kubectl get runtimeclass                 # kata-fc present
```

Smoke test — the pod's kernel must differ from the host (proof a microVM booted):

```bash
k0s kubectl run kata-uname --restart=Never --image=docker.io/library/busybox:1.36 \
  --overrides='{"spec":{"runtimeClassName":"kata-fc"}}' -- uname -r
k0s kubectl logs kata-uname        # e.g. 6.1.62 (Kata guest kernel)
uname -r                           # e.g. 6.17.0-29-generic (host) — must differ
k0s kubectl delete pod kata-uname
```

## Troubleshooting

| Symptom | Cause | Fix |
|---|---|---|
| `devmapper` snapshotter `error`; `no such device kata-thinpool` | Loop-backed pool dropped on reboot/restart | `sudo bash scripts/fix-kata-gti.sh`; ensure `kata-thinpool.service` is enabled (§3) |
| RuntimeClass apply → `6443: connection refused` | apiserver still starting after a k0s restart | wait for `/healthz` (§4) |
| SSH dies mid-setup | You reach the node through a k0s-hosted tunnel; k0s restart kills it | run detached via `systemd-run … --wait`; reconnect after ~1 min |
| kata pod `Pending` → `FailedCreatePodSandBox … ENOENT … rootfs` | overlayfs snapshotter (not devmapper) serving kata-fc | confirm `kata-fc.toml` sets `snapshotter = "devmapper"` and the pool is healthy |
| `kata-runtime check` fails on `/dev/kvm` | no KVM (VM without nested virt) | use `gvisor` runtimeClass, or enable nested virtualization |
| pods stuck `Error`/`Unknown` right after step 2/3 | old sandboxes killed by the containerd restart | transient — controllers recreate them; the node returns to a healthy Running set |

## Related

- `scripts/install-kata-k0s.sh` — installs kata-fc on an existing k0s node.
- `scripts/fix-kata-gti.sh` — restores the pool + boot persistence + RuntimeClass.
- [`docs/runbooks/agent-node-pools.md`](agent-node-pools.md) — `AgentNodePool` →
  kata nodes via the operator (the production path).
- [`docs/INSTALL.md`](../INSTALL.md) — full smol-agents install (Linux kernel,
  SPIRE, eBPF prerequisites).
