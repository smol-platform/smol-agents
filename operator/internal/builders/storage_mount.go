// Package builders — storage_mount.go
//
// AttachStorageFS wires an Agent's durable AgentFS storage (Agent.Spec.Storage,
// kind=agentfs) into its AgentRun pod: the SQLite-canonical volume, an init
// container that restores it from S3, and a sidecar that serves the AgentFS API
// and streams WAL frames + snapshots back to S3.
//
// This closes the gap where BuildAgentRunPod previously created only a bare
// EmptyDir for storage and ignored AgentFSSpec.Image/Backup/Restore — i.e. the
// agent's "files" were ephemeral scratch with no durability. The shape mirrors
// AttachMemoryFS (memory_mount.go); the difference is that this path consumes
// the BackupPolicy/RestorePolicy and passes them to the sidecar via env, and
// uses the volume name "agentfs" (distinct from "memory-agentfs") so an Agent
// can have both a durable storage FS and a memory FS in the same pod.
//
// S3 destination + crypto are non-secret and travel as env; AWS credentials
// (BackupPolicy.S3.CredentialsRef) are projected from the referenced k8s Secret
// via secretKeyRef so they never sit in the pod spec.
package builders

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"

	pure "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

const (
	// storageFSVolumeName is the volume for Agent.Spec.Storage AgentFS.
	storageFSVolumeName = "agentfs"

	// defaultStorageMountPath is the fallback when AgentFSSpec.MountPath is empty.
	defaultStorageMountPath = "/var/agentfs"

	// storageFSSidecarName / storageFSInitName are the storage AgentFS containers.
	storageFSSidecarName = "agentfs-sidecar"
	storageFSInitName    = "agentfs-init"
)

// StorageMountInput carries the resolved AgentFS storage spec for a pod.
type StorageMountInput struct {
	AgentFS *pure.AgentFSSpec
}

// storageEnabled reports whether the agent declares durable AgentFS storage.
func storageMountFor(agent *pure.AgentSpec) (StorageMountInput, bool) {
	if agent == nil || agent.Storage == nil ||
		agent.Storage.Kind != pure.StorageAgentFS || agent.Storage.AgentFS == nil {
		return StorageMountInput{}, false
	}
	return StorageMountInput{AgentFS: agent.Storage.AgentFS}, true
}

func (s StorageMountInput) mountPath() string {
	if s.AgentFS != nil && s.AgentFS.MountPath != "" {
		return s.AgentFS.MountPath
	}
	return defaultStorageMountPath
}

func (s StorageMountInput) image() string {
	if s.AgentFS != nil && s.AgentFS.Image != "" {
		return s.AgentFS.Image
	}
	return defaultAgentFSSidecarImage // shared with memory_mount.go
}

// AttachStorageFS appends the durable AgentFS volume, restore init container,
// and serving sidecar to pod, mounting the volume into every pre-existing
// container. No-op when the agent has no AgentFS storage. Idempotent on the
// "agentfs" volume name.
func AttachStorageFS(pod *corev1.Pod, input StorageMountInput) *corev1.Pod {
	if input.AgentFS == nil {
		return pod
	}
	for _, v := range pod.Spec.Volumes {
		if v.Name == storageFSVolumeName {
			return pod // already wired
		}
	}
	mp := input.mountPath()

	// Volume: EmptyDir bounded by SizeGiB; the sidecar owns durability via S3.
	sizeLimit := volumeSizeLimit(input.AgentFS)
	emptyDir := &corev1.EmptyDirVolumeSource{}
	if sizeLimit != nil {
		emptyDir.SizeLimit = sizeLimit
	}
	pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
		Name:         storageFSVolumeName,
		VolumeSource: corev1.VolumeSource{EmptyDir: emptyDir},
	})

	backupEnv := agentFSBackupEnv(input.AgentFS)
	pod.Spec.InitContainers = append(pod.Spec.InitContainers,
		agentFSContainer(storageFSInitName, input.image(), "init", mp, append(backupEnv, restoreEnv(input.AgentFS.Restore)...)))
	pod.Spec.Containers = append(pod.Spec.Containers,
		agentFSContainer(storageFSSidecarName, input.image(), "serve", mp, backupEnv))

	// Mount into every pre-existing container (the harness/loop), not the
	// sidecar we just appended.
	vm := corev1.VolumeMount{Name: storageFSVolumeName, MountPath: mp}
	lastIdx := len(pod.Spec.Containers) - 1
	for i := range pod.Spec.Containers[:lastIdx] {
		if !hasVolumeMount(pod.Spec.Containers[i].VolumeMounts, storageFSVolumeName) {
			pod.Spec.Containers[i].VolumeMounts = append(pod.Spec.Containers[i].VolumeMounts, vm)
		}
	}
	return pod
}

// agentFSContainer builds an init/sidecar container for the AgentFS sidecar
// image. verb is "init" (restore from S3, then exit) or "serve" (gRPC API +
// WAL/snapshot upload).
func agentFSContainer(name, image, verb, mountPath string, env []corev1.EnvVar) corev1.Container {
	return corev1.Container{
		Name:            name,
		Image:           image,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Args:            []string{verb, "--mount=" + mountPath},
		Env:             env,
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: ptr.To(false),
			ReadOnlyRootFilesystem:   ptr.To(false), // writes to the mounted volume
			RunAsNonRoot:             ptr.To(true),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		},
		VolumeMounts: []corev1.VolumeMount{{Name: storageFSVolumeName, MountPath: mountPath}},
	}
}

// agentFSBackupEnv renders the S3 destination + crypto + cadence as env, plus
// AWS credentials projected from BackupPolicy.S3.CredentialsRef (never inlined).
func agentFSBackupEnv(a *pure.AgentFSSpec) []corev1.EnvVar {
	if a == nil || a.Backup == nil || a.Backup.S3 == nil {
		return nil
	}
	b, s3 := a.Backup, a.Backup.S3
	env := []corev1.EnvVar{
		{Name: "AGENTFS_S3_BUCKET", Value: s3.Bucket},
		{Name: "AGENTFS_S3_PREFIX", Value: s3.Prefix},
		{Name: "AGENTFS_S3_REGION", Value: s3.Region},
	}
	env = appendIf(env, "AGENTFS_S3_ENDPOINT", s3.EndpointURL)
	env = appendIf(env, "AGENTFS_S3_SSE", s3.SSEAlgorithm)
	env = appendIf(env, "AGENTFS_S3_KMS_KEY_ARN", s3.KMSKeyARN)
	env = appendIf(env, "AGENTFS_BACKUP_SCHEDULE", b.Schedule)
	env = appendIf(env, "AGENTFS_WAL_INTERVAL", b.WALSnapshotInterval)
	env = appendIf(env, "AGENTFS_RETENTION_MIN_AGE", b.Retention.MinAge)
	if b.Retention.MaxVersions > 0 {
		env = append(env, corev1.EnvVar{Name: "AGENTFS_RETENTION_MAX_VERSIONS", Value: itoa(b.Retention.MaxVersions)})
	}
	// AWS credentials from the referenced k8s Secret (never inlined in the spec).
	if s3.CredentialsRef != nil && s3.CredentialsRef.SecretName != "" {
		sn := s3.CredentialsRef.SecretName
		env = append(env,
			awsCredEnv("AWS_ACCESS_KEY_ID", sn, "access-key-id", false),
			awsCredEnv("AWS_SECRET_ACCESS_KEY", sn, "secret-access-key", false),
			awsCredEnv("AWS_SESSION_TOKEN", sn, "session-token", true),
		)
	}
	return env
}

// restoreEnv renders the RestorePolicy as env consumed only by the init container.
func restoreEnv(r *pure.RestorePolicy) []corev1.EnvVar {
	if r == nil {
		return nil
	}
	var env []corev1.EnvVar
	env = appendIf(env, "AGENTFS_RESTORE_MODE", r.Mode)
	env = appendIf(env, "AGENTFS_RESTORE_VERSION_ID", r.VersionID)
	env = appendIf(env, "AGENTFS_RESTORE_POINT_IN_TIME", r.PointInTime)
	env = appendIf(env, "AGENTFS_RESTORE_IF_MISSING", r.IfMissing)
	return env
}

func appendIf(env []corev1.EnvVar, name, value string) []corev1.EnvVar {
	if value == "" {
		return env
	}
	return append(env, corev1.EnvVar{Name: name, Value: value})
}

// awsCredEnv projects a key from a k8s Secret into an env var. optional=true
// tolerates the key being absent (e.g. session-token).
func awsCredEnv(envName, secretName, key string, optional bool) corev1.EnvVar {
	return corev1.EnvVar{
		Name: envName,
		ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
				Key:                  key,
				Optional:             ptr.To(optional),
			},
		},
	}
}

func itoa(n int32) string {
	// small, no fmt import churn
	if n == 0 {
		return "0"
	}
	neg := n < 0
	var buf [12]byte
	i := len(buf)
	x := int64(n)
	if neg {
		x = -x
	}
	for x > 0 {
		i--
		buf[i] = byte('0' + x%10)
		x /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
