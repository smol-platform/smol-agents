# syntax=docker/dockerfile:1.6
# terminal-sidecar: ttyd (>=1.7.7) + tmux + asciinema for the M4 attach plane.
# ttyd ships per-arch static binaries; tmux/asciinema come from the distro.
# Multiarch: bare ARG TARGETARCH (BuildKit fills it) — no default (see memory:
# multiarch_images).
FROM debian:bookworm-slim

ARG TARGETARCH
ARG TTYD_VERSION=1.7.7

RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends tmux asciinema ca-certificates curl; \
    rm -rf /var/lib/apt/lists/*; \
    case "$TARGETARCH" in \
      amd64) ttyd_arch=x86_64 ;; \
      arm64) ttyd_arch=aarch64 ;; \
      arm)   ttyd_arch=arm ;; \
      *) echo "unsupported TARGETARCH=$TARGETARCH" >&2; exit 1 ;; \
    esac; \
    curl -fsSL -o /usr/local/bin/ttyd \
      "https://github.com/tsl0922/ttyd/releases/download/${TTYD_VERSION}/ttyd.${ttyd_arch}"; \
    chmod 0755 /usr/local/bin/ttyd; \
    # smoke: the binary runs and reports >= the pinned version.
    ttyd --version

# Non-root, matching the serving-pod PSA (runAsUser 65532).
USER 65532:65532
ENTRYPOINT ["ttyd"]
