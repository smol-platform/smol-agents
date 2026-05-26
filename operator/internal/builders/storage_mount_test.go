package builders

import (
	"testing"

	corev1 "k8s.io/api/core/v1"

	amv1 "github.com/smol-platform/smol-agents/operator/api/agentmodel/v1"
	pure "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

func storageAgent() *amv1.Agent {
	a := &amv1.Agent{}
	a.Name = "a1"
	a.Namespace = "tenant-a"
	a.Spec.Mode = pure.ModeHarness
	a.Spec.Harness = &pure.HarnessSpec{Kind: pure.HarnessHermes, HTTP: &pure.HarnessHTTPSpec{URL: "http://gw"}}
	a.Spec.Storage = &pure.StorageSpec{
		Kind: pure.StorageAgentFS,
		AgentFS: &pure.AgentFSSpec{
			SizeGiB:   5,
			MountPath: "/var/agentfs",
			Backup: &pure.BackupPolicy{
				S3: &pure.S3BackupSpec{
					Bucket: "b", Prefix: "p/", Region: "us-east-2",
					SSEAlgorithm: "aws:kms", KMSKeyARN: "arn:kms",
					CredentialsRef: &pure.AuthRef{SecretName: "aws-creds"},
				},
				Schedule: "@hourly", WALSnapshotInterval: "30s",
				Retention: pure.RetentionPolicy{MaxVersions: 24, MinAge: "1h"},
			},
			Restore: &pure.RestorePolicy{Mode: "latest", IfMissing: "fresh"},
		},
	}
	return a
}

func findCtr(pod *corev1.Pod, name string) *corev1.Container {
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == name {
			return &pod.Spec.Containers[i]
		}
	}
	for i := range pod.Spec.InitContainers {
		if pod.Spec.InitContainers[i].Name == name {
			return &pod.Spec.InitContainers[i]
		}
	}
	return nil
}

func envVal(c *corev1.Container, name string) string {
	for _, e := range c.Env {
		if e.Name == name {
			return e.Value
		}
	}
	return ""
}

func secretEnvRef(c *corev1.Container, name string) (secret, key string, ok bool) {
	for _, e := range c.Env {
		if e.Name == name && e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil {
			return e.ValueFrom.SecretKeyRef.Name, e.ValueFrom.SecretKeyRef.Key, true
		}
	}
	return "", "", false
}

func TestBuildAgentRunPod_StorageAgentFS(t *testing.T) {
	run := &amv1.AgentRun{}
	run.Name = "r1"
	run.Namespace = "tenant-a"

	pod := BuildAgentRunPod(run, storageAgent())

	// Volume + init + serving sidecar are all present (no longer an EmptyDir stub).
	if !hasVolume(pod, storageFSVolumeName) {
		t.Fatal("agentfs volume missing")
	}
	if !hasInitContainer(pod, storageFSInitName) {
		t.Error("agentfs-init container missing (no restore wiring)")
	}
	if !hasSidecar(pod, storageFSSidecarName) {
		t.Error("agentfs-sidecar container missing (no backup wiring)")
	}
	// The execution container (index 0) gets the mount.
	if _, ok := hasMount(pod.Spec.Containers[0], storageFSVolumeName); !ok {
		t.Error("main container missing agentfs mount")
	}

	// Sidecar carries the S3 backup config from the BackupPolicy.
	sc := findCtr(pod, storageFSSidecarName)
	for k, want := range map[string]string{
		"AGENTFS_S3_BUCKET":              "b",
		"AGENTFS_S3_PREFIX":              "p/",
		"AGENTFS_S3_REGION":              "us-east-2",
		"AGENTFS_S3_SSE":                 "aws:kms",
		"AGENTFS_S3_KMS_KEY_ARN":         "arn:kms",
		"AGENTFS_BACKUP_SCHEDULE":        "@hourly",
		"AGENTFS_WAL_INTERVAL":           "30s",
		"AGENTFS_RETENTION_MAX_VERSIONS": "24",
		"AGENTFS_RETENTION_MIN_AGE":      "1h",
	} {
		if got := envVal(sc, k); got != want {
			t.Errorf("sidecar env %s = %q, want %q", k, got, want)
		}
	}
	// AWS creds are projected from the secret, never inlined.
	if s, key, ok := secretEnvRef(sc, "AWS_ACCESS_KEY_ID"); !ok || s != "aws-creds" || key != "access-key-id" {
		t.Errorf("AWS_ACCESS_KEY_ID secretKeyRef = (%q,%q,%v), want (aws-creds,access-key-id,true)", s, key, ok)
	}

	// Restore policy reaches only the init container.
	ic := findCtr(pod, storageFSInitName)
	if got := envVal(ic, "AGENTFS_RESTORE_MODE"); got != "latest" {
		t.Errorf("init AGENTFS_RESTORE_MODE = %q, want latest", got)
	}
	if got := envVal(ic, "AGENTFS_RESTORE_IF_MISSING"); got != "fresh" {
		t.Errorf("init AGENTFS_RESTORE_IF_MISSING = %q, want fresh", got)
	}
	if envVal(sc, "AGENTFS_RESTORE_MODE") != "" {
		t.Error("restore env leaked onto the serving sidecar")
	}
}

func TestBuildAgentRunPod_NoStorage(t *testing.T) {
	run := &amv1.AgentRun{}
	run.Name = "r"
	run.Namespace = "t"
	agent := &amv1.Agent{}
	agent.Name = "a"
	agent.Namespace = "t"
	agent.Spec.Mode = pure.ModeHarness
	agent.Spec.Harness = &pure.HarnessSpec{Kind: pure.HarnessHermes, HTTP: &pure.HarnessHTTPSpec{URL: "http://gw"}}

	pod := BuildAgentRunPod(run, agent)
	if hasVolume(pod, storageFSVolumeName) {
		t.Error("no-storage pod should not carry an agentfs volume")
	}
	if hasSidecar(pod, storageFSSidecarName) {
		t.Error("no-storage pod should not carry an agentfs sidecar")
	}
	if len(pod.Spec.Containers) != 1 {
		t.Errorf("expected exactly 1 container, got %d", len(pod.Spec.Containers))
	}
}
