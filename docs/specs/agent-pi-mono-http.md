# Spec: Full support for pi-mono (Mario Zechner's `pi`) over HTTP

> **✅ Decisions resolved 2026-06-03 — see [decisions.md](../design/decisions.md).** Decided: rename the `pi` kind → `inflection-pi` (+ deprecation alias); `pi-mono` is the CLI. D2/D4/D5: the resident interactive variant gets `spec.session` + driver-mode attach. Where this doc still says OPEN/PROPOSED and conflicts, the decision log wins.

> **Status: DESIGN / PROPOSAL — 2026-06-03.** Nothing in this spec is built yet. It is an implementation-grade plan to add **first-class support for the real `pi` coding agent** (`@earendil-works/pi-coding-agent`, formerly `@mariozechner/pi-coding-agent`) to the smol-agents runtime, **driven over HTTP** as the user explicitly requested. The existing `HarnessKind=pi` is a **false friend** (Inflection AI's hosted Pi) — this spec resolves that naming collision.
>
> Builds on (read first, do not duplicate): [harness-authoring.md](../design/harness-authoring.md) — the authoritative "how to add a HarnessKind" + Response-richness contract. This spec is a concrete *instance* of that process plus the HTTP-wrapping, bundle-image, credential, deadline, and terminal pieces specific to pi.
>
> Companion specs (this run): [agent-claude-code.md](agent-claude-code.md), [agent-codex.md](agent-codex.md) (sibling CLI-bundle specs, same shape), [terminal-exposure-http-ssh-tmux.md](terminal-exposure-http-ssh-tmux.md) (interactive access), [loop-mode-tools-and-invokers.md](loop-mode-tools-and-invokers.md), [agentnetwork-datapath-enforcement.md](agentnetwork-datapath-enforcement.md), [response-richness.md](response-richness.md), [run-governance.md](run-governance.md), [dynamic-credential-backends.md](dynamic-credential-backends.md). Scorecard context: [agent-runtime-fit-analysis-v0.2.0.md](../research/agent-runtime-fit-analysis-v0.2.0.md).

---

## 1. Summary

**Full pi support over HTTP** means: an operator can declare `harness.kind=pi-mono` on an `Agent`, the runtime starts a hardened run pod from a **bundle image** carrying the real `pi` CLI (Node 22 + git + `pi` + the `/agent` driver **and** a tiny HTTP shim), and each `AgentRun` is executed by **POSTing the prompt to a localhost HTTP endpoint inside the pod** which drives `pi` in non-interactive mode and returns the final answer (plus, where pi exposes it, token usage and a tool-call trace). The provider API key is **never placed in the harness process environment** — it is leased from the broker and injected only into the HTTP shim, so pi's built-in `bash` tool cannot `printenv` it. The run is bounded by an `activeDeadlineSeconds` (pi has *no* max-step limit) layered on top of the existing wall-clock budget, and the same long-lived pod can optionally expose an **interactive terminal** (tmux + ttyd / SSH) via [terminal-exposure-http-ssh-tmux.md](terminal-exposure-http-ssh-tmux.md). The outcome: pi joins claude-code/codex/aider/goose as a fully-supported, microVM-isolated, egress-caged coding agent — and the misleading `pi` kind is renamed.

The single load-bearing external fact: **pi has no native HTTP/server/daemon mode** (it has interactive, `--print`, `--mode json`, and `--mode rpc`; the last is *stdin/stdout JSONL*, not a socket). "pi over HTTP" therefore requires **wrapping pi in a small HTTP server (a `pi-bridge`)** that the harness POSTs to. This is the core design decision in §4.

---

## 2. Current state

### What exists

| Thing | Where | Reality |
|---|---|---|
| `HarnessKind="pi"` constant | `pkg/agentmodel/v1/harness.go:43-47` | **False friend.** Comment already says so. |
| `PiHarness` | `pkg/agentruntime/harness/http.go:20-46` | POSTs to `https://api.inflection.ai/external/api/inference` when `spec.http.url` empty; prompt field `context`, response field `text`. This is **Inflection's hosted Pi assistant**, not the `pi` coding CLI. |
| Registration | `pkg/agentruntime/harness/iface.go:97` | `r.Register(&PiHarness{})` |
| Validation | `pkg/agentmodel/v1/harness.go:316-320` | `kind=pi` requires `http.url` (load-bearing: stops the silent Inflection default — see [harness-authoring.md](../design/harness-authoring.md) §7). |
| CLI bundle pattern | `deploy/docker/harness-{claude-code,codex,aider,goose}.Dockerfile` | Two-stage: build `/agent` from `golang:1.26`, install the CLI on `node:22-slim`, `ENV HOME=/tmp`, `USER 65532`, `ENTRYPOINT ["/agent"]`. The template pi will follow. |
| Per-kind image map | `operator/internal/builders/harness_image.go:20-25` | Maps `claude-code/codex/aider/goose` → bundle image; HTTP kinds + `generic-cli` fall back to base `/agent`. **No `pi` entry.** |
| Run pod sandbox | `operator/internal/builders/agentrun.go:56-71`, `run_sandbox.go:43-51` | RestartPolicy=Never, non-root uid `RunPodUID`, drop-ALL, seccomp RuntimeDefault; RuntimeClass pinned to resolved sandbox (default `kata-fc`, fail-closed) by `ApplyRunSandbox`. |
| Run egress cage | `operator/internal/builders/run_sandbox.go:60+` (`BuildAgentRunEgressPolicy`) | Static default-deny NetworkPolicy: DNS + in-cluster RFC1918 + public 80/443; `169.254.0.0/16` (metadata) blocked. **Ignores AgentNetwork allow-lists** (see [agentnetwork-datapath-enforcement.md](agentnetwork-datapath-enforcement.md)). |
| Broker sidecar | `operator/internal/builders/secret_broker.go` (`AttachSecretBroker`), wired in `operator/internal/controllers/agentmodel/agentrun_controller.go` | UDS broker (`/run/secret-broker/secret-broker.sock`); resolves harness `env[].secretRef` + loop `ModelProvider` and serves leases. |
| RunSpec marshalling | `operator/internal/builders/runspec.go:46-80` | Marshals `agent.json` + `run.json` (+ `provider.json` for loop). Harness env literals are *also* stamped as pod env in `agentrun.go:90-96`. |

### What is missing / stubbed (the gap this spec fills)

- **No real pi integration.** The only path today is `generic-cli` with a hand-rolled custom image + `spec.command` — which loses the curated argv and the per-kind bundle, and provides **no HTTP drive**.
- **No HTTP wrapper for any CLI.** Every CLI kind is a subprocess (`runCLI`, `cli.go:27-79`). There is no "drive a local coding CLI over HTTP" plumbing. The user explicitly asked for pi **with http**.
- **No `activeDeadlineSeconds` anywhere.** `rg ActiveDeadlineSeconds operator/**.go` → **zero hits.** The only run bound is the harness `ctx` wall-clock budget (`iface.go:129-134`) and the RunResult termination. pi has *no max-step limit*, so an unbounded loop (e.g. pi thrashing on a failing test) only stops when the budget ctx fires — and if pi ignores SIGTERM, the pod can hang. A pod-level hard deadline is needed.
- **Credential leak surface.** For CLI kinds, `mergeEnv` (`cli.go:108-119`) puts the provider key (`ANTHROPIC_API_KEY` etc.) **directly in the harness process env**, and pi's `bash` tool can read it (`printenv`, `env`). The broker exists but its leases land in `Request.Env` → process env, so the agent-blind property is **lost for tool-bearing CLIs** (this is a general CLI-harness gap; pi makes it acute because `bash` is a first-class pi tool).
- **No interactive terminal.** Run pods are batch (`RestartPolicy=Never`, exits when the run ends). pi's whole UX is interactive; there is no way to attach. Cross-cuts [terminal-exposure-http-ssh-tmux.md](terminal-exposure-http-ssh-tmux.md).
- **Naming collision unresolved.** `pi` means Inflection in the codebase; users will reasonably expect Mario Zechner's pi.

---

## 3. External interface research (pi / pi-mono — confirmed 2026-06-03)

> Sources: the pi-mono monorepo moved org from `badlogic/pi-mono` to **`earendil-works/pi`**; the npm scope likewise moved from `@mariozechner/pi-coding-agent` to **`@earendil-works/pi-coding-agent`** (`@mariozechner/*` still resolves/redirects). Verified against the repo README and Mario Zechner's write-up. **Re-pin the exact package + version at implementation time — this tool moves weekly.**
>
> - Repo: <https://github.com/earendil-works/pi> (was <https://github.com/badlogic/pi-mono>)
> - CLI README: <https://github.com/earendil-works/pi/blob/main/packages/coding-agent/README.md>
> - npm: <https://www.npmjs.com/package/@earendil-works/pi-coding-agent> (and `@mariozechner/pi-coding-agent`)
> - Author write-up: <https://mariozechner.at/posts/2025-11-30-pi-coding-agent/>

| Question | Answer (verbatim where possible) |
|---|---|
| Binary / package | Binary `pi`; package `@earendil-works/pi-coding-agent` (alias `@mariozechner/pi-coding-agent`). Install `npm install -g @earendil-works/pi-coding-agent`; for hermetic/CI installs use `npm install --ignore-scripts` (pi needs no lifecycle scripts). |
| Modes | **Four:** interactive (default TUI); **`-p` / `--print`** ("Print response and exit"; accepts piped stdin); **`--mode json`** ("Output all events as JSON lines"); **`--mode rpc`** ("RPC mode for process integration", **LF-delimited JSONL over stdin/stdout**). |
| **HTTP / server / daemon mode** | **NONE.** No socket/HTTP daemon is documented. `--mode rpc` is the closest, but it is stdin/stdout JSONL, **not** an HTTP endpoint. ⇒ HTTP must be added by a wrapper (§4). |
| RPC framing caveat | "RPC mode uses strict LF-delimited JSONL framing. Clients must split records on `\n` only. Do not use generic line readers like Node `readline`, which also split on Unicode separators inside JSON payloads." (Relevant if the bridge speaks RPC rather than spawning `--print` per request.) |
| Model / provider config | `--provider <name>`, `--model <pattern>` (or combined `--model provider/id`, e.g. `--model openai/gpt-4o`; thinking shorthand `--model sonnet:high`). Custom providers/models in `~/.pi/agent/models.json`; project override `.pi/settings.json`. |
| BYO-key env | Provider keys via standard env: `ANTHROPIC_API_KEY` (shown in Quick Start `export ANTHROPIC_API_KEY=sk-ant-...`), `OPENAI_API_KEY`, etc.; per-provider keys in `docs/providers.md`. |
| System prompt | `--system-prompt <text>` (replace), `--append-system-prompt <text>` (append); files `.pi/SYSTEM.md` (project), `~/.pi/agent/SYSTEM.md` (global). |
| Built-in tools | `read`, `write`, `edit`, **`bash`** (first-class). In the TUI, `!cmd` runs bash + sends output, `!!cmd` runs without sending. **No `bash` is the live key-exfil concern.** |
| Permissions / sandbox stance | **No auto-approve/yolo flag is needed for `--print`/`--mode json`** — non-interactive modes don't prompt; pi just executes its tools (incl. `bash`). The author's explicit guidance: **"Run in a container, or build your own confirmation flow with extensions."** This is exactly our model — the kata-fc sandbox + egress cage *is* the container. |
| Max-step / loop bound | **No max-step / max-iteration flag is documented** — pi loops tool→model until the task is done or the model stops. ⇒ we MUST bound it externally (`activeDeadlineSeconds` + budget ctx; §5.4). |
| Sessions | `--no-session` (ephemeral, don't save), `--session <path\|id>`, `--fork <path\|id>`. Maps cleanly onto our `SessionPolicy` (§5.5). |
| Node version | **Not stated** in the README. pi is modern ESM; pin **Node 22 LTS** (matches the existing harness bundles' `node:22-slim`). Re-verify `engines` at build time. |

**Decision forced by research:** because there is *no* HTTP mode, "pi over HTTP" = **a `pi-bridge` HTTP server we ship in the bundle image** that, per request, spawns `pi --print`/`--mode json` (or holds one `--mode rpc` process). The harness becomes an **HTTP kind** that POSTs to `http://127.0.0.1:<port>/run`. See §4.

---

## 4. Design

### 4.1 The core choice: CLI-subprocess vs HTTP-bridge

[harness-authoring.md](../design/harness-authoring.md) §3 frames the decision as CLI vs HTTP and notes a CLI kind **structurally cannot** report tokens or tool calls (only HTTP kinds parsing structured JSON can). pi *does* emit structured JSON (`--mode json` → event-per-line incl. usage + tool calls). The user asked for **http** explicitly. Both pressures point the same way:

> **DECISION: ship pi as an HTTP kind (`pi-mono`) backed by an in-pod `pi-bridge` HTTP server that wraps the pi CLI.** The bridge runs `pi --mode json --no-session -p <prompt>` per request, parses the JSON event stream, and returns `{output, tokensIn, tokensOut, toolCalls}` as JSON. The harness is a thin `doHTTP`-style client posting to `127.0.0.1`.

Why a bridge and not just `generic-cli`:
- **HTTP, as requested.** The wire between harness and pi is HTTP; the bridge is the only way to get that given pi has no server mode.
- **Richer Response.** Parsing `--mode json` lets us populate `TokensIn/TokensOut/ToolCalls` (the CLI driver can't — `cli.go` never parses). This directly closes the harness-row gap in the scorecard for pi specifically.
- **Credential isolation (§7).** The bridge holds the provider key in *its* env; pi is spawned by the bridge **with the key stripped from pi's child env** but configured via pi's config file or a per-request injected header→config, so pi's `bash` can't `printenv` it. This is impossible with a bare subprocess where the key sits in the harness env pi inherits.
- **Terminal reuse.** The bridge process can keep the pod alive and host an interactive pi session for [terminal-exposure-http-ssh-tmux.md](terminal-exposure-http-ssh-tmux.md).

Cost: the bridge is new code (~150 LoC Go, in `cmd/pi-bridge`, shipped in the bundle). Acceptable; it is the smallest thing that satisfies "with http" + richer Response + key isolation.

> **Alternative considered (rejected as primary):** add `kind=pi-mono` as a *CLI* kind that just spawns `pi -p`. Simpler (≈6 lines like `GooseHarness`), but loses HTTP, loses tokens/tool-calls, and leaves the key in pi's env. We keep this as a **fallback/escape hatch** only (a `cli` sub-mode), not the default.

### 4.2 Topology (run pod)

```
AgentRun pod (RuntimeClass=kata-fc, RestartPolicy=Never, default-deny egress)
┌──────────────────────────────────────────────────────────────────┐
│ container "harness"  (image: harness-pi-mono bundle)               │
│   /agent run --dir=/etc/smol-agents/run                            │
│     └─ PiMonoHarness.Run(): POST http://127.0.0.1:8848/run         │
│            { prompt, system, model, seed }                         │
│                                                                    │
│   pi-bridge  (started by /agent as a child OR shipped as its own   │
│              container — see §4.3)                                  │
│     • holds provider key in ITS env (from broker)                  │
│     • on /run: spawn `pi --mode json --no-session -p <prompt>`     │
│       with a CHILD env that OMITS the provider key; key reaches pi │
│       via ~/.pi/agent/models.json written at boot (0600)           │
│     • parse JSON event lines → {output, usage, toolCalls}          │
│                                                                    │
│   pi child process: read/write/edit/bash over the workspace       │
│     CWD = EffectiveWorkingDir (AgentFS mount when durable)         │
└──────────────────────────────────────────────────────────────────┘
   sidecar "secret-proxy" (broker UDS) ── leases key to pi-bridge ONLY
   (optional) sidecar "ttyd"/"sshd" for interactive terminal (separate spec)
```

### 4.3 Where the bridge runs: same container vs sidecar

Two viable placements:

1. **Same container, `/agent` spawns `pi-bridge` as a child** (preferred for v1). `/agent run` for `kind=pi-mono` starts the bridge on `127.0.0.1:8848`, waits for readiness, POSTs, then tears it down. Simplest networking (loopback, no Service), one image, one lifecycle. The key-isolation trick (bridge env vs pi child env) is purely a `cmd/pi-bridge` concern.
2. **Dedicated sidecar container** (`pi-bridge`) in the same pod. Cleaner separation (the harness container need not carry Node), but the provider key must be injected into the *sidecar's* env not the harness's, and loopback still works (shared netns). Heavier. **Defer to a follow-up** unless the terminal spec wants a long-lived bridge anyway.

> **DECISION: v1 uses placement (1)** — `/agent` (already the entrypoint) launches `pi-bridge` in-process/in-container. The bundle image carries Node + pi + `/agent` + `pi-bridge` (a second Go binary, or a subcommand `/agent pi-bridge`).

---

## 5. Concrete changes

### 5.1 Resolve the naming collision (do this first)

The maintainer must pick one (see §10 Open decisions). Recommended:

- **Add `HarnessKind="pi-mono"`** (the real pi) and **rename the existing constant** in code from `HarnessPi`/`"pi"` to `HarnessInflectionPi`/`"inflection-pi"`, keeping `"pi"` accepted at admission as a **deprecated alias** that maps to `inflection-pi` with a warning event. This avoids silently changing behavior of any existing `kind: pi` Agent (which today means Inflection).
  - `pkg/agentmodel/v1/harness.go:43-47`: add `HarnessPiMono HarnessKind = "pi-mono"`; rename `HarnessPi` → `HarnessInflectionPi` (`"inflection-pi"`); add a `deprecatedKindAliases = map[string]HarnessKind{"pi": HarnessInflectionPi}` consulted in admission.
  - `HarnessKind.Valid()` (`harness.go:64-71`): add `HarnessPiMono`, `HarnessInflectionPi`, and accept `"pi"` via the alias.
  - Update the false-friend comment to point here.

> **Alternative (lower-churn):** keep `kind: pi` = Inflection forever, add only `kind: pi-mono`. Less correct naming but zero migration. Maintainer's call (§10).

### 5.2 New HarnessKind + harness implementation

- **`pkg/agentmodel/v1/harness.go`**
  - Add `HarnessPiMono` const + `Valid()` arm (above).
  - **Classify `pi-mono` as an HTTP kind** in `ValidateHarness` (`harness.go:316-320`): add `HarnessPiMono` to the `case HarnessGenericHTTP, HarnessPi, HarnessHermes:` arm **BUT** make `http.url` **optional/defaulted** for pi-mono (the bridge URL defaults to `http://127.0.0.1:8848/run`). So: a dedicated `case HarnessPiMono:` that requires nothing (URL defaulted) but validates `PiMono` sub-block if present.
  - Add a typed sub-spec (sibling to `HarnessHTTPSpec`/`HarnessCLISpec`):

    ```go
    // HarnessPiMonoSpec configures the real pi coding agent (kind=pi-mono),
    // driven over HTTP via the in-pod pi-bridge.
    type HarnessPiMonoSpec struct {
        // Model is pi's --model (e.g. "anthropic/claude-sonnet-4", "openai/gpt-4o",
        // "sonnet:high"). Required: pi otherwise picks a default that may not match
        // the leased provider key.
        Model string `json:"model"`

        // Provider is pi's --provider when Model is bare. +optional
        Provider string `json:"provider,omitempty"`

        // Mode selects how the bridge drives pi:
        //   "print" (default) → `pi --mode json -p` per request (richest Response)
        //   "rpc"             → one persistent `pi --mode rpc` process (faster, stateful)
        // +optional
        Mode string `json:"mode,omitempty"`

        // BridgePort overrides the loopback port. Default 8848. +optional
        BridgePort int32 `json:"bridgePort,omitempty"`

        // ExtraArgs are appended to pi's argv verbatim (e.g. ["--append-system-prompt","..."]).
        // +optional
        ExtraArgs []string `json:"extraArgs,omitempty"`

        // ActiveDeadlineSeconds caps the whole run at the POD level (pi has no
        // max-step bound). Maps to PodSpec.ActiveDeadlineSeconds. Default derived
        // from Budget.MaxWallClockSeconds + a grace margin; see §5.4. +optional
        ActiveDeadlineSeconds int64 `json:"activeDeadlineSeconds,omitempty"`
    }
    ```

    Add `PiMono *HarnessPiMonoSpec` to `HarnessSpec` (after `CLI`, `harness.go:126`).

- **`pkg/agentruntime/harness/pi.go`** (new) — `PiMonoHarness`:
  - `Kind()` returns `HarnessPiMono`.
  - `Run()` builds the bridge request body `{prompt, system: req.Instructions, model, seed}` and POSTs to the bridge URL (default `http://127.0.0.1:8848/run`). **Reuse `doWithRetry`** (`retry.go`) for transient localhost failures (bridge still starting). Parse the bridge JSON response into `Response{Output, TokensIn, TokensOut, ToolCalls, DurationMs}` — this is the **first CLI-family harness to honestly populate tokens + tool calls** because the bridge does the JSON parsing (contract-compliant per [harness-authoring.md](../design/harness-authoring.md) §4).
  - `ctx` cancellation aborts the POST; the bridge translates the dropped connection (or a `DELETE /run/{id}`) into `SIGTERM`→`SIGKILL` on the pi child.
  - Inject a fake transport via the existing `HTTPClient` seam (`http.go:15-18`) for unit tests.

- **`pkg/agentruntime/harness/iface.go:90-101`** — register `r.Register(&PiMonoHarness{})` in `Default()`.

### 5.3 The pi-bridge (new binary)

- **`cmd/pi-bridge/main.go`** (new) — a ~150 LoC HTTP server:
  - `POST /run` `{prompt, system, model, seed}` → spawn `pi --mode json --no-session -p <prompt>` (plus `--system-prompt`/`--append-system-prompt`, `--model`, `ExtraArgs`); stream-parse the JSONL events; accumulate the final assistant text + the last `usage` event + tool-call events; reply `{output, tokensIn, tokensOut, toolCalls, durationMs}`. **Split on `\n` only** (honor pi's RPC/JSONL caveat — do NOT use a Unicode-aware line reader).
  - `GET /healthz` for readiness.
  - **Key isolation:** the bridge reads the provider key from **its own env** (broker-injected, e.g. `PI_PROVIDER_KEY` / `ANTHROPIC_API_KEY`), writes `~/.pi/agent/models.json` (mode 0600) **once at boot**, then spawns pi with `exec.Cmd.Env` that **omits** the provider-key vars (a deny-list scrub of `*_API_KEY`/`PI_PROVIDER_KEY` from `os.Environ()`). pi reads its key from the config file it can't trivially `printenv`. (Defense-in-depth, not perfect — see §7.)
  - Bound output (reuse the `capWriter` idea / default 1 MiB) and enforce the bridge's own ctx deadline as a backstop.
  - Optionally support `Mode=rpc`: hold one `pi --mode rpc` child, frame requests/responses as JSONL. Defer to phase 2.

- **`/agent run` wiring (`cmd/agent`):** when `agent.Spec.Harness.Kind == pi-mono`, before calling the harness, **start `pi-bridge`** (spawn `/agent pi-bridge` or the `pi-bridge` binary) bound to loopback, wait for `/healthz`, then run `PiMonoHarness`; on exit, SIGTERM the bridge. (Mirror how the executor already owns the harness lifecycle.)

### 5.4 activeDeadlineSeconds (new — closes a real gap)

`ActiveDeadlineSeconds` appears **nowhere** in the operator today. Add it generically (benefits all harnesses, acute for pi):

- **`operator/internal/builders/agentrun.go`** `BuildAgentRunPod` (`agentrun.go:56-71`): set `pod.Spec.ActiveDeadlineSeconds` from a resolver:
  - explicit `Harness.PiMono.ActiveDeadlineSeconds` (or a new generic `AgentRunSpec`/`Budget` field — see [run-governance.md](run-governance.md)) wins;
  - else derive from `Budget.MaxWallClockSeconds + grace` (e.g. `+120s` so the budget ctx fires first and produces a RunResult, with the pod deadline as the hard backstop if pi ignores SIGTERM);
  - else a platform default (e.g. operator flag `--default-run-active-deadline=3600`).
  - **Failure mode:** when the pod deadline fires, kubelet kills the pod with `reason=DeadlineExceeded` and **no termination message** — the agentrun controller must map a `DeadlineExceeded` pod failure to a terminal AgentRun with a clear `TerminationReason` (the controller already owns terminal state — recent commit `9f29f2e`). Add that mapping.
- This is the right home because the budget ctx (`iface.go:129-134`) only kills the *harness subprocess via ctx*; if pi (or its `bash` grandchildren) ignore SIGTERM, only the pod-level deadline guarantees teardown.

### 5.5 SessionPolicy mapping

Wire pi's session flags to the existing `SessionPolicy` (`harness.go:80-85`), consistent with [harness-authoring.md](../design/harness-authoring.md) §5:

| `SessionPolicy` | pi flags the bridge passes | Workspace |
|---|---|---|
| `ephemeral` (default) | `--no-session` (fresh, nothing saved) | `/tmp` unless durable storage binds CWD |
| `persistent` | `--session <stable-id>` (id derived from Agent+conversation) | reuse the AgentFS mount (`EffectiveWorkingDir`, `harness.go:291-304`) so file edits + the session file persist |

### 5.6 Bundle image

- **`deploy/docker/harness-pi-mono.Dockerfile`** (new) — clone of `harness-codex.Dockerfile` (`deploy/docker/harness-codex.Dockerfile`) with:
  - build `/agent` **and** `/pi-bridge` (or a single `/agent` with a `pi-bridge` subcommand) from `golang:1.26`;
  - runtime base `node:22-slim`; `npm install -g --ignore-scripts @earendil-works/pi-coding-agent@${PI_VERSION}` (default `latest`, pinned by `HarnessSpec.Version`); `pi --version` smoke check;
  - `apt-get install git ca-certificates` (pi shells out to git);
  - `ENV HOME=/tmp` (pi writes `~/.pi/...`); `USER 65532:65532`; `ENTRYPOINT ["/agent"]`.
- **`operator/internal/builders/harness_image.go:20-25`** — add `pure.HarnessPiMono: "harness-pi-mono"` to `perKindHarnessImage`. (HTTP-classified but *does* get a bundle, unlike hermes — because the "HTTP" here is in-pod loopback to a CLI we must carry.) `HarnessImage` (`harness_image.go:33-45`) already resolves it once the map entry exists.
- **Build/publish:** add to `build-images.sh` / `make images-push` (multiarch amd64+arm64 per the standing rule). New `HARNESS_PI_MONO_VERSION` build arg.
- **`harness_image_test.go`** — add a `pi-mono default bundle` → `/harness-pi-mono:0.2.0` case.

### 5.7 Credential plumbing (broker → bridge only)

- The Agent declares the provider key as `harness.env[].secretRef` (e.g. `name: ANTHROPIC_API_KEY`, `secretRef: ...`). Today the broker resolves it and the controller serves it over UDS; `mergeEnv` would expose it to the harness *and* pi. **Change:** for `kind=pi-mono`, the operator must route the lease into the **pi-bridge** env (placement (1): same container, so the bridge must fetch it from the UDS itself and scrub it before spawning pi — see §5.3). The harness env literals stamped at `agentrun.go:90-96` must **exclude** secret-backed entries for pi-mono (they already are — only `e.Value != ""` literals are stamped; secrets come via broker). Confirm no `*_API_KEY` literal leaks into the harness container env.
- See [dynamic-credential-backends.md](dynamic-credential-backends.md) for broker backends; this spec only needs the static lease path.

### 5.8 Interactive terminal (cross-link)

Interactive pi (its primary UX) is **out of scope to fully build here** but enabled by this design (the bridge can host a long-lived pi). The terminal transport (tmux + ttyd over HTTP, or SSH) is specified in [terminal-exposure-http-ssh-tmux.md](terminal-exposure-http-ssh-tmux.md). pi-specific note for that spec: attach to a `pi` (interactive) session inside the same kata-fc pod, CWD = AgentFS mount, with the same broker-injected key + egress cage.

---

## 6. Data / control flow

```
1. Operator creates Agent{mode: harness, harness:{kind: pi-mono, image?, version?,
   piMono:{model, mode, activeDeadlineSeconds?}, env:[{name: ANTHROPIC_API_KEY, secretRef}]}}.
2. Operator creates AgentRun{agentRef, input:{prompt}}.
3. agentrun_controller:
   a. resolves sandbox class (kata-fc) fail-closed → ApplyRunSandbox.
   b. renders run-spec ConfigMap (agent.json + run.json)         [runspec.go]
   c. BuildAgentRunPod → harness container (image = harness-pi-mono bundle),
      sets PodSpec.ActiveDeadlineSeconds (NEW, §5.4).
   d. AttachSecretBroker → UDS sidecar serving ANTHROPIC_API_KEY lease.
   e. BuildAgentRunEgressPolicy → default-deny egress (DNS+in-cluster+80/443; metadata blocked).
4. Pod starts. /agent run:
   a. starts pi-bridge on 127.0.0.1:8848; bridge fetches the key from the broker UDS,
      writes ~/.pi/agent/models.json (0600), waits ready.
   b. PiMonoHarness.Run → POST 127.0.0.1:8848/run {prompt, system, model, seed}.
   c. bridge spawns `pi --mode json --no-session -p <prompt> --model <m>` with a
      CHILD env scrubbed of *_API_KEY; pi loops read/write/edit/bash over CWD,
      reaching the model endpoint through the egress cage.
   d. bridge parses JSONL events → {output, tokensIn, tokensOut, toolCalls}.
   e. harness returns Response; /agent run folds it into the RunResult wire.
5. agentrun_controller reads the RunResult (Steps/output), writes AgentRun.Status.
   - If the pod hit ActiveDeadlineSeconds first → terminal AgentRun, reason=DeadlineExceeded.
```

---

## 7. Security model

How pi-mono composes with the existing controls (verified files cited in §2):

- **Sandbox.** pi runs inside the **kata-fc microVM** RuntimeClass (fail-closed; `resolveSandbox`/`ApplyRunSandbox`). pi's `bash` tool can do anything *inside* that VM — which is exactly the isolation boundary the pi author tells users to rely on ("Run in a container"). A pi `rm -rf /` only nukes the microVM rootfs, not the host/cluster.
- **Egress cage.** The static default-deny NetworkPolicy (`BuildAgentRunEgressPolicy`) blocks `169.254.169.254` (cloud metadata / credential theft) and limits the public internet to 80/443. pi's `bash` cannot exfiltrate to arbitrary ports. **Limitation:** the policy is static and **ignores AgentNetwork allow-lists** — a pi run can still reach *any* public 80/443 host (e.g. paste sites), so it is not a true allow-list. Tightening is [agentnetwork-datapath-enforcement.md](agentnetwork-datapath-enforcement.md)'s job; this spec does not change egress.
- **Credential blindness (the new surface + mitigation).** pi's first-class `bash` makes env-var key theft a real risk (`printenv ANTHROPIC_API_KEY`). The bridge design mitigates by (a) holding the key in the **bridge** process, (b) spawning pi with a **scrubbed child env**, and (c) handing pi the key via a 0600 config file. **This is defense-in-depth, NOT airtight:** pi runs as the same uid as `bash`, so `bash` can `cat ~/.pi/agent/models.json` and read the key. **Honest stance:** within a single sandbox we cannot fully hide a credential from a tool that has filesystem + shell access *and legitimately needs that credential to call the model*. The real containment is the microVM + egress cage limiting what a stolen key buys (the key only works against the model endpoint reachable through the cage). For provider creds that must be agent-blind (GitHub/GitLab push), use the **secretless egress broker** (TraT-authorized dynamic mint, agent never sees the token) per the egress-credentials feature — that pattern, not env/config, is the strong control. Document this clearly so operators don't over-trust the scrub.
- **SPIFFE / broker.** The broker UDS uses `SO_PEERCRED`/SPIRE attestation (per the runtime-and-identity model); only the pod's processes can lease. The bridge is the lease consumer; pi is not.
- **New attack surface introduced by the bridge:** a loopback HTTP server. Mitigations: bind `127.0.0.1` only (never `0.0.0.0`), no auth needed (same netns, single tenant), bound request/response sizes, and the bridge is the **only** writer of the pi config. The bridge must not echo the provider key in `/run` responses or logs.
- **No-max-step DoS.** Bounded by `activeDeadlineSeconds` (§5.4) + the wall-clock budget ctx + pod CPU/mem limits (`agentrun.go:102-111`).

---

## 8. Phasing & effort

| Phase | Scope | Effort | Depends on |
|---|---|---|---|
| **P0 — Naming** | Add `HarnessPiMono`; rename Inflection const + `"pi"` alias with deprecation warning; update false-friend doc. Unit tests for `Valid()`/alias. | **S** | — |
| **P1 — Bridge + harness (print mode), no key-scrub** | `cmd/pi-bridge` (`--print`/`--mode json` per request, JSONL parse, loopback), `PiMonoHarness`, register, `ValidateHarness` arm, `HarnessPiMonoSpec`. Key still via env (parity w/ other CLIs). Unit tests via `HTTPClient`/fake bridge. | **M** | P0 |
| **P2 — Bundle image + operator wiring** | `harness-pi-mono.Dockerfile`, `perKindHarnessImage` entry, build/publish multiarch, `/agent run` spawns bridge. | **M** | P1 |
| **P3 — activeDeadlineSeconds** | Generic `PodSpec.ActiveDeadlineSeconds` resolver + controller `DeadlineExceeded`→terminal mapping. | **S–M** | P2; aligns with [run-governance.md](run-governance.md) |
| **P4 — Credential scrub + Response richness** | Bridge fetches key from broker UDS, scrubs pi child env, writes 0600 config; harness populates `TokensIn/Out` + `ToolCalls` from JSONL. | **M** | P2; broker path ([dynamic-credential-backends.md](dynamic-credential-backends.md)) |
| **P5 — RPC mode (persistent pi)** | Bridge holds one `pi --mode rpc` child; lower per-request latency, stateful sessions. | **M** | P1 |
| **P6 — Interactive terminal** | Long-lived pi + tmux/ttyd/SSH. | **L** | [terminal-exposure-http-ssh-tmux.md](terminal-exposure-http-ssh-tmux.md) |

MVP "full support over HTTP" = **P0+P1+P2+P3** (declarative pi-mono Agent, HTTP-driven, sandboxed, bounded). P4–P6 harden + enrich.

---

## 9. Test plan

**Unit**
- `harness.go`: `HarnessPiMono.Valid()`; `"pi"` alias → `inflection-pi` with deprecation; `ValidateHarness` accepts `pi-mono` with no `http.url` and validates `PiMono` sub-block.
- `pi_test.go`: `PiMonoHarness.Run` against a fake `HTTPClient` bridge — asserts request body (`prompt/system/model/seed`), parses a canned JSONL→`Response` with non-zero `TokensIn/Out` + `ToolCalls` (the first CLI-family harness that can), and `ctx` cancel aborts.
- `cmd/pi-bridge`: spawn a **fake `pi`** (a tiny script emitting canned JSONL events incl. a `usage` line) via a `commandFunc`-style seam; assert argv (`--mode json --no-session -p ... --model ...`), `\n`-only line splitting, env scrub (no `*_API_KEY` in the child env), bounded output, and 0600 config write.
- `harness_image_test.go`: `pi-mono` → `harness-pi-mono:0.2.0`; explicit `harness.image` wins; `version` pins tag.
- `agentrun_test.go`: `ActiveDeadlineSeconds` set from budget+grace / explicit / default; pod still non-root/drop-ALL/seccomp; no `*_API_KEY` literal in harness container env.

**E2E** (the **cftest single-node k0s box** exists for live verification — see MEMORY)
- Build+publish `harness-pi-mono` multiarch; create an `Agent{kind: pi-mono, model: <z.ai or anthropic>, env:[ANTHROPIC_API_KEY secretRef]}` + an `AgentRun` with a coding prompt (e.g. "write fib(n) in fib.py and print fib(12)"); assert `AgentRun.Status` folds pi's output (and tokens once P4) and that `fib.py` lands on the AgentFS volume (persistent).
- Negative: an `AgentRun` whose prompt makes pi loop (no max-step) is killed by `activeDeadlineSeconds`; AgentRun terminal with `reason=DeadlineExceeded`.
- Security: exec into the pod, `printenv` in the harness container shows **no** provider key (P4); metadata endpoint unreachable from a pi `bash` (curl `169.254.169.254` fails).
- kata-fc note: the cftest box must have a working block-device snapshotter for kata-fc (devmapper/nydus) or the run falls back per the known `kata_fc_snapshotter` gotcha — verify RuntimeClass actually applied.

---

## 10. Risks & open decisions

**Open decisions (maintainer must choose):**
1. **Naming.** (a) rename `pi`→`inflection-pi` + alias (recommended, correct) vs (b) keep `pi`=Inflection, add only `pi-mono` (zero migration). Affects existing `kind: pi` Agents.
2. **Bridge placement.** Same container (v1, simplest) vs dedicated sidecar (cleaner, needed if the terminal spec wants a always-on bridge). 
3. **`activeDeadlineSeconds` home.** pi-specific field (this spec) vs a **generic** `AgentRunSpec.ActiveDeadlineSeconds`/`Budget` field (better; coordinate with [run-governance.md](run-governance.md) so we don't add two).
4. **Credential delivery to pi.** Config-file (0600) vs a pi env the bridge sets on the child only vs (strongest) never give pi the raw key and front the model with a broker-minted short-TTL token. Decision gates how honest §7 can be.
5. **Default `--model`.** Require it (safer; avoids pi picking a model the leased key can't use) vs allow pi's default.

**Risks:**
- **pi velocity.** The CLI's flags/package/org have already moved once (`badlogic`→`earendil-works`, `@mariozechner`→`@earendil-works`). Pin exact package+version; re-verify `--mode json` event schema and `--print`/`--no-session`/`--system-prompt` flags at implementation time (training cutoff Jan 2026; confirmed via web 2026-06-03).
- **`--mode json` event schema churn.** The bridge parses pi's JSONL; if the event shape changes, token/tool-call extraction breaks (output extraction is more robust). Keep the parser defensive (unknown events ignored; always fall back to concatenated assistant text).
- **Credential scrub is not airtight** (§7) — same-uid `bash` can read the 0600 config. Don't oversell it; the microVM + egress cage are the real containment, and secretless-broker is the strong pattern for must-be-blind creds.
- **kata-fc snapshotter** on some clusters (known gotcha) can silently drop the run to runc unless fail-closed catches it — verify in e2e.
- **No native HTTP mode** means we own the bridge forever; if pi later ships a server mode, revisit to drop the shim.
