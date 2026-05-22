#!/bin/sh
# bootstrap.sh — generate a SPIRE join token + register workload
# entries for the L0 ring. Idempotent (re-running updates entries).
set -eu

SOCKET=/tmp/spire-server/private/api.sock
TOKEN_FILE=/tmp/spire/agent-token

# 1. Generate a join token. The token's parent SPIFFE ID becomes
#    spiffe://smol-agents.ai/spire/agent/join_token/<token>.
TOKEN=$(/opt/spire/bin/spire-server token generate -socketPath "$SOCKET" -output json | jq -r .value)
if [ -z "$TOKEN" ] || [ "$TOKEN" = "null" ]; then
  echo "failed to generate token" >&2
  exit 1
fi
echo "$TOKEN" > "$TOKEN_FILE"
PARENT_ID="spiffe://smol-agents.ai/spire/agent/join_token/$TOKEN"
echo "token=$TOKEN parent=$PARENT_ID"

# 2. Register workload entries.
#    fake-gateway: runs as root in the test rig (uid 0).
/opt/spire/bin/spire-server entry create \
  -socketPath "$SOCKET" \
  -spiffeID spiffe://smol-agents.ai/ns/tenant-a/sa/fake-gateway \
  -parentID "$PARENT_ID" \
  -selector unix:uid:0 \
  -selector unix:user:root \
  || true

#    Test driver runs as the host user invoking `go test`. Register
#    a wide UID range to cover common dev/CI users (root, 501, 1000+).
for uid in 0 501 1000 1001; do
  /opt/spire/bin/spire-server entry create \
    -socketPath "$SOCKET" \
    -spiffeID spiffe://smol-agents.ai/ns/tenant-a/sa/agent \
    -parentID "$PARENT_ID" \
    -selector unix:uid:$uid \
    || true
done

echo "spire-init complete"
