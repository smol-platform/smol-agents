# CRD drift — intentional deltas from controller-gen

The CRDs in this directory are **hand-edited and deliberately NOT reproducible
from the Go types**. Running `make manifests` (controller-gen) would overwrite
the hand-authored descriptions and validation, so it is **never run blind**.

`scripts/crd-drift-check.sh` (CI job `crd-drift`, and `make crd-drift`)
regenerates CRDs from the Go types into a temp dir and diffs them against these
committed files. It is **informational** — it never mutates the committed CRDs
and the CI job is non-blocking. Use it to spot when a Go-type change (a new
field or kind) has not yet been ported into the hand-edited CRD.

## Known intentional deltas (drift expected here)

- **Every CRD** differs from generated output: hand-authored multi-line
  `description:` blocks, inline `{ type: ..., description: ... }` formatting, and
  curated validation that controller-gen does not emit. Cosmetic + intentional.

## Resolved

- **SmolAgent / SmolAgentPlatform removed.** The legacy serving CRDs
  (`smolagents.smol-agents.ai_smolagents.yaml`,
  `smolagents.smol-agents.ai_smolagentplatforms.yaml`) and their group-name
  drift note above are gone — the SmolAgent serving spine was deleted in favor
  of the agent-model CRD family (Agent/AgentRun/AgentSession/...) plus
  AgentNodePool, which now carries its own node-provisioning config directly
  (rehomed off the deleted SmolAgentPlatform singleton).

- **`ModelProvider.spec.secretRef.key`** (rv4.2). The hand-edited schema was
  missing the `key` property that Go's `AuthRef` has, so the apiserver pruned
  `spec.secretRef.key` and a provider secret had to hold exactly one key. The
  `key` property is now hand-added to
  `runtime.agents.smol-agents.ai_modelproviders.yaml`.

## When the drift check flags something new

1. A **new field** on an existing type → port the field into the hand-edited CRD
   (matching the surrounding hand-authored style), then re-run the check.
2. A **new API kind** ("NEW API type with no committed CRD") → hand-author its
   CRD from the generated one as a starting point.
3. Update this file's "Known intentional deltas" if the new drift is deliberate.
