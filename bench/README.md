# agentbench — benchmarking + verification platform

Deploys full agents across the smol-agents stack and **proves** tools / plugins /
filesystems / secrets work with **real LLM backends** (z.ai glm-4.6 via Hermes;
z.ai Anthropic endpoint for claude-code), measuring latency / tokens / cost /
throughput / isolation. Design: [`docs/design/benchmarking-platform.md`](../docs/design/benchmarking-platform.md).

Runner: `operator/cmd/agentbench` (Go). Run via `make bench-lint` / `make bench-l1` / `make bench-l2`.

## Tiers
- **correctness** (cftest+metal) — boolean oracles that prove a feature worked with a real LLM.
- **perf / scale** (cftest) — latency p50/p95, tokens (Hermes-real), throughput, durable-session recovery, scale-to-zero.
- **isolation** (AWS-Graviton-metal only) — real kata microVM; auto-SKIPS on cftest (runc).
- **future** (`--allow-blocked`) — negative-oracle tripwires for stubbed features; each FAILS the day its spec lands.

## Honesty constraints (load-bearing)
- Tokens are real **only for Hermes**; CLI kinds report 0 by contract — no token/cost gate on CLI cases.
- **No oracle gates on `usage.toolCalls`** (structurally 0 on the harness path). Tool execution is proven by output side-effects / the echo-server access log.
- Loop-mode tools are unwired today → the loop tool case ships as a **negative** `tool_rejected` oracle.
- Seed is best-effort (glm-4.6 may ignore it); correctness uses semantic oracles + N-sample distributions, not bit-exact equality.

## Schema (`agentbench/v1`)
- **BenchPlan** (`plan.yaml`): `fleet:{secretSourceNamespace, copySecrets[], manifests[], awaitReady[]}`, `caseFiles:[glob]`, `cases:[BenchCase]`.
- **BenchCase**: `tier`, `agentRef`, `driver:(run|gateway)`, `samples`, `seed`, `requiredCaps[]`, `input:{prompt, nonce}`, `oracle:{kind, …}`, `gates:[{metric, op:(lte|gte|eq), want}]`, `blocked:{reason, unblockSpec}` (future tier).
- Oracle kinds: `output_match`, `output_jsonpath`, `tool_observed`, `tool_rejected`, `fs_roundtrip`, `secret_reach`, `secret_absent`, `isolation_kernel`, `egress_metadata_blocked`, `budget_terminated`.

## Prereqs (never commit keys)
The run namespace needs the provider secrets (`hermes-gw-auth`, `zai-anthropic`, `minio-creds`, `zai-openai`); the runner copies them from `--secret-source-ns` (default `hermes-e2e`). `sec-echo-token` is generated fresh per run.
