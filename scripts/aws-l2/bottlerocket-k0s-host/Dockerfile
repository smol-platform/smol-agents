# Bottlerocket "k0s" host-container.
#
# Runs k0s controller --single inside a long-lived Bottlerocket
# host-container declared via [settings.host-containers.k0s] with
# superpowered=true. The container has:
#   - host PID + host network namespace
#   - all caps + all devices (KVM/eBPF reachable)
#   - /.bottlerocket/rootfs mounted at /.bottlerocket/rootfs (the
#     host's / read-only)
#   - /.bottlerocket/host-containers/k0s persistent rw under
#     /local/host-containers/k0s in-container
#
# Lifecycle: systemd unit host-containers@k0s.service starts after
# multi-user.target with Restart=always. We piggyback on the
# upstream k0s image's tini + entrypoint, but replace the default
# command to run our wrapper.sh which:
#   1. sources user-data (ARTIFACT_BUCKET etc.) from
#      /.bottlerocket/host-containers/current/user-data
#   2. configures k0s containerd ECR auth
#   3. starts k0s controller --single in the background
#   4. drops manifests + cert-manager into k0s manifest watcher
#   5. runs the same health gate as AL2023/Ubuntu/Flatcar
#   6. writes /var/log/l2-bootstrap.READY (visible on host as
#      /.bottlerocket/rootfs/var/log/...)
#   7. waits on k0s PID so the container persists.

# k0s v1.33 still tolerates kernel-cgroup-v1 (Bottlerocket's
# host-containers are launched with the legacy v1 hierarchy in
# /proc/self/cgroup, even though the host runs unified v2). 1.34+
# kubelet refuses to start on v1; 1.33 still warns-and-continues.
FROM docker.io/k0sproject/k0s:v1.33.10-k0s.0

# The upstream image is alpine-based; layer in awscli + curl.
# python3 + py3-pip are needed for awscli v1 (alpine ships v1).
USER root
RUN apk add --no-cache aws-cli bash curl tar coreutils

COPY wrapper.sh /usr/local/bin/wrapper.sh
RUN chmod +x /usr/local/bin/wrapper.sh

# host-containers' command field in the TOML overrides this; setting
# it explicitly here so the image is testable in isolation.
ENTRYPOINT ["/sbin/tini", "--"]
CMD ["/usr/local/bin/wrapper.sh"]
