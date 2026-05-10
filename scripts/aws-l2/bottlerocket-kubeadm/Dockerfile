# Bottlerocket kubeadm bootstrap-container.
#
# Pulled at first boot via [settings.bootstrap-containers.kubeadm].
# Bundles kubeadm + kubectl + bash + awscli; the entrypoint scripts
# kubeadm phases, stages static pods via apiclient, then exits.
# After exit, Bottlerocket's static-pods service materialises the
# manifests at /etc/kubernetes/static-pods/, kubelet runs them as
# the cluster's control plane, and the node registers itself.
#
# Pattern source: https://github.com/aws/eks-anywhere-build-tooling/
# tree/main/projects/aws/bottlerocket-bootstrap (simplified for our
# single-node smoke — no joins, no etcdadm, no kube-vip).

FROM docker.io/library/ubuntu:24.04

RUN apt-get update -qq \
    && apt-get install -y -qq --no-install-recommends \
        bash curl unzip ca-certificates dpkg \
    && rm -rf /var/lib/apt/lists/* \
    && mkdir -p /opt/bin

# AWS CLI v2 (for ECR auth + S3 manifest fetch)
RUN curl -sSL https://awscli.amazonaws.com/awscli-exe-linux-aarch64.zip -o /tmp/aws.zip \
    && unzip -q /tmp/aws.zip -d /tmp \
    && /tmp/aws/install \
    && rm -rf /tmp/aws /tmp/aws.zip

# kubeadm + kubectl, pinned to match the Bottlerocket aws-k8s-1.31
# variant's kubelet version.
ARG K8S_VERSION=v1.31.5
RUN curl -sSL --fail \
        "https://dl.k8s.io/release/${K8S_VERSION}/bin/linux/arm64/kubeadm" \
        -o /opt/bin/kubeadm \
    && chmod +x /opt/bin/kubeadm \
    && curl -sSL --fail \
        "https://dl.k8s.io/release/${K8S_VERSION}/bin/linux/arm64/kubectl" \
        -o /opt/bin/kubectl \
    && chmod +x /opt/bin/kubectl

ENV PATH=/opt/bin:$PATH

COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

CMD ["/entrypoint.sh"]
