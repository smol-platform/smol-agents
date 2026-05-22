package builders

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	amv1 "github.com/stigen/smol-agents/operator/api/agentmodel/v1"
	pure "github.com/stigen/smol-agents/pkg/agentmodel/v1"
)

// ── helpers ───────────────────────────────────────────────────────────────────

func minimalPod(name string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "test"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "agent", Image: "smol-agents/agent:0.1.0"},
			},
		},
	}
}

func enabledInput(mountPath string) MemoryMountInput {
	mp := mountPath
	if mp == "" {
		mp = defaultMemoryMountPath
	}
	return MemoryMountInput{
		AgentFS: &pure.AgentFSSpec{SizeGiB: 1, MountPath: "/var/agentfs"},
		Mount:   &pure.MountSpec{Enabled: true, MountPath: mp},
	}
}

func hasVolume(pod *corev1.Pod, name string) bool {
	for _, v := range pod.Spec.Volumes {
		if v.Name == name {
			return true
		}
	}
	return false
}

func hasMount(container corev1.Container, name string) (string, bool) {
	for _, vm := range container.VolumeMounts {
		if vm.Name == name {
			return vm.MountPath, true
		}
	}
	return "", false
}

// ── AttachMemoryFS tests ─────────────────────────────────────────────────────

func TestAttachMemoryFS_AddsVolumeAndMount(t *testing.T) {
	pod := minimalPod("run-1")
	input := enabledInput("/var/memory-agentfs")

	AttachMemoryFS(pod, input)

	if !hasVolume(pod, memoryFSVolumeName) {
		t.Errorf("volume %q not added", memoryFSVolumeName)
	}
	mp, ok := hasMount(pod.Spec.Containers[0], memoryFSVolumeName)
	if !ok {
		t.Errorf("VolumeMount %q not added to container[0]", memoryFSVolumeName)
	}
	if mp != "/var/memory-agentfs" {
		t.Errorf("mountPath = %q, want /var/memory-agentfs", mp)
	}
}

func TestAttachMemoryFS_DefaultMountPath(t *testing.T) {
	pod := minimalPod("run-2")
	input := MemoryMountInput{
		AgentFS: &pure.AgentFSSpec{SizeGiB: 1},
		Mount:   &pure.MountSpec{Enabled: true}, // no explicit MountPath
	}

	AttachMemoryFS(pod, input)

	mp, _ := hasMount(pod.Spec.Containers[0], memoryFSVolumeName)
	if mp != defaultMemoryMountPath {
		t.Errorf("mountPath = %q, want %q", mp, defaultMemoryMountPath)
	}
}

func TestAttachMemoryFS_MountDisabled_NoOp(t *testing.T) {
	pod := minimalPod("run-3")
	input := MemoryMountInput{
		AgentFS: &pure.AgentFSSpec{SizeGiB: 1},
		Mount:   &pure.MountSpec{Enabled: false},
	}

	AttachMemoryFS(pod, input)

	if hasVolume(pod, memoryFSVolumeName) {
		t.Error("volume added even though Enabled=false")
	}
}

func TestAttachMemoryFS_NilMount_NoOp(t *testing.T) {
	pod := minimalPod("run-4")
	input := MemoryMountInput{AgentFS: &pure.AgentFSSpec{SizeGiB: 1}, Mount: nil}

	AttachMemoryFS(pod, input)

	if hasVolume(pod, memoryFSVolumeName) {
		t.Error("volume added with nil Mount")
	}
}

func TestAttachMemoryFS_NilAgentFS_NoOp(t *testing.T) {
	pod := minimalPod("run-5")
	input := MemoryMountInput{AgentFS: nil, Mount: &pure.MountSpec{Enabled: true}}

	AttachMemoryFS(pod, input)

	if hasVolume(pod, memoryFSVolumeName) {
		t.Error("volume added with nil AgentFS")
	}
}

func TestAttachMemoryFS_Idempotent(t *testing.T) {
	pod := minimalPod("run-6")
	input := enabledInput("/var/memory-agentfs")

	AttachMemoryFS(pod, input)
	AttachMemoryFS(pod, input) // second call must be a no-op

	count := 0
	for _, v := range pod.Spec.Volumes {
		if v.Name == memoryFSVolumeName {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected exactly 1 volume after double-attach, got %d", count)
	}

	mountCount := 0
	for _, vm := range pod.Spec.Containers[0].VolumeMounts {
		if vm.Name == memoryFSVolumeName {
			mountCount++
		}
	}
	if mountCount != 1 {
		t.Errorf("expected exactly 1 VolumeMount after double-attach, got %d", mountCount)
	}
}

func TestAttachMemoryFS_MultipleContainers(t *testing.T) {
	pod := minimalPod("run-7")
	pod.Spec.Containers = append(pod.Spec.Containers, corev1.Container{
		Name:  "sidecar",
		Image: "smol-agents/sidecar:0.1.0",
	})

	AttachMemoryFS(pod, enabledInput("/var/memory-agentfs"))

	for i, c := range pod.Spec.Containers {
		if _, ok := hasMount(c, memoryFSVolumeName); !ok {
			t.Errorf("container[%d] (%s) missing VolumeMount", i, c.Name)
		}
	}
}

func TestAttachMemoryFS_CoexistsWithStorageAgentFS(t *testing.T) {
	// Simulate a pod that already has the storage AgentFS volume (from agentrun.go).
	run := &amv1.AgentRun{}
	run.Name = "my-run"
	run.Namespace = "test"

	agent := &amv1.Agent{}
	agent.Name = "my-agent"
	agent.Namespace = "test"
	agent.Spec.Storage = &pure.StorageSpec{
		Kind:    pure.StorageAgentFS,
		AgentFS: &pure.AgentFSSpec{SizeGiB: 2, MountPath: "/var/agentfs"},
	}

	pod := BuildAgentRunPod(run, agent)

	// The pod should already have the "agentfs" volume from BuildAgentRunPod.
	if !hasVolume(pod, "agentfs") {
		t.Fatal("expected agentfs volume from BuildAgentRunPod")
	}

	// Now attach the memory FS mount.
	AttachMemoryFS(pod, enabledInput("/var/memory-agentfs"))

	// Both volumes must coexist.
	if !hasVolume(pod, "agentfs") {
		t.Error("storage agentfs volume removed after AttachMemoryFS")
	}
	if !hasVolume(pod, memoryFSVolumeName) {
		t.Error("memory-agentfs volume not added")
	}

	// The agent container must have both mounts.
	c := pod.Spec.Containers[0]
	if _, ok := hasMount(c, "agentfs"); !ok {
		t.Error("storage AgentFS VolumeMount lost")
	}
	if _, ok := hasMount(c, memoryFSVolumeName); !ok {
		t.Error("memory AgentFS VolumeMount missing")
	}
}

// ── AgentFSVolumeMount tests ─────────────────────────────────────────────────

func TestAgentFSVolumeMount_Enabled(t *testing.T) {
	input := enabledInput("/custom/path")
	vol, vm, ok := AgentFSVolumeMount(input)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if vol.Name != memoryFSVolumeName {
		t.Errorf("vol.Name = %q, want %q", vol.Name, memoryFSVolumeName)
	}
	if vm.MountPath != "/custom/path" {
		t.Errorf("vm.MountPath = %q, want /custom/path", vm.MountPath)
	}
	if vol.EmptyDir == nil {
		t.Error("expected EmptyDir volume source")
	}
}

func TestAgentFSVolumeMount_Disabled(t *testing.T) {
	input := MemoryMountInput{AgentFS: &pure.AgentFSSpec{SizeGiB: 1}, Mount: &pure.MountSpec{Enabled: false}}
	_, _, ok := AgentFSVolumeMount(input)
	if ok {
		t.Error("expected ok=false when mount disabled")
	}
}

// ── MountEnabled / MountPath helpers ─────────────────────────────────────────

func TestMemoryMountInput_MountEnabled(t *testing.T) {
	cases := []struct {
		name    string
		input   MemoryMountInput
		enabled bool
	}{
		{
			name:    "all set enabled",
			input:   enabledInput("/x"),
			enabled: true,
		},
		{
			name:    "nil mount",
			input:   MemoryMountInput{AgentFS: &pure.AgentFSSpec{SizeGiB: 1}},
			enabled: false,
		},
		{
			name:    "nil agentfs",
			input:   MemoryMountInput{Mount: &pure.MountSpec{Enabled: true}},
			enabled: false,
		},
		{
			name:    "disabled flag",
			input:   MemoryMountInput{AgentFS: &pure.AgentFSSpec{SizeGiB: 1}, Mount: &pure.MountSpec{Enabled: false}},
			enabled: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.input.MountEnabled() != tc.enabled {
				t.Errorf("MountEnabled() = %v, want %v", tc.input.MountEnabled(), tc.enabled)
			}
		})
	}
}

func TestMemoryMountInput_MountPath_Default(t *testing.T) {
	input := MemoryMountInput{
		AgentFS: &pure.AgentFSSpec{SizeGiB: 1},
		Mount:   &pure.MountSpec{Enabled: true},
	}
	if input.MountPath() != defaultMemoryMountPath {
		t.Errorf("MountPath() = %q, want %q", input.MountPath(), defaultMemoryMountPath)
	}
}

func TestMemoryMountInput_MountPath_Override(t *testing.T) {
	input := MemoryMountInput{
		AgentFS: &pure.AgentFSSpec{SizeGiB: 1},
		Mount:   &pure.MountSpec{Enabled: true, MountPath: "/custom"},
	}
	if input.MountPath() != "/custom" {
		t.Errorf("MountPath() = %q, want /custom", input.MountPath())
	}
}
