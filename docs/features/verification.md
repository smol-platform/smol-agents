# Verification

> "Verifiable by default" is a build artifact, not a slogan: critical state
> machines are model-checked in Quint, runtime properties are exercised with
> `rapid`, and the whole stack runs across three escalating e2e rings.
> **Spec:** `.spec-workflow/specs/smol-agents-fullstack-e2e/`.
> **Code:** `spec/quint/`, `test/e2e/`, `cmd/{spiffe-probe,ebpf-probe}`.

## The principle

If a property is critical, it is also model-checked or property-tested. Comments
are not specifications; Quint specs are. Every requirement tagged in a
`requirements.md` is cited by at least one Quint invariant or `rapid` property,
and every step of the build emits a check you can re-run locally.

```bash
make verify          # vet + lint + test (-race) + verify-formal
```

## Layer 1 — Formal models (Quint)

Ten specifications under `spec/quint/`, each typechecked and run against a
`Safety` invariant by `make verify-formal`:

| Spec | What it proves |
|---|---|
| `identity.qnt` | SVID rotation never serves an expired/!valid identity. |
| `secrets.qnt` | The broker handshake only leases to an attested, policy-permitted caller. |
| `agent_lifecycle.qnt` | The agent never serves before Ready / never skips drain. |
| `agent_execution.qnt` | The budget cap (`maxSteps`/`maxTokens`/…) is never exceeded. |
| `operator_lifecycle.qnt` | A feature is enabled only when its prerequisites hold. |
| `agentnet.qnt` | Only allow-listed destinations egress; redirect is loss-free. |
| `agentfs.qnt` | Filesystem backup/restore/WAL invariants. |
| `secretless_egress.qnt` | A minted credential reaches only the authorized host; TraT is sender-bound. |
| `memory_access.qnt` | A document returns only to an identity granted read on its namespace+tenant. |
| `memory_merge.qnt` | 3-way merge never silently drops or corrupts a conflicting edit. |

```bash
make verify-formal
# === typecheck spec/quint/identity.qnt ===
# === invariant Safety: spec/quint/identity.qnt ===
# ... (×10) ...
# === formal verification PASSED ===

# Explore one interactively:
quint repl   spec/quint/secrets.qnt
quint run    spec/quint/memory_access.qnt --invariant=Safety --max-samples=5000 --verbosity=3
```

## Layer 2 — Property tests (`rapid`)

Runtime invariants the model can't reach (real serialization, real timing) are
exercised with `pgregory.net/rapid` generative property tests, run under the race
detector:

```bash
make test            # unit + rapid property suite, -race -count=1
```

These cover SVID rotation timing, broker lease accounting, the agent runtime's
determinism/replay, the memory backends, and the AgentFS merge classification.

## Layer 3 — Controller tests (`envtest`)

The operator's controllers are tested against a real API server with `envtest`:
child-object create/update/delete, owner references, and finalizer teardown.

```bash
make envtest         # operator/internal/controllers/... against KUBEBUILDER_ASSETS
```

## Layer 4 — End-to-end rings

Three rings of increasing fidelity and cost run against **real binaries**, using
in-cluster **probes** (`cmd/spiffe-probe` for SPIFFE-dependent assertions,
`cmd/ebpf-probe` for cgroup eBPF assertions) and deterministic **fakes**
(`fake-llm`, `fake-gateway`, `fake-github`, `fake-tts`).

| Ring | Substrate | Time | Cost | Command |
|---|---|---|---|---|
| **L0** | docker-compose | ~1 min | free | `make e2e-l0` |
| **L1** | kind (Linux) | ~5 min | free | `make e2e-l1` |
| **L2** | AWS Spot bare-metal (k0s) | ~12 min | ~USD 0.22/run | `make e2e-l2` |

L2 has wider variants for distro and ingress coverage:

```bash
make e2e-l2-smoke        # Provision + Teardown only (~6 min)
make e2e-l2-alldistros   # AL2023 + Ubuntu + Flatcar + Fedora CoreOS, NodePort ingress
make e2e-clean-aws       # terminate any stranded e2e instances (sweeper)
```

> All L2 targets run in `us-east-2` under the sandbox AWS profile (the IAM role,
> sweeper Lambda, and budget alarm only exist there) and self-terminate per run.
> See [docs/runbooks/e2e-l2.md](../runbooks/e2e-l2.md).

The L2 ring drives full scenarios end-to-end — agent up under kata-fc, SPIFFE
identity issued, secretless GitHub egress, memory write→retrieve with tenant
isolation, eBPF egress drop/redirect — on freshly bootstrapped bare-metal nodes.

## CI

`.github/workflows/ci.yaml` runs the fast layers (vet, lint, `make test`,
`make verify-formal`) on every push; `.github/workflows/e2e.yml` drives the e2e
rings. The badges on the [README](../../README.md) reflect their live status.

## Why four layers

Each catches what the others can't: Quint finds *design* bugs in concurrent
state machines before any code exists; `rapid` finds *implementation* bugs in
real serialization and timing; `envtest` finds *reconciliation* bugs against a
real API server; the e2e rings find *integration* bugs — kernel, sandbox,
network, and identity interacting on real (or near-real) infrastructure.

## See also

- Every feature guide ends with a **"Proven by"** link to its Quint spec.
- [INSTALL §8](../INSTALL.md) — the full verification command cheat-sheet.
