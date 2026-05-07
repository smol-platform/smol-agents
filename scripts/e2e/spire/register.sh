#!/bin/sh
# Register SPIRE entries for the L0 stack. Idempotent — re-running
# updates entries instead of duplicating.
#
# Workloads:
#   - fake-gateway: SPIFFE ID spiffe://stigen.ai/ns/tenant-a/sa/fake-gateway
#   - test-driver: SPIFFE ID spiffe://stigen.ai/ns/tenant-a/sa/agent
#
# Both attest via unix uid/gid (the SPIRE agent's WorkloadAttestor "unix").
set -eu

SOCKET=/tmp/spire-server/private/api.sock

# fake-gateway runs as nonroot (uid 65532) per its distroless base.
/opt/spire/bin/spire-server entry create \
  -socketPath $SOCKET \
  -spiffeID spiffe://stigen.ai/ns/tenant-a/sa/fake-gateway \
  -parentID spiffe://stigen.ai/spire/agent/join_token/$(cat /run/spire/data/agent-token 2>/dev/null || echo PLACEHOLDER) \
  -selector unix:uid:65532 \
  || true

# Test driver runs on the host as the user invoking `go test`.
# We register a wide UID range so any local user works; tighter
# entries can replace this in CI where the runner UID is known.
/opt/spire/bin/spire-server entry create \
  -socketPath $SOCKET \
  -spiffeID spiffe://stigen.ai/ns/tenant-a/sa/agent \
  -parentID spiffe://stigen.ai/spire/agent/join_token/$(cat /run/spire/data/agent-token 2>/dev/null || echo PLACEHOLDER) \
  -selector unix:uid:0 \
  -selector unix:uid:1000 \
  -selector unix:uid:1001 \
  || true

echo "SPIRE entries registered"
