# Bottlerocket bootstrap container.
#
# Pulled by Bottlerocket's settings.bootstrap-containers facility on
# first boot; runs the entrypoint script with /.bottlerocket/rootfs
# bind-mounted (via Bottlerocket's defaults). The script writes the
# k0s install + manifest apply + health gate, same shape as the
# AL2023/Ubuntu/Flatcar templates.
#
# Bottlerocket constraint: the host /usr is read-only and there's
# no shell on the host. Everything we install must land under /var
# (writable). k0s + its bundled binaries DO live under /var/lib/k0s
# by default, so this works.

FROM docker.io/library/ubuntu:24.04

RUN apt-get update -qq \
    && apt-get install -y -qq --no-install-recommends \
        bash curl unzip ca-certificates dpkg \
    && rm -rf /var/lib/apt/lists/*

# AWS CLI v2 (needed for ECR auth + S3 manifest pull)
RUN curl -sSL https://awscli.amazonaws.com/awscli-exe-linux-aarch64.zip -o /tmp/aws.zip \
    && unzip -q /tmp/aws.zip -d /tmp \
    && /tmp/aws/install \
    && rm -rf /tmp/aws /tmp/aws.zip

# k0s binary (single-arch arm64 since Bottlerocket arm64 = c7g/etc.)
ARG K0S_VERSION=v1.35.3+k0s.0
RUN curl -sSL --fail \
        "https://github.com/k0sproject/k0s/releases/download/${K0S_VERSION}/k0s-${K0S_VERSION}-arm64" \
        -o /usr/local/bin/k0s \
    && chmod +x /usr/local/bin/k0s

COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

ENTRYPOINT ["/entrypoint.sh"]
