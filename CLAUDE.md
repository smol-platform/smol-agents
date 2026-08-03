# smol-agents — project guide

A Kubernetes-native agent platform: declare an **Agent** (LLM + instructions + budget +
tools, referencing a **ModelProvider**), trigger work with an **AgentRun** (one execution)
or **AgentSession** (durable multi-turn worker). The operator renders sandboxed pods
(kata-fc by default), runs the plan-act-observe loop or a CLI/HTTP **harness**, and folds
results into status. Secrets are broker-leased over a UDS — the agent process never reads a
provider key. See `README.md` (architecture) and `docs/examples/` (worked scenarios).

Single Go module: `github.com/smol-platform/smol-agents` (root `pkg/` + `cmd/` + `operator/`).

## Build / test

```bash
make build           # binaries: agent secret-proxy agentctl ebpf-loader
make test            # go test -race ./...
make envtest         # controller tests (KUBEBUILDER_ASSETS)
make kind-verify     # build+load operator into kind, apply sample chain, assert Ready
make e2e-l0 / e2e-l1 # docker-compose / kind fullstack rings
make e2e-l2          # AWS Spot c7gd.metal kata ring (needs AWS_PROFILE, us-east-2)
```

## Hard rules (these have bitten us)

- **CRDs under `operator/config/crd` are hand-edited, NOT reproducible from Go.** Never blind
  `make manifests`. Known drift: `ModelProvider.spec.secretRef` is missing the `key` field
  that Go's `AuthRef` has — so a provider secret must hold a **single key** (it reads the sole
  key) until the CRD is fixed.
- Never `--no-verify`; never disable tests to make them pass; commit/push only when asked.
- All published images must be **multiarch** (amd64+arm64). ghcr `0.2.1` images are multiarch.
- Cost is integer **milli-USD, observability-only**; never gate on `usage.toolCalls`; roll up
  usage field-wise (never `Usage.Add`); never log/persist secrets.

## Running the examples for real, locally (kind + z.ai) — verified 2026-06-06

`docs/examples/` + `.claude/live-zai/` (scratch) run on real **z.ai glm-4.6**. The 5 user
scenarios (multi-run, session-per-request, idle pause/resume, file-state recovery,
config/settings) were verified live on `kind-smol-agents-kind`. Recipe:

1. **Operator on a kataless cluster:** default run class is `kata-fc`, so runs hang
   `Pending/NoKVMCapacity` unless you set `--default-run-runtime-class=runc --allow-host-runtime`.
   Pin spawned images with env `SMOL_AGENTS_IMAGE_REGISTRY=ghcr.io/smol-platform/smol-agents`
   + `SMOL_AGENTS_IMAGE_TAG=0.2.1`. (kindnet does NOT enforce NetworkPolicy — the egress floor
   is a no-op locally; it's real on policy-enforcing CNIs / eBPF, proven at L2.)
2. **z.ai key** (1Password `stigenai` → vault "Personal Agents" → `zai-agent-token`/`ZAI_API_KEY`):
   `kubectl -n <ns> create secret generic zai-key --from-literal=ZAI_API_KEY=<key>` (single key), then
   `kubectl -n <ns> label secret zai-key agents.smol-agents.ai/tenant-secret=true` — the operator refuses
   to read a CR-referenced Secret without this label (tenant boundary, 5vr).
3. **z.ai is a Coding Plan, not pay-as-you-go.** The pay-as-you-go OpenAI endpoint
   `https://api.z.ai/api/paas/v4/chat/completions` returns **429 code 1113 "insufficient
   balance"**. Use the Coding-Plan endpoints: `https://api.z.ai/api/coding/paas/v4/...`
   (OpenAI-compat, for the loop) and `https://api.z.ai/api/anthropic` (for claude-code).
4. **Loop needs a path bridge.** `openaillm` hardcodes `<endpoint>/v1/chat/completions`; z.ai's
   path is `/api/coding/paas/v4/chat/completions`. Run an in-cluster nginx proxy that rewrites
   `/v1/chat/completions` → the coding path and point `ModelProvider.endpoint` at the proxy.
5. **Sessions** need NATS + `agentgateway` and the operator started with
   `--session-nats-url`; submit turns via `POST /v1/sessions/{ns}/{name}/turns`. **AgentFS**
   needs minio (or S3) + `agentfs-kopia-creds` (`access-key-id`/`secret-access-key`/`kopia-password`).

## Runtime gotchas discovered live

- **Sessions are loop-only.** `serve-session` builds a loop LLM (`buildLoopLLM`); a CLI harness
  (claude-code) CANNOT drive an AgentSession — sessions need an OpenAI-compatible ModelProvider
  or Hermes (HTTP). claude-code is run-mode only.
- **claude-code can't write files headlessly on runc.** `approvalMode: acceptEdits` and
  `cli.allowedTools:[Write,…]` are NOT enough; only `--dangerously-skip-permissions` lets it
  write, and **D3 refuses that flag unless the runtime is a kata microVM**. So claude-code
  file-writing agents are effectively **kata-only**. (Verify AgentFS file-state on runc via
  cross-pod kopia recovery instead.)
- **AgentFS ephemeral (no S3 backup) is broken on `0.2.1`** (fix is HEAD-only) → session agents
  on `0.2.1` must set `storage.agentfs.backup.s3`. `storage.agentfs.sizeGiB` is required (`>0`)
  or the Agent goes `Failed/InvalidSpec`, which silently blocks its session worker (no SA → no pod).
- **Harness token accounting** needs `cli.outputFormat: json` (else `usage.tokens=0`).
- The per-agent ServiceAccount `<agent>-agent` is auto-created by the Agent reconciler; a
  worker won't schedule until the Agent is `Ready`.
