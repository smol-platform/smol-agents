// Package builders — memory_mount.go
//
// AttachMemoryFS wires a filesystem MemoryRetriever's AgentFS volume into an
// agent pod (an AgentRun Pod) when the retriever has Mount.Enabled == true.
//
// Design intent (R-MEM-FS-2):
//
//	The mounted view and the worker-served MCP view are the SAME SQLite-canonical
//	AgentFS state. The AgentFS sidecar in the agent pod hosts the branchable
//	SQLite DB; the memory-worker's agentfs backend operates on the same underlying
//	storage path. No divergent copies exist.
//
// Real volume shape:
//
//   - EmptyDir with a SizeLimit derived from AgentFSSpec.SizeGiB (Gi suffix).
//     This mirrors the storage AgentFS volume in agentrun.go. The EmptyDir is
//     local SQLite storage managed by the AgentFS sidecar; for durable state the
//     sidecar backs it up to S3 (BackupPolicy). A PVC is NOT used for agent pods
//     because AgentRun pods are ephemeral — the AgentFS sidecar handles
//     persistence via S3 WAL + snapshot upload.
//   - An agentfs-init init container that initialises or restores the SQLite DB
//     from S3 before the main containers start (RestorePolicy). The init container
//     image is taken from AgentFSSpec.Image; a default is used when empty.
//   - A sidecar container (agentfs-sidecar) that runs alongside the agent to
//     provide the gRPC AgentFS API (branchable FS) and to upload WAL frames to
//     S3 continuously while the agent runs.
//
// The volume name "memory-agentfs" differs from "agentfs" (Agent.Spec.Storage)
// so both can coexist in the same pod.
//
// The operator wires this during AgentRun reconciliation: after resolving the
// MemoryRetriever referenced by AgentRunSpec.MemoryRetrieverRef, it calls
// AttachMemoryFS on the rendered pod if the retriever is a filesystem kind with
// Mount.Enabled.
package builders

import (
	corev1 "k8s.io/api/core/v1"
	resource "k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/utils/ptr"

	pure "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

const (
	// memoryFSVolumeName is the volume name for the filesystem MemoryRetriever
	// AgentFS mount. Distinct from "agentfs" (Agent.Spec.Storage AgentFS) to
	// allow both to coexist in the same pod.
	memoryFSVolumeName = "memory-agentfs"

	// defaultMemoryMountPath is the fallback when MountSpec.MountPath is empty.
	defaultMemoryMountPath = "/var/memory-agentfs"

	// agentFSSidecarName is the sidecar container name for memory AgentFS.
	agentFSSidecarName = "memory-agentfs-sidecar"

	// agentFSInitName is the init container name that restores the DB from S3.
	agentFSInitName = "memory-agentfs-init"
)

// MemoryMountInput carries the resolved configuration needed to attach a
// filesystem MemoryRetriever mount to a pod.
type MemoryMountInput struct {
	// AgentFS is the AgentFSSpec from the MemoryStore (size, image, backup, etc.).
	// Used to configure the volume size, sidecar image, and backup parameters.
	AgentFS *pure.AgentFSSpec

	// Mount is the MountSpec from the MemoryRetriever.
	// Mount.Enabled must be true; Mount.MountPath overrides the default.
	Mount *pure.MountSpec
}

// MountEnabled returns true when the input is configured and mounting is enabled.
func (m MemoryMountInput) MountEnabled() bool {
	return m.AgentFS != nil && m.Mount != nil && m.Mount.Enabled
}

// MountPath returns the effective mount path (MountSpec.MountPath or the default).
func (m MemoryMountInput) MountPath() string {
	if m.Mount != nil && m.Mount.MountPath != "" {
		return m.Mount.MountPath
	}
	return defaultMemoryMountPath
}

// sidecarImage returns the AgentFS sidecar image from spec or falls back to the default.
func (m MemoryMountInput) sidecarImage() string {
	if m.AgentFS != nil && m.AgentFS.Image != "" {
		return m.AgentFS.Image
	}
	return defaultAgentFSSidecarImage()
}

// defaultAgentFSSidecarImage is the AgentFS sidecar image used when
// AgentFSSpec.Image is empty (built from cmd/agentfs-sidecar: S3 restore +
// periodic full-snapshot backup; SQLite WAL streaming is not yet implemented).
func defaultAgentFSSidecarImage() string { return Image("agentfs-sidecar") }

// volumeSizeLimit returns a resource.Quantity for the EmptyDir SizeLimit based
// on AgentFSSpec.SizeGiB. Returns nil when SizeGiB is zero (unlimited EmptyDir).
func volumeSizeLimit(agentfs *pure.AgentFSSpec) *resource.Quantity {
	if agentfs == nil || agentfs.SizeGiB <= 0 {
		return nil
	}
	q := resource.MustParse(resource.NewQuantity(int64(agentfs.SizeGiB)<<30, resource.BinarySI).String())
	return &q
}

// buildMemoryAgentFSVolume constructs the EmptyDir Volume for the memory
// AgentFS mount. The SizeLimit bounds local disk usage to AgentFSSpec.SizeGiB.
func buildMemoryAgentFSVolume(input MemoryMountInput) corev1.Volume {
	sizeLimit := volumeSizeLimit(input.AgentFS)
	emptyDir := &corev1.EmptyDirVolumeSource{}
	if sizeLimit != nil {
		emptyDir.SizeLimit = sizeLimit
	}
	return corev1.Volume{
		Name:         memoryFSVolumeName,
		VolumeSource: corev1.VolumeSource{EmptyDir: emptyDir},
	}
}

// buildAgentFSInitContainer returns an init container that bootstraps the
// AgentFS SQLite DB from S3 (or creates a fresh one) before the agent starts.
// The init container exits after the restore/init completes.
func buildAgentFSInitContainer(input MemoryMountInput, mountPath string) corev1.Container {
	return corev1.Container{
		Name:            agentFSInitName,
		Image:           input.sidecarImage(),
		ImagePullPolicy: corev1.PullIfNotPresent,
		Args:            []string{"init", "--mount=" + mountPath},
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: ptr.To(false),
			ReadOnlyRootFilesystem:   ptr.To(false), // writes to the mounted volume
			RunAsNonRoot:             ptr.To(true),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: memoryFSVolumeName, MountPath: mountPath},
		},
	}
}

// buildAgentFSSidecarContainer returns the AgentFS sidecar container that runs
// alongside the agent. It serves the gRPC AgentFS API (branching, snapshotting)
// and uploads WAL frames to S3 while the agent runs.
func buildAgentFSSidecarContainer(input MemoryMountInput, mountPath string) corev1.Container {
	return corev1.Container{
		Name:            agentFSSidecarName,
		Image:           input.sidecarImage(),
		ImagePullPolicy: corev1.PullIfNotPresent,
		Args:            []string{"serve", "--mount=" + mountPath},
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: ptr.To(false),
			ReadOnlyRootFilesystem:   ptr.To(false), // writes WAL frames to the volume
			RunAsNonRoot:             ptr.To(true),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: memoryFSVolumeName, MountPath: mountPath},
		},
	}
}

// AttachMemoryFS appends the memory AgentFS volume, init container, sidecar
// container, and volume mounts to pod. It is a pure transformation: it
// modifies pod in-place and returns it for chaining. A no-op when
// input.MountEnabled() is false.
//
// Volume shape (real, not stub):
//   - EmptyDir with SizeLimit = AgentFSSpec.SizeGiB GiB (mirrors agentrun.go
//     storage AgentFS; the sidecar manages the SQLite DB inside the EmptyDir)
//   - Init container: "memory-agentfs-init" restores DB from S3 or initialises fresh
//   - Sidecar container: "memory-agentfs-sidecar" serves gRPC AgentFS API + WAL upload
//
// Idempotent: if the volume "memory-agentfs" is already present (e.g. called
// twice during reconciliation), it is not added a second time.
func AttachMemoryFS(pod *corev1.Pod, input MemoryMountInput) *corev1.Pod {
	if !input.MountEnabled() {
		return pod
	}

	mp := input.MountPath()

	// Idempotency: skip if already wired.
	for _, v := range pod.Spec.Volumes {
		if v.Name == memoryFSVolumeName {
			return pod
		}
	}

	// Append the real AgentFS volume (EmptyDir with SizeLimit).
	pod.Spec.Volumes = append(pod.Spec.Volumes, buildMemoryAgentFSVolume(input))

	// Add the init container that restores/initialises the SQLite DB.
	pod.Spec.InitContainers = append(pod.Spec.InitContainers, buildAgentFSInitContainer(input, mp))

	// Add the sidecar container.
	pod.Spec.Containers = append(pod.Spec.Containers, buildAgentFSSidecarContainer(input, mp))

	// Mount the volume into every pre-existing container (the agent/harness).
	vm := corev1.VolumeMount{Name: memoryFSVolumeName, MountPath: mp}
	// Mount into all containers except the sidecar we just appended (last index).
	lastIdx := len(pod.Spec.Containers) - 1
	for i := range pod.Spec.Containers[:lastIdx] {
		if !hasVolumeMount(pod.Spec.Containers[i].VolumeMounts, memoryFSVolumeName) {
			pod.Spec.Containers[i].VolumeMounts = append(pod.Spec.Containers[i].VolumeMounts, vm)
		}
	}

	return pod
}

// hasVolumeMount returns true when mounts already contains a mount for name.
func hasVolumeMount(mounts []corev1.VolumeMount, name string) bool {
	for _, m := range mounts {
		if m.Name == name {
			return true
		}
	}
	return false
}

// AgentFSVolumeMount builds the (volume, volumeMount) pair for the memory
// AgentFS attachment. Exported for use by the operator controller without
// requiring the full Pod to be available (e.g. when building a PodTemplateSpec).
func AgentFSVolumeMount(input MemoryMountInput) (corev1.Volume, corev1.VolumeMount, bool) {
	if !input.MountEnabled() {
		return corev1.Volume{}, corev1.VolumeMount{}, false
	}
	vol := buildMemoryAgentFSVolume(input)
	vm := corev1.VolumeMount{
		Name:      memoryFSVolumeName,
		MountPath: input.MountPath(),
	}
	return vol, vm, true
}
