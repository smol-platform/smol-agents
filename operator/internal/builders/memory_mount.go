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
// Mount wiring approach:
//
//	This file provides a helper function AttachMemoryFS(pod, spec, mountSpec)
//	that appends an EmptyDir volume named "memory-agentfs" and adds a
//	corresponding VolumeMount to every container in the pod that doesn't already
//	have the volume mounted.
//
//	The function is modelled after the existing AgentFS storage wiring in
//	agentrun.go (agent.Spec.Storage.AgentFS path) so both use the same volume
//	shape — EmptyDir for local SQLite + AgentFS sidecar management. The volume
//	name differs ("memory-agentfs") to avoid collision with the storage AgentFS
//	volume ("agentfs") that an agent may already have.
//
// The operator wires this during AgentRun reconciliation: after resolving the
// MemoryRetriever referenced by the AgentRun's memoryRetrieverRef, it calls
// AttachMemoryFS on the rendered pod if the retriever is a filesystem kind with
// Mount.Enabled.
package builders

import (
	corev1 "k8s.io/api/core/v1"

	pure "github.com/stigen/smol-agents/pkg/agentmodel/v1"
)

const (
	// memoryFSVolumeName is the volume name for the filesystem MemoryRetriever
	// AgentFS mount. Distinct from "agentfs" (Agent.Spec.Storage AgentFS) to
	// allow both to coexist in the same pod.
	memoryFSVolumeName = "memory-agentfs"

	// defaultMemoryMountPath is the fallback when MountSpec.MountPath is empty.
	defaultMemoryMountPath = "/var/memory-agentfs"
)

// MemoryMountInput carries the resolved configuration needed to attach a
// filesystem MemoryRetriever mount to a pod.
type MemoryMountInput struct {
	// AgentFS is the AgentFSSpec from the MemoryStore (size, image, backup, etc.).
	// Used to configure the mount size and backup parameters.
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

// AttachMemoryFS appends the memory AgentFS volume and volume mounts to pod.
// It is a pure transformation: it modifies pod in-place and returns it for
// chaining. A no-op when input.MountEnabled() is false.
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

	// Append the EmptyDir volume (same shape as the storage AgentFS volume in
	// agentrun.go; the AgentFS sidecar manages the SQLite DB inside it).
	pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
		Name:         memoryFSVolumeName,
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
	})

	// Mount into every existing container.
	vm := corev1.VolumeMount{Name: memoryFSVolumeName, MountPath: mp}
	for i := range pod.Spec.Containers {
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
	vol := corev1.Volume{
		Name:         memoryFSVolumeName,
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
	}
	vm := corev1.VolumeMount{
		Name:      memoryFSVolumeName,
		MountPath: input.MountPath(),
	}
	return vol, vm, true
}
