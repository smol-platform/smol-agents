#!/usr/bin/env bash
# crd-drift-check.sh — make CRD drift VISIBLE without ever blindly applying it.
#
# operator/config/crd is HAND-EDITED and deliberately NOT reproducible from the
# Go types (richer descriptions, hand-authored validation, intentional schema
# deltas — see operator/config/crd/DRIFT.md). So `make manifests` is never run
# blind. This check regenerates CRDs from the Go types into a TEMP dir and diffs
# them against the committed files, printing what controller-gen would produce.
#
# It is INFORMATIONAL: a reviewer reads the diff to spot when a Go type change
# (a new field, a new API kind) has NOT yet been reflected into the hand-edited
# CRD. It does not mutate the committed CRDs. Exit code is non-zero when drift
# exists so CI can surface it (the CI job runs it continue-on-error, so expected
# hand-edit drift never breaks the build — it just shows up in the logs).
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"
CONTROLLER_GEN="${CONTROLLER_GEN:-$(go env GOPATH)/bin/controller-gen}"

if [[ ! -x "$CONTROLLER_GEN" ]]; then
  echo "installing controller-gen v0.16.5..."
  go install sigs.k8s.io/controller-tools/cmd/controller-gen@v0.16.5
fi

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

"$CONTROLLER_GEN" crd:crdVersions=v1,allowDangerousTypes=true \
  paths=./operator/api/... output:crd:dir="$TMP" >/dev/null

echo "=== CRD drift: committed (operator/config/crd) vs controller-gen output ==="
echo "    CRDs are hand-edited on purpose; see operator/config/crd/DRIFT.md."
echo

drift=0
for gen in "$TMP"/*.yaml; do
  base="$(basename "$gen")"
  committed="operator/config/crd/$base"
  if [[ ! -f "$committed" ]]; then
    echo "!! NEW API type with no committed CRD: $base (hand-author one)"
    drift=1
    continue
  fi
  if ! diff -u "$committed" "$gen" >/dev/null 2>&1; then
    echo "~~ drift in $base (committed <-> generated):"
    diff -u "$committed" "$gen" || true
    echo
    drift=1
  fi
done

# A committed CRD whose API type was deleted from Go would never be regenerated;
# flag it so a stale CRD is visible too.
for committed in operator/config/crd/*.yaml; do
  base="$(basename "$committed")"
  [[ "$base" == "kustomization.yaml" ]] && continue # the kustomize index, not a CRD
  [[ -f "$TMP/$base" ]] || echo "?? committed CRD with no generated counterpart: $base (orphan or non-Go CRD)"
done

if [[ "$drift" -eq 0 ]]; then
  echo "No drift: committed CRDs match controller-gen output."
else
  echo "Drift found (expected for hand-edited CRDs). Review above; port any"
  echo "missing Go-type changes into the hand-edited CRD, then update DRIFT.md."
fi
exit "$drift"
