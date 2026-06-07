#!/bin/sh
# OpenClaw daemon entrypoint (M4.22): link the operator-rendered openclaw.json
# into ~/.openclaw, start a tiny :8080 readiness shim (OpenClaw's own control
# plane is :18789), then exec the daemon. The config's ${VAR} provider key is
# resolved from the broker-injected env by OpenClaw itself.
set -eu

mkdir -p "$HOME/.openclaw"
if [ -f /etc/openclaw/openclaw.json ]; then
  ln -sf /etc/openclaw/openclaw.json "$HOME/.openclaw/openclaw.json"
fi

# Minimal readiness endpoint on :8080 for the serving-path probe.
( while true; do
    printf 'HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok' | nc -l -p 8080 -q 1 2>/dev/null || sleep 1
  done ) &

exec openclaw serve
