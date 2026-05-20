# Installing smol-agents

This guide covers four install paths, in order of how much infrastructure you
already have:

1. **Developer shell** — fastest local iteration with `devenv`.
2. **Build from source** — produce binaries and container images.
3. **Local Kubernetes (kind)** — full stack including SPIRE and Knative.
4. **Production cluster** — Helm install onto an existing cluster with
   gVisor, SPIRE, and Knative already running.

If you just want to read the system end-to-end first, the source of truth is
`.spec-workflow/specs/smol-agents-platform/{product,requirements,design}.md`.

---

## 1. Prerequisites

### 1.1 Operating system

| Component                   | OS / version requirement                              |
|-----------------------------|-------------------------------------------------------|
| `agent` binary (production) | Linux only — eBPF programs require Linux kernel       |
| `agent` binary (dev/test)   | Linux, macOS, Windows (eBPF runtime is no-op on non-Linux) |
| `secret-proxy` binary       | Linux only in production (uses `SO_PEERCRED`); macOS for unit tests |
| `agentctl`                  | Linux, macOS, Windows                                 |
| Go toolchain                | macOS, Linux, Windows                                 |
| Quint model checker         | Anywhere with Node.js 18+                             |

### 1.2 Linux kernel requirements (for the running agent)

| Kernel feature             | Min version | Notes                                       |
|----------------------------|-------------|---------------------------------------------|
| BPF type format (BTF)      | 5.2         | Required for CO-RE                          |
| BPF ring buffer            | 5.8         | Falls back to perf events on older kernels with reduced throughput |
| `BPF_PROG_TYPE_RAW_TRACEPOINT` | 4.17    | Used by `syscalls.bpf.c`                    |
| `bpf_get_current_cgroup_id`| 4.18        | Used by all BPF programs                    |
| `userfaultfd` (gVisor)     | 5.7+        | Some gVisor features prefer this            |

Recommended kernel: **5.15+ (Ubuntu 22.04, Debian 12, RHEL 9, Bottlerocket)**.

Verify on a target node:

```bash
uname -r
# Expect: 5.15.x or newer

ls /sys/kernel/btf/vmlinux
# Expect: file exists; presence of BTF means CO-RE will work without bundled vmlinux.h

bpftool feature probe kernel | grep -E "ring_buf|BTF|raw_tracepoint"
```

### 1.3 Toolchain prerequisites

#### Required for *building* smol-agents

| Tool        | Min version | Purpose                                              |
|-------------|-------------|------------------------------------------------------|
| Go          | 1.24        | Compiles all binaries                                |
| clang       | 14          | Compiles BPF C → BPF bytecode                        |
| llvm        | 14          | Required by clang's `-target bpf`                    |
| `bpftool`   | recent      | Generates `vmlinux.h` from kernel BTF                |
| make        | any         | Drives the build                                     |
| git         | any         | Source control                                       |

#### Required for *deploying* smol-agents

| Tool        | Min version | Purpose                                              |
|-------------|-------------|------------------------------------------------------|
| kubectl     | 1.28        | Cluster access                                       |
| Helm        | 3.14        | Installs the chart                                   |
| Kustomize   | 5.x         | Overlay-based install (optional alternative to Helm) |
| Docker / Buildx | 24+     | Builds container images                              |

#### Required for *verification*

| Tool        | Min version | Purpose                                              |
|-------------|-------------|------------------------------------------------------|
| Quint       | 0.20        | Formal model checker (`npm i -g @informalsystems/quint`) |
| Node.js     | 18          | Runs Quint                                           |
| `golangci-lint` | 1.60    | Lints Go (CI requires it)                            |
| `gofumpt`   | latest      | Formats Go (CI requires it)                          |
| Java 17+    | optional    | Apalache backend for Quint (faster on large state spaces) |

### 1.4 Cluster prerequisites

A target Kubernetes cluster must have:

| Capability                            | How to verify                                                            |
|---------------------------------------|--------------------------------------------------------------------------|
| Kubernetes ≥ 1.28                     | `kubectl version`                                                        |
| `RuntimeClass` API enabled (default)  | `kubectl api-resources` includes `runtimeclasses`                        |
| **Sandbox runtime** on every node     | One of: `kata-fc` (default), `kata-cc-isolation` (AKS), `kata` (OpenShift), `gvisor` (GKE). See §1.4.1. |
| **`/dev/kvm` exposed on every node**  | Required for *any Kata variant*. Bare metal, KVM-capable VM family, or nested virt. Skip if using `gvisor`. |
| **SPIRE** with workload API on CSI    | `kubectl get pods -n spire-server` shows running agents and server       |
| ClusterSPIFFEID CRD installed         | `kubectl api-resources \| grep clusterspiffeids`                         |
| **Knative Serving** ≥ 1.15            | `kubectl get ksvc` works (only required for `mode: knative`)             |
| LoadBalancer or Ingress               | Required for `mode: knative` external traffic                            |
| OTLP-compatible collector             | Optional; agents tolerate `otlpEndpoint=""`                              |

#### 1.4.1 Sandbox runtime by distro / cloud

The chart's `sandbox.preset` selects RuntimeClass + handler per
environment. Pick the row that matches yours:

| `sandbox.preset`     | Target environment                       | RuntimeClass        | Install path                                                                          | KVM required |
|----------------------|------------------------------------------|---------------------|---------------------------------------------------------------------------------------|--------------|
| `generic`            | Standard Linux nodes (default)           | `kata-fc`           | [kata-deploy Helm chart](https://github.com/kata-containers/kata-containers/tree/main/tools/packaging/kata-deploy/helm-chart) | yes |
| `bare-metal`         | Bare metal, no virtualization layer      | `kata-fc`           | kata-deploy or [manual install](https://github.com/kata-containers/kata-containers/blob/main/docs/install/README.md) | yes (native) |
| `eks-bottlerocket`   | EKS Bottlerocket nodes                   | `kata-fc`           | Bottlerocket Kata variant AMIs (`bottlerocket-aws-k8s-*-aarch64-kata`)                | yes (.metal or nitro-tpm) |
| `aks`                | Azure Kubernetes Service Pod Sandboxing  | `kata-cc-isolation` | [AKS Pod Sandboxing](https://learn.microsoft.com/azure/aks/use-pod-sandboxing)        | yes (Dv5 or later) |
| `openshift-sandboxed`| OpenShift                                | `kata`              | [OpenShift Sandboxed Containers Operator](https://docs.openshift.com/sandboxed-containers/) | yes |
| `k3s`                | K3s / k3os                               | `kata-fc`           | kata-deploy with `--set k3s.enabled=true`                                             | yes |
| `talos`              | Talos Linux                              | `kata-fc`           | `kata-containers` Talos system extension                                              | yes |
| `gke`                | GKE managed nodes (no `/dev/kvm`)        | `gvisor`            | [GKE Sandbox](https://cloud.google.com/kubernetes-engine/docs/concepts/sandbox-pods)  | **no** |
| `generic-gvisor`     | Any Linux node, prefers gVisor over Kata | `gvisor`            | [gVisor production install](https://gvisor.dev/docs/user_guide/install/)              | **no** |

The chart only renders a `RuntimeClass` object when
`sandbox.installRuntimeClass=true` (set automatically by the
`bare-metal`, `k3s`, and `generic-gvisor` presets). Production clusters
should install the runtime via its own operator/chart and leave
`installRuntimeClass=false`.

Notes:
- The chart does **not** install Kata or gVisor binaries — those are
  per-node concerns and should be managed by the platform team using
  the upstream installers above.
- The chart does **not** install SPIRE (deliberately — most platforms have an
  existing tenant). To install SPIRE for a fresh cluster, use the
  [official Helm chart](https://artifacthub.io/packages/helm/spiffe/spire).

### 1.5 Identity prerequisites (SPIRE)

smol-agents expects:

- **Trust domain** matches the value of `trustDomain` in `values.yaml`
  (default `stigen.ai`).
- **Workload API** is mounted into agent Pods via the SPIRE CSI driver at
  `/run/spire/agent-sockets/api.sock`.
- **ClusterSPIFFEID** matches the agent ServiceAccount and namespace. The
  default template renders:
  ```yaml
  spiffeIDTemplate: "spiffe://stigen.ai/ns/{{ .Release.Namespace }}/sa/{{ .Values.serviceAccount.name }}"
  ```
- **Selectors** include `k8s:ns:<namespace>` and
  `k8s:sa:<serviceaccount>`. The chart renders these by default.

Verify SPIRE is healthy before installing smol-agents:

```bash
kubectl exec -n spire-server -it deploy/spire-server -- /opt/spire/bin/spire-server agent list
# Should list at least one node-local agent.
```

---

## 2. Install path 1 — Developer shell with devenv

For local development of agents *and* the platform itself.

```bash
# Install devenv (one-time):
#   https://devenv.sh/getting-started/

cd smol-agents
devenv shell           # drops you into a shell with Go, clang, kubectl, helm, kind, quint pre-installed
make all               # tidy + fmt + vet + lint + build + test
make verify-formal     # run the Quint model checker
```

Output of `make all` includes:

- `bin/agent`, `bin/secret-proxy`, `bin/agentctl`
- Test pass/fail per package
- `coverage.out`

`devenv` reads `devenv.nix` at the repo root and pins all toolchain versions
declaratively. To rebuild the env after a change, run `devenv shell` again.

---

## 3. Install path 2 — Build from source (no devenv)

If you don't want devenv, install prerequisites manually:

### 3.1 macOS (Homebrew)

```bash
brew install go@1.24 clang-format llvm bpftool kubernetes-cli helm kustomize kind node
npm install -g @informalsystems/quint
```

### 3.2 Linux (Ubuntu 24.04)

```bash
sudo apt-get update
sudo apt-get install -y golang-1.24-go clang llvm linux-tools-common linux-tools-generic bpftool make git
curl -L https://github.com/kubernetes-sigs/kustomize/releases/download/kustomize%2Fv5.4.0/kustomize_v5.4.0_linux_amd64.tar.gz | tar xz -C /usr/local/bin/
curl -fsSL https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-3 | bash
curl -Lo /usr/local/bin/kind https://kind.sigs.k8s.io/dl/latest/kind-linux-amd64 && chmod +x /usr/local/bin/kind
curl -fsSL https://deb.nodesource.com/setup_20.x | sudo -E bash - && sudo apt-get install -y nodejs
sudo npm install -g @informalsystems/quint
```

### 3.3 Build

```bash
cd smol-agents
go mod download              # fetch deps
make tidy                    # ensure go.mod is consistent
make build                   # → bin/{agent,secret-proxy,agentctl}
make bpf                     # build BPF objects (Linux only; needs /sys/kernel/btf/vmlinux)
make test                    # unit tests
make test-integration        # integration tests (build tag)
make verify-formal           # Quint
```

### 3.4 Build container images

```bash
docker build -f deploy/docker/agent.Dockerfile         -t smol-agents/agent:0.1.0 .
docker build -f deploy/docker/secret-proxy.Dockerfile  -t smol-agents/secret-proxy:0.1.0 .
docker build -f deploy/docker/agentctl.Dockerfile      -t smol-agents/agentctl:0.1.0 .
```

For multi-arch with Buildx:

```bash
docker buildx build --platform linux/amd64,linux/arm64 \
    -f deploy/docker/agent.Dockerfile \
    -t your-registry/smol-agents/agent:0.1.0 --push .
```

---

## 4. Install path 3 — Local cluster (kind) end-to-end

Use this to validate the entire stack on a laptop.

### 4.1 What you get

- 3-node kind cluster
- Knative Serving + Kourier networking
- SPIRE server + agent (DaemonSet) with trust domain `stigen.ai`
- smol-agents chart installed in namespace `smol-agents`
- A working agent reachable via Knative

### 4.2 Caveat about Kata + Firecracker on kind

kind runs each node as a Docker container, so getting `/dev/kvm` into
that container requires either bare-metal Linux or nested virt. Local
options, in order of fidelity:

**Option A (recommended for laptops): override the RuntimeClass to `runc`**.
This relaxes R-SBX-1 — fine for development, never for production.

```bash
helm upgrade --install agents deploy/helm \
    --namespace smol-agents --create-namespace \
    --set sandbox.runtimeClass=runc \
    --set sandbox.allowHostRuntime=true
```

**Option B: bare-metal Linux + `kind` with KVM passthrough.** Pass
`--device /dev/kvm` to the kind node Docker run, install kata-deploy,
and use `sandbox.preset=generic`. This validates the production sandbox
path locally.

**Option C: switch to gVisor for local-only**: `--set sandbox.preset=generic-gvisor`.
gVisor needs no nested virt and works on most kind hosts via the
`runsc` userspace shim — useful if you want to keep R-SBX-1 active
without provisioning KVM.

### 4.3 Run the bundled bootstrap script

```bash
./test/e2e/scripts/up-kind.sh
```

This script (read it before running — it makes destructive cluster changes):

1. `kind create cluster --name smol-agents`
2. Installs Knative Serving CRDs, core, and Kourier
3. Installs SPIRE CRDs and the SPIRE chart with `trustDomain=stigen.ai`
4. `helm upgrade --install agents deploy/helm --namespace smol-agents --create-namespace`
5. Waits for the Knative Service to be Ready

Tear down with:

```bash
kind delete cluster --name smol-agents
```

### 4.4 Verify

```bash
# 1. Pod is Running and Ready
kubectl get pods -n smol-agents -o wide

# 2. The Pod has a SPIFFE SVID issued
kubectl exec -n smol-agents <agent-pod> -c agent -- /agentctl status

# 3. Knative Service URL responds
SVC=$(kubectl get ksvc -n smol-agents -o jsonpath='{.items[0].status.url}')
curl -k $SVC/healthz   # expects 200 ok
curl -k $SVC/readyz    # expects 200 ok
```

---

## 5. Install path 4 — Production cluster

Assumes the cluster prerequisites in §1.4 are already met.

### 5.1 Make images available

Push the three images to a registry that your cluster's nodes can pull from:

```bash
REG=your-registry.example.com
TAG=0.1.0
for cmd in agent secret-proxy agentctl; do
  docker buildx build --platform linux/amd64,linux/arm64 \
    -f deploy/docker/$cmd.Dockerfile \
    -t $REG/smol-agents/$cmd:$TAG --push .
done
```

For SLSA L3-style provenance, sign with `cosign`:

```bash
cosign sign --yes $REG/smol-agents/agent:$TAG
cosign sign --yes $REG/smol-agents/secret-proxy:$TAG
```

### 5.2 Configure values

Create a values override file. Minimum production set:

```yaml
# my-values.yaml
trustDomain: example.test                    # match your SPIRE deployment

agent:
  image:
    repository: your-registry.example.com/smol-agents/agent
    tag: 0.1.0
  config:
    mode: strict
    transport:
      private:
        authorize:
          - "prefix:spiffe://example.test/ns/agents"
      public:
        addr: "0.0.0.0:8444"
        certPath: /etc/tls/tls.crt
        keyPath:  /etc/tls/tls.key
    observability:
      otlpEndpoint: otel-collector.observability.svc:4317

secretProxy:
  image:
    repository: your-registry.example.com/smol-agents/secret-proxy
    tag: 0.1.0
  config:
    backend:
      kind: static
      static:
        - spiffeID: "spiffe://example.test/ns/agents/sa/agent-a"
          items:
            db-cred: "REDACTED"
    policy:
      - spiffeID: "spiffe://example.test/ns/agents/sa/agent-a"
        allow: ["db-cred"]

mode: knative                                # or deployment | statefulset

knative:
  scaleToZero: true
  minScale: 0
  maxScale: 50
```

### 5.3 Render and review before applying

```bash
helm template agents deploy/helm \
    --namespace smol-agents \
    --values my-values.yaml > /tmp/render.yaml

# Review:
less /tmp/render.yaml

# Sanity-check:
grep "runtimeClassName" /tmp/render.yaml      # MUST show kata-fc (or your preset's mapping)
grep "spiffeIDTemplate" /tmp/render.yaml      # confirm trust domain
```

### 5.4 Install

```bash
kubectl create namespace smol-agents
helm upgrade --install agents deploy/helm \
    --namespace smol-agents \
    --values my-values.yaml \
    --wait --timeout 5m
```

### 5.5 Post-install verification

Run the validation matrix in `tasks.md`:

| Requirement | Check                                                                         |
|-------------|-------------------------------------------------------------------------------|
| R-IDN-1     | `kubectl logs <agent-pod> -c agent \| grep "identity ready"`                  |
| R-MTL-1     | `openssl s_client -connect <pod-ip>:8443` — refuses without SVID              |
| R-SBX-1     | `kubectl get pod <agent-pod> -o yaml \| grep runtimeClassName` → `kata-fc` (or preset-mapped) |
| R-RUN-1     | `curl http://<pod-ip>:8080/readyz` → 200                                      |
| R-DEP-1     | `kubectl get ksvc -n smol-agents` shows Ready                              |
| R-EBP-1     | `kubectl get ds -n smol-agents -l app.kubernetes.io/component=ebpf-loader` Ready on all nodes |
| R-EBP-2     | `kubectl exec <loader-pod> -- ls /sys/fs/bpf/smol-agents` shows pinned maps |
| R-VRF-1     | `make verify-formal` (CI runs this)                                           |
| R-VRF-2     | `make test` includes the rapid property suite                                 |

---

## 6. Configuration reference

The agent's full config schema lives in
`.spec-workflow/specs/smol-agents-platform/design.md` under "Data Models".
Highlights:

```yaml
mode: strict                # insecure | permissive | strict
trustDomain: stigen.ai
identity:
  workloadAPI: unix:///run/spire/agent-sockets/api.sock
  bootTimeout: 30s          # block this long for first SVID
  maxJWTLifetime: 5m        # cap on issued JWT-SVID lifetime
  rotationThreshold: 0.5    # rotate at 50% remaining
transport:
  private:
    addr: "0.0.0.0:8443"
    authorize:              # at least one matcher; OR semantics
      - "any:spiffe://stigen.ai"
      - "prefix:spiffe://stigen.ai/ns/agents"
      - "spiffe://stigen.ai/ns/agents/sa/admin"
  public:
    addr: ""                # empty = disabled
    certPath: /etc/tls/tls.crt
    keyPath:  /etc/tls/tls.key
secrets:
  brokerSocket: /run/secret-broker/secret-broker.sock
  maxLeaseTTL: 15m
ebpf:
  programs: [syscalls, network]
  objectsDir: /usr/share/smol-agents/bpf
  ringBufferSize: 1048576
runtime:
  drainTimeout: 30s
  shutdownTimeout: 5s
  healthAddr: "0.0.0.0:8080"
sandbox:
  runtimeClass: kata-fc
observability:
  otlpEndpoint: otel-collector.observability:4317
  serviceName: smol-agent
```

Environment overrides:

| Variable                         | Effect                                |
|----------------------------------|---------------------------------------|
| `SMOL_AGENTS_MODE`            | Override `.mode`                      |
| `SMOL_AGENTS_TRUST_DOMAIN`    | Override `.trustDomain`               |
| `SMOL_AGENTS_WORKLOAD_API`    | Override `.identity.workloadAPI`      |
| `SMOL_AGENTS_BROKER_SOCKET`   | Override `.secrets.brokerSocket`      |
| `SMOL_AGENTS_OTLP_ENDPOINT`   | Override `.observability.otlpEndpoint`|
| `SMOL_AGENTS_ALLOW_INSECURE`  | Required (=`1`) for `mode: insecure`  |
| `SMOL_AGENTS_ENV`             | Sets `deployment.environment` resource|

---

## 6.5 Per-node eBPF loader (DaemonSet)

The `ebpf-loader` DaemonSet runs once per Linux node, attaches CO-RE BPF
programs, and pins their maps to bpffs so that **unprivileged agent Pods
can read events without holding `CAP_BPF` themselves**. This split is the
"two layers of containment" principle of `product.md`: privileged
operations live in one well-audited DaemonSet; the agent stays inside
its Kata + Firecracker microVM (or gVisor on the fallback presets).

### 6.5.1 Per-node prerequisites

The loader needs the following on every node it schedules onto:

| Requirement                                | Notes                                       |
|--------------------------------------------|---------------------------------------------|
| Linux kernel ≥ 5.8 (preferred ≥ 5.15)      | Older kernels work but require `privileged: true` |
| `/sys/kernel/btf/vmlinux` present          | Mandatory for CO-RE; if missing the loader will refuse to start |
| Either `bpffs` mounted at `/sys/fs/bpf`, OR `mountBPFFS: true` (default) | The init container mounts it |
| `kubernetes.io/os: linux` label            | The DaemonSet skips non-Linux nodes via nodeAffinity |
| Capabilities: `CAP_BPF` + `CAP_PERFMON` (modern), or `privileged: true` (universal) | `capabilities.mode` selects |

For managed Kubernetes services where you cannot install kernel modules
or control kubelet flags, the loader uses *only* in-kernel features
(BTF, ring buffer, raw_tracepoint, kprobe), so no host changes are
required beyond scheduling the DaemonSet.

### 6.5.2 Distro presets

`ebpfLoader.preset` selects a preconfigured set of host paths,
capabilities, and tolerations. Override anything beneath it as needed.

| Preset             | Target distros                                   | Capability mode | Notes                                    |
|--------------------|--------------------------------------------------|-----------------|------------------------------------------|
| `generic`          | Ubuntu, Debian, RHEL, AL2, plain CRI-O           | privileged      | Default; works almost everywhere         |
| `gke-cos`          | Google Container-Optimized OS                    | privileged      | Skips `/sys/kernel/debug` (locked down)  |
| `eks-bottlerocket` | Bottlerocket on EKS / EKS-A                      | minimal         | Kernel ≥ 5.10 always; CAP_BPF available  |
| `aks-mariner`      | Azure Mariner / CBL-Mariner                      | minimal         | Kernel ≥ 5.15                            |
| `k3s`              | Rancher k3s, k3os                                | privileged      | Forces bpffs mount via init              |
| `openshift`        | Red Hat OpenShift                                | privileged      | Requires SCC binding (see §6.5.3)        |
| `talos`            | Talos Linux                                      | minimal         | No `/lib/modules`, no `/sys/kernel/debug`|

Switch presets with:

```bash
helm upgrade --install agents deploy/helm \
    --namespace smol-agents \
    --set ebpfLoader.preset=eks-bottlerocket
```

### 6.5.3 OpenShift SCC binding

OpenShift requires a Security Context Constraint (SCC) to grant the
loader the privileges its DaemonSet needs. Apply this once per cluster
**before** installing the chart:

```bash
oc adm policy add-scc-to-user privileged -z agents-smol-agents-ebpf-loader \
    -n smol-agents
```

If you set `capabilities.mode: minimal`, replace `privileged` with a
custom SCC granting only `CAP_BPF`, `CAP_PERFMON`, and `CAP_NET_ADMIN`.

### 6.5.4 Disabling the loader

If you already run another eBPF tool (Cilium Tetragon, Falco, Pixie)
that would conflict, disable the loader entirely:

```yaml
# my-values.yaml
ebpfLoader:
  enabled: false

agent:
  config:
    ebpf:
      programs: []      # also stop the in-process agent loader
```

When the loader is disabled, agents can still embed their own eBPF
programs via the in-process `pkg/ebpf` Loader, *if* their Pods are
granted `CAP_BPF`. We do not recommend this for production: it
contradicts the principle of pushing privileged operations out of the
sandboxed agent.

### 6.5.5 Verifying the loader

```bash
# 1. DaemonSet is healthy on every Linux node:
kubectl get ds -n smol-agents -l app.kubernetes.io/component=ebpf-loader
# DESIRED == CURRENT == READY == NUMBER-AVAILABLE

# 2. Pinned objects exist on each node:
kubectl exec -n smol-agents <loader-pod> -- ls /sys/fs/bpf/smol-agents
# Expect: events  syscalls  network  (or as configured)

# 3. Programs are attached in-kernel:
kubectl exec -n smol-agents <loader-pod> -- cat /proc/kallsyms 2>/dev/null | grep -c bpf_prog || true
# Non-zero count means programs are present.

# 4. Loader logs:
kubectl logs -n smol-agents -l app.kubernetes.io/component=ebpf-loader --tail=50
# Expect: "loaded" with kernel features, programs, pinned maps
```

### 6.5.6 Rolling upgrades without dropping events

Because pinned maps survive Pod termination, a rolling DaemonSet upgrade
does not drop events: the new Pod re-adopts the existing pins. The
loader's `Detach`-on-shutdown explicitly does **not** call `coll.Close()`
(which would unpin) — see comments in
`pkg/ebpfloader/loader_linux.go::attachedObject.close`.

If you need to *fully* unload (e.g. to drain telemetry between major
versions) delete the pinned files manually after stopping the
DaemonSet:

```bash
kubectl scale ds/<release>-smol-agents-ebpf-loader -n smol-agents --replicas=0
# wait for Pods to terminate
kubectl debug node/<node> -it --image=busybox -- chroot /host \
    rm -rf /sys/fs/bpf/smol-agents
```

---

## 7. Troubleshooting

### `identity: boot timeout waiting for SVID`

The agent could not reach the SPIRE workload API in
`identity.bootTimeout`. Causes, in order of likelihood:

1. SPIRE CSI volume not mounted. `kubectl describe pod <agent-pod>` and
   look for the `spire-agent-socket` volume.
2. Workload selectors don't match. `kubectl get clusterspiffeid -A` and
   confirm `k8s:ns:<ns>` + `k8s:sa:<sa>` match the agent Pod.
3. SPIRE agent not registered on this node. `kubectl exec -n spire-server -it
   deploy/spire-server -- spire-server agent list`.

### `tls: bad certificate` on the private listener

The peer presented an SVID outside the configured `authorize` set. Inspect
the peer's SVID with `agentctl status` from the calling Pod, then add its
SPIFFE ID (or a prefix) to `transport.private.authorize`.

### `secrets: dial /run/secret-broker/...: no such file or directory`

The `secret-proxy` sidecar didn't bind the UDS. Check:

1. The sidecar Pod is running: `kubectl logs <pod> -c secret-proxy`.
2. The volume `secret-broker` is shared by both containers
   (the chart does this automatically).
3. The broker config has a valid `socketPath` and the parent directory is
   writable.

### Helm install fails with `sandbox.runtimeClass=runc requires sandbox.allowHostRuntime=true (R-SBX-1)`

This is the R-SBX-1 guard intentionally refusing to run an agent without a
sandbox. Either install Kata (or gVisor on `gke`/`generic-gvisor`
presets) on your nodes (production) or pass
`--set sandbox.allowHostRuntime=true` (development only).

### `unable to verify the first certificate` from external clients hitting `PublicListener`

The cert chain you supplied at `transport.public.certPath` is missing
intermediates. Concatenate the leaf, intermediates, and root in order:

```bash
cat leaf.pem intermediate.pem root.pem > tls.crt
```

### Knative Pod stuck in `ContainerCreating` with `RunContainerError`

Almost always a missing `RuntimeClass`. Confirm:

```bash
kubectl get runtimeclass kata-fc gvisor 2>/dev/null
kubectl describe pod <pod> | grep -A 5 Events
```

If your selected RuntimeClass is missing on the cluster, install the
matching runtime (kata-deploy, GKE Sandbox enablement, OpenShift
Sandboxed Containers Operator, etc.) per §1.4.1, or override locally
per §4.2.

### eBPF programs fail to load with `permission denied` or `verifier rejected`

The intended path is the **`ebpf-loader` DaemonSet** (§6.5): it runs
privileged on each node, pins maps, and lets unprivileged agents
subscribe through the pinned paths. If you see permission denials in
the *agent* Pod, you are running its in-process loader — disable it
with `agent.config.ebpf.programs: []` and let the DaemonSet do the work.

If you must keep eBPF inside the agent (not recommended), grant
capabilities:

```yaml
containerSecurityContext:
  allowPrivilegeEscalation: false
  readOnlyRootFilesystem: true
  capabilities:
    drop: ["ALL"]
    add: ["BPF", "PERFMON"]    # 5.8+ ; CAP_SYS_ADMIN on older kernels
```

Note: granting `CAP_BPF` partially relaxes the sandbox boundary
(Firecracker's seccomp filter for kata-fc, or gVisor's allow-list);
the trade-off is documented in `requirements.md` §R-SBX-2.

### `ebpf-loader` DaemonSet Pod is in `CrashLoopBackOff`

Most common causes, in order:

1. **No BTF on the node**. `kubectl exec <pod> -- ls /sys/kernel/btf/vmlinux` —
   if missing, the kernel was built without `CONFIG_DEBUG_INFO_BTF=y`.
   Newer EKS / GKE / AKS images have it; very old or hardened custom
   images may not. Fix: upgrade the host image.

2. **bpffs mount failed in init container**. `kubectl logs <pod> -c init-bpffs`.
   Most often caused by the host's `/sys/fs/bpf` already being mounted
   read-only or by a missing kernel module. On Talos, switch
   `ebpfLoader.preset=talos`.

3. **Capabilities insufficient on kernel < 5.8**. `kubectl logs <pod>` shows
   `failed to load: operation not permitted`. Switch
   `ebpfLoader.capabilities.mode=privileged` (works on every kernel).

4. **OpenShift SCC missing**. If `events` show "unable to validate
   against any security context constraint", run the SCC binding from
   §6.5.3.

5. **Hostpath unmountable on Bottlerocket**. Bottlerocket exposes the
   host fs at `/run/host`. The `eks-bottlerocket` preset already
   configures the right paths; verify you set `preset=eks-bottlerocket`.

### Pinned maps disappear after rolling DaemonSet upgrade

The loader is designed to leave pins on shutdown. If you observe
disappearance, check:

- That the new Pod scheduled on the same node before the old one's
  graceful period elapsed. `terminationGracePeriodSeconds` defaults to
  30s; longer values give the new Pod more time to come up first.
- That `pinRoot` is unchanged across versions (it must match exactly).
- That bpffs is mounted at the same host path. Some distros bind-mount
  `/sys/fs/bpf` to a per-version namespace path; pin to a stable
  subpath if so.

---

## 8. Verification commands cheat sheet

```bash
make tidy                         # go.mod consistent
make fmt                          # gofumpt -w .
make vet                          # go vet ./...
make lint                         # golangci-lint
make test                         # unit + property tests with -race
make test-integration             # integration tests (build tag)
make test-e2e                     # full kind harness
make verify-formal                # Quint model checks (Safety invariants)
make verify                       # vet + lint + test + verify-formal

helm lint deploy/helm
helm template agents deploy/helm > /tmp/render.yaml
kubectl apply --dry-run=server -f /tmp/render.yaml
```

---

## 9. Uninstall

```bash
helm uninstall agents -n smol-agents
kubectl delete namespace smol-agents

# Optionally remove the ClusterSPIFFEID if you are decommissioning the trust:
kubectl delete clusterspiffeid agents-smol-agents
```

The `RuntimeClass` resource is shared with other workloads — leave it.

---

## 10. Where to go next

- Read the spec: `.spec-workflow/specs/smol-agents-platform/design.md`
- Customize agent business logic: drop your code into a fork of
  `cmd/agent/main.go`; the wiring scaffolding is reusable.
- Run the formal model interactively: `quint repl spec/quint/identity.qnt`
- Trace through a property failure: `quint run --invariant=Safety
  --max-samples=5000 spec/quint/secrets.qnt --verbosity=3`

---

## 11. Migrating from Helm to Operator

The Helm chart and the operator coexist for one minor release so
tenants can migrate at their own pace. The end state has the operator
managing every `SmolAgent` CR with the chart removed.

### 11.1 Order of operations

1. **Install the operator alongside the chart**:

   ```bash
   kubectl apply -k operator/config/default
   ```

   This installs:
   - `smol-agents-system` namespace + ServiceAccount + RBAC
   - 8 CRDs (SmolAgent, SmolAgentPlatform, plus the 6 agent-model CRDs)
   - 2 webhook configurations + Service
   - The operator Deployment (2 replicas, leader-elect)

2. **Apply the singleton platform CR**:

   ```bash
   kubectl apply -f operator/config/samples/smolagentplatform.yaml
   ```

   This is the cluster-wide knob the operator uses for defaults +
   feature policy. Without it, every `SmolAgent` stays in
   `Pending → PlatformAbsent`.

3. **Translate each chart release into a `SmolAgent` CR**.
   Mapping table:

   | Helm `values.yaml` field          | `SmolAgent.spec` field            |
   |-----------------------------------|--------------------------------------|
   | `mode`                            | `mode`                               |
   | `trustDomain`                     | `trustDomain`                        |
   | `mode: knative\|deployment\|statefulset` | `deploymentKind`              |
   | `agent.replicas`                  | `replicas`                           |
   | `agent.image.repository:tag`      | `image`                              |
   | `agent.config.transport.private.*`| `features.transport.private.*`       |
   | `agent.config.transport.public.*` | `features.transport.public.*`        |
   | `agent.config.secrets.*`          | `features.secrets.*`                 |
   | `agent.config.ebpf.programs`      | `features.ebpf.programs`             |
   | `sandbox.runtimeClass`            | `features.sandbox.runtimeClass`      |
   | `sandbox.allowHostRuntime`        | `features.sandbox.allowHostEscape`   |
   | `agent.config.observability.*`    | `features.observability.*`           |

4. **Apply the new CR and wait for `Phase=Ready`**:

   ```bash
   kubectl apply -f my-tenant.yaml
   kubectl wait --for=jsonpath='{.status.phase}'=Ready smolagent/<name> -n <ns> --timeout=120s
   ```

5. **`helm uninstall` the chart-managed release** for that tenant.
   Because the operator owns its resources via OwnerReferences and
   the chart owned its own copies (under different release labels),
   there's no fight — but you do briefly run two ConfigMaps / SAs
   for the same agent. The operator's resources will replace the
   chart's at this step.

6. **Repeat for every tenant**, then delete the chart entirely.

### 11.2 Coexistence guarantees

- The operator only manages resources whose `OwnerReferences`
  include its `SmolAgent` CR. It never touches resources owned by
  the chart's release.
- If you accidentally apply both, the operator and chart will
  generate two copies of every owned object with different
  `app.kubernetes.io/managed-by` labels. Pick one to keep and
  delete the other.

### 11.3 Sample migration walk-through

Before (chart):

```bash
helm install agents-prod deploy/helm \
    --namespace tenant-prod \
    --set trustDomain=stigen.ai \
    --set mode=knative \
    --set sandbox.preset=eks-bottlerocket
```

After (operator):

```yaml
# tenant-prod-agent.yaml
apiVersion: agents.stigen.ai/v1
kind: SmolAgent
metadata:
  name: agents-prod
  namespace: tenant-prod
spec:
  trustDomain: stigen.ai
  mode: strict
  deploymentKind: knative
  features:
    sandbox:
      runtimeClass: kata-fc            # eks-bottlerocket preset's mapping
```

```bash
kubectl apply -f tenant-prod-agent.yaml
kubectl wait --for=jsonpath='{.status.phase}'=Ready smolagent/agents-prod -n tenant-prod
helm uninstall agents-prod -n tenant-prod
```

### 11.4 Rollback

If the operator path mis-renders something, the chart still works:

```bash
kubectl delete smolagent agents-prod -n tenant-prod
helm install agents-prod deploy/helm --namespace tenant-prod ...
```

Rollback is fast because the operator's owned objects are deleted by
the K8s GC immediately after the CR goes away.

### 11.5 Deprecation timeline

- **Today (v0.1)** — chart and operator coexist. New tenants should
  use the operator; existing tenants migrate at convenience.
- **v0.2** — chart marked deprecated; CI no longer publishes new
  chart versions, only operator images.
- **v0.3** — chart removed from the repository.
