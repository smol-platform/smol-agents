# syntax=docker/dockerfile:1.6
#
# bpf-builder produces the CO-RE .bpf.o artifacts that the
# ebpf-loader image embeds. It's an arm64 build container with
# clang/llvm + libbpf-dev and a pinned generic vmlinux.h so the
# objects work across any kernel >= the one vmlinux.h was emitted
# from. We pull the header from libbpf/vmlinux.h@main (kernel 6.19),
# which covers the AL2023 kernel-6.12 AMI used by L2.
#
# Build is invocation-style:
#
#   docker buildx build --platform linux/arm64 \
#     --file deploy/docker/bpf-builder.Dockerfile \
#     --target export --output type=local,dest=. .
#
# That emits bpf/build/*.bpf.o into the workspace.

FROM debian:bookworm AS build
RUN apt-get update && \
    apt-get install -y --no-install-recommends \
      clang llvm libbpf-dev linux-libc-dev curl ca-certificates && \
    rm -rf /var/lib/apt/lists/*
RUN curl -fsSL -o /opt/vmlinux.h \
      https://raw.githubusercontent.com/libbpf/vmlinux.h/main/include/aarch64/vmlinux_6.19.h
WORKDIR /src
COPY bpf/programs /src/programs
RUN mkdir -p /out && cp /opt/vmlinux.h /src/programs/vmlinux.h && \
    cd /src/programs && \
    for f in *.bpf.c; do \
      out=/out/${f%.c}.o ; \
      echo "BUILD $f -> $out" ; \
      clang -O2 -g -Wall \
        -Wno-missing-declarations \
        -target bpf -D__TARGET_ARCH_arm64 \
        -I . -c "$f" -o "$out" ; \
    done && \
    ls -la /out

# Stage that gets exported to the host filesystem. Buildkit's
# `--output type=local` writes everything in this stage to the host.
FROM scratch AS export
COPY --from=build /out/ /bpf/build/
