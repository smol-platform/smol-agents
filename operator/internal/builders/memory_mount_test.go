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

func hasInitContainer(pod *corev1.Pod, name string) bool {
	for _, c := range pod.Spec.InitContainers {
		if c.Name == name {
			return true
		}
	}
	return false
}

func hasSidecar(pod *corev1.Pod, name string) bool {
	for _, c := range pod.Spec.Containers {
		if c.Name == name {
			return true
		}
	}
	return false
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
		t.Errorf("VolumeMount %q not added to agent container", memoryFSVolumeName)
	}
	if mp != "/var/memory-agentfs" {
		t.Errorf("mountPath = %q, want /var/memory-agentfs", mp)
	}
}

// TestAttachMemoryFS_SizeLimit verifies the EmptyDir has a SizeLimit from SizeGiB.
func TestAttachMemoryFS_SizeLimit(t *testing.T) {
	pod := minimalPod("run-size")
	input := MemoryMountInput{
		AgentFS: &pure.AgentFSSpec{SizeGiB: 2},
		Mount:   &pure.MountSpec{Enabled: true, MountPath: "/var/memory-agentfs"},
	}

	AttachMemoryFS(pod, input)

	for _, v := range pod.Spec.Volumes {
		if v.Name == memoryFSVolumeName {
			if v.EmptyDir == nil {
				t.Fatal("volume source is not EmptyDir")
			}
			if v.EmptyDir.SizeLimit == nil {
				t.Fatal("EmptyDir SizeLimit is nil; expected 2Gi")
			}
			// 2 GiB = 2 * 2^30 bytes = 2147483648
			want := int64(2) << 30
			got, ok := v.EmptyDir.SizeLimit.AsInt64()
			if !ok || got != want {
				t.Errorf("SizeLimit = %v, want %d bytes (2Gi)", v.EmptyDir.SizeLimit, want)
			}
			return
		}
	}
	t.Error("memory-agentfs volume not found")
}

// TestAttachMemoryFS_NoSizeLimit verifies SizeLimit is absent when SizeGiB==0.
func TestAttachMemoryFS_NoSizeLimit(t *testing.T) {
	pod := minimalPod("run-nosize")
	input := MemoryMountInput{
		AgentFS: &pure.AgentFSSpec{SizeGiB: 0},
		Mount:   &pure.MountSpec{Enabled: true, MountPath: "/var/memory-agentfs"},
	}

	AttachMemoryFS(pod, input)

	for _, v := range pod.Spec.Volumes {
		if v.Name == memoryFSVolumeName {
			if v.EmptyDir == nil {
				t.Fatal("volume source is not EmptyDir")
			}
			if v.EmptyDir.SizeLimit != nil {
				t.Errorf("SizeLimit should be nil when SizeGiB==0, got %v", v.EmptyDir.SizeLimit)
			}
			return
		}
	}
	t.Error("memory-agentfs volume not found")
}

// TestAttachMemoryFS_AddsInitContainer verifies the AgentFS init container is added.
func TestAttachMemoryFS_AddsInitContainer(t *testing.T) {
	pod := minimalPod("run-init")
	input := enabledInput("/var/memory-agentfs")

	AttachMemoryFS(pod, input)

	if !hasInitContainer(pod, agentFSInitName) {
		t.Errorf("init container %q not added", agentFSInitName)
	}
	for _, c := range pod.Spec.InitContainers {
		if c.Name == agentFSInitName {
			_, ok := hasMount(c, memoryFSVolumeName)
			if !ok {
				t.Error("init container missing VolumeMount for memory-agentfs")
			}
		}
	}
}

// TestAttachMemoryFS_AddsSidecar verifies the AgentFS sidecar container is added.
func TestAttachMemoryFS_AddsSidecar(t *testing.T) {
	pod := minimalPod("run-sidecar")
	input := enabledInput("/var/memory-agentfs")

	AttachMemoryFS(pod, input)

	if !hasSidecar(pod, agentFSSidecarName) {
		t.Errorf("sidecar container %q not added", agentFSSidecarName)
	}
	for _, c := range pod.Spec.Containers {
		if c.Name == agentFSSidecarName {
			_, ok := hasMount(c, memoryFSVolumeName)
			if !ok {
				t.Error("sidecar container missing VolumeMount for memory-agentfs")
			}
		}
	}
}

// TestAttachMemoryFS_SidecarImage_Custom verifies the sidecar uses the spec image.
func TestAttachMemoryFS_SidecarImage_Custom(t *testing.T) {
	pod := minimalPod("run-img")
	input := MemoryMountInput{
		AgentFS: &pure.AgentFSSpec{SizeGiB: 1, Image: "myreg/agentfs:v2"},
		Mount:   &pure.MountSpec{Enabled: true, MountPath: "/var/memory-agentfs"},
	}

	AttachMemoryFS(pod, input)

	for _, c := range pod.Spec.Containers {
		if c.Name == agentFSSidecarName {
			if c.Image != "myreg/agentfs:v2" {
				t.Errorf("sidecar image = %q, want myreg/agentfs:v2", c.Image)
			}
			return
		}
	}
	t.Error("sidecar container not found")
}

// TestAttachMemoryFS_SidecarImage_Default verifies the default image is used when empty.
func TestAttachMemoryFS_SidecarImage_Default(t *testing.T) {
	pod := minimalPod("run-defimg")
	input := MemoryMountInput{
		AgentFS: &pure.AgentFSSpec{SizeGiB: 1, Image: ""},
		Mount:   &pure.MountSpec{Enabled: true, MountPath: "/var/memory-agentfs"},
	}

	AttachMemoryFS(pod, input)

	for _, c := range pod.Spec.Containers {
		if c.Name == agentFSSidecarName {
			if c.Image != defaultAgentFSSidecarImage {
				t.Errorf("sidecar image = %q, want %q", c.Image, defaultAgentFSSidecarImage)
			}
			return
		}
	}
	t.Error("sidecar container not found")
}

// TestAttachMemoryFS_AgentContainerMountedNotSidecar verifies the agent container
// gets the volume mount but the sidecar already has its own (not double-mounted).
func TestAttachMemoryFS_AgentContainerMountedNotSidecar(t *testing.T) {
	pod := minimalPod("run-mounts")
	input := enabledInput("/var/memory-agentfs")

	AttachMemoryFS(pod, input)

	// Agent container (index 0) must have the mount.
	agentContainer := pod.Spec.Containers[0]
	if agentContainer.Name != "agent" {
		t.Fatalf("expected agent container at index 0, got %q", agentContainer.Name)
	}
	if _, ok := hasMount(agentContainer, memoryFSVolumeName); !ok {
		t.Error("agent container missing memory-agentfs VolumeMount")
	}

	// Sidecar (last container) has exactly one mount for the volume.
	sidecar := pod.Spec.Containers[len(pod.Spec.Containers)-1]
	if sidecar.Name != agentFSSidecarName {
		t.Fatalf("expected sidecar at last index, got %q", sidecar.Name)
	}
	count := 0
	for _, vm := range sidecar.VolumeMounts {
		if vm.Name == memoryFSVolumeName {
			count++
		}
	}
	if count != 1 {
		t.Errorf("sidecar has %d mounts for memory-agentfs, want exactly 1", count)
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
	if hasInitContainer(pod, agentFSInitName) {
		t.Error("init container added even though Enabled=false")
	}
	if hasSidecar(pod, agentFSSidecarName) {
		t.Error("sidecar added even though Enabled=false")
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

	initCount := 0
	for _, c := range pod.Spec.InitContainers {
		if c.Name == agentFSInitName {
			initCount++
		}
	}
	if initCount != 1 {
		t.Errorf("expected exactly 1 init container after double-attach, got %d", initCount)
	}

	sidecarCount := 0
	for _, c := range pod.Spec.Containers {
		if c.Name == agentFSSidecarName {
			sidecarCount++
		}
	}
	if sidecarCount != 1 {
		t.Errorf("expected exactly 1 sidecar after double-attach, got %d", sidecarCount)
	}

	mountCount := 0
	for _, c := range pod.Spec.Containers {
		if c.Name == "agent" {
			for _, vm := range c.VolumeMounts {
				if vm.Name == memoryFSVolumeName {
					mountCount++
				}
			}
		}
	}
	if mountCount != 1 {
		t.Errorf("expected exactly 1 VolumeMount on agent container after double-attach, got %d", mountCount)
	}
}

func TestAttachMemoryFS_MultipleContainers(t *testing.T) {
	pod := minimalPod("run-7")
	pod.Spec.Containers = append(pod.Spec.Containers, corev1.Container{
		Name:  "observer",
		Image: "smol-agents/observer:0.1.0",
	})

	AttachMemoryFS(pod, enabledInput("/var/memory-agentfs"))

	// The agent and observer containers (pre-existing, not the sidecar) must all have the mount.
	preExisting := 2 // agent + observer
	for i, c := range pod.Spec.Containers[:preExisting] {
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

	// The original agent container must have both mounts.
	c := pod.Spec.Containers[0]
	if _, ok := hasMount(c, "agentfs"); !ok {
		t.Error("storage AgentFS VolumeMount lost")
	}
	if _, ok := hasMount(c, memoryFSVolumeName); !ok {
		t.Error("memory AgentFS VolumeMount missing")
	}

	// The memory init container must have been added.
	if !hasInitContainer(pod, agentFSInitName) {
		t.Error("memory-agentfs init container not added")
	}
	// The memory sidecar must have been added.
	if !hasSidecar(pod, agentFSSidecarName) {
		t.Error("memory-agentfs sidecar not added")
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
	// SizeLimit should reflect SizeGiB=1.
	if vol.EmptyDir.SizeLimit == nil {
		t.Error("EmptyDir SizeLimit should be set for SizeGiB=1")
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
