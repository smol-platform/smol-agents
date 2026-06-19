package v1

import (
	"strings"
	"testing"
)

func TestValidateStorage_Nil_OK(t *testing.T) {
	if err := ValidateStorage(nil); err != nil {
		t.Errorf("nil storage rejected: %v", err)
	}
}

func TestValidateStorage_KindNone_OK(t *testing.T) {
	if err := ValidateStorage(&StorageSpec{Kind: StorageNone}); err != nil {
		t.Errorf("kind=none rejected: %v", err)
	}
}

func TestValidateStorage_AgentFSRequiresSpec(t *testing.T) {
	err := ValidateStorage(&StorageSpec{Kind: StorageAgentFS})
	if err == nil || !strings.Contains(err.Error(), "agentfs is required") {
		t.Errorf("expected error: %v", err)
	}
}

func TestValidateStorage_AgentFSHappyPath(t *testing.T) {
	s := &StorageSpec{
		Kind: StorageAgentFS,
		AgentFS: &AgentFSSpec{
			SizeGiB:   5,
			MountPath: "/var/agentfs",
			Backup: &BackupPolicy{
				S3: &S3BackupSpec{
					Bucket:       "agents-state",
					Region:       "us-east-1",
					SSEAlgorithm: "AES256",
					Versioning:   true,
				},
				Schedule:            "@hourly",
				WALSnapshotInterval: "30s",
				Retention:           RetentionPolicy{MaxVersions: 24, MinAge: "24h"},
			},
		},
	}
	if err := ValidateStorage(s); err != nil {
		t.Errorf("happy: %v", err)
	}
}

func TestValidateStorage_KopiaEphemeralOK(t *testing.T) {
	// backend=kopia with NO backup.s3 is the ephemeral (in-pod repo) backend —
	// valid, so the default can later flip to kopia without breaking no-S3 agents.
	s := &StorageSpec{Kind: StorageAgentFS, AgentFS: &AgentFSSpec{
		SizeGiB: 1, MountPath: "/var/agentfs", Backend: "kopia",
	}}
	if err := ValidateStorage(s); err != nil {
		t.Errorf("ephemeral kopia (no s3) should be valid: %v", err)
	}
}

func TestValidateStorage_KopiaWithS3OK(t *testing.T) {
	s := &StorageSpec{Kind: StorageAgentFS, AgentFS: &AgentFSSpec{
		SizeGiB: 1, Backend: "kopia",
		Backup: &BackupPolicy{S3: &S3BackupSpec{Bucket: "b", Region: "us-east-1"}},
	}}
	if err := ValidateStorage(s); err != nil {
		t.Errorf("durable kopia (with s3) should be valid: %v", err)
	}
}

func TestValidateStorage_AgentFSRequiresPositiveSize(t *testing.T) {
	s := &StorageSpec{Kind: StorageAgentFS, AgentFS: &AgentFSSpec{SizeGiB: 0}}
	if err := ValidateStorage(s); err == nil {
		t.Error("expected size error")
	}
}

func TestValidateStorage_MountPathMustBeAbsolute(t *testing.T) {
	s := &StorageSpec{Kind: StorageAgentFS, AgentFS: &AgentFSSpec{
		SizeGiB: 1, MountPath: "relative/path",
	}}
	if err := ValidateStorage(s); err == nil {
		t.Error("expected absolute path error")
	}
}

func TestValidateStorage_KMSRequiresKey(t *testing.T) {
	s := &StorageSpec{Kind: StorageAgentFS, AgentFS: &AgentFSSpec{
		SizeGiB: 1,
		Backup: &BackupPolicy{S3: &S3BackupSpec{
			Bucket: "b", SSEAlgorithm: "aws:kms",
		}},
	}}
	if err := ValidateStorage(s); err == nil {
		t.Error("expected kmsKeyARN error")
	}
}

func TestValidateStorage_BackupRequiresBucket(t *testing.T) {
	s := &StorageSpec{Kind: StorageAgentFS, AgentFS: &AgentFSSpec{
		SizeGiB: 1,
		Backup:  &BackupPolicy{S3: &S3BackupSpec{}},
	}}
	if err := ValidateStorage(s); err == nil {
		t.Error("expected bucket error")
	}
}

func TestValidateStorage_DurationsParsed(t *testing.T) {
	s := &StorageSpec{Kind: StorageAgentFS, AgentFS: &AgentFSSpec{
		SizeGiB: 1,
		Backup: &BackupPolicy{
			S3:                  &S3BackupSpec{Bucket: "b"},
			WALSnapshotInterval: "not a duration",
		},
	}}
	if err := ValidateStorage(s); err == nil {
		t.Error("expected duration parse error")
	}
}

func TestValidateStorage_UnknownKind(t *testing.T) {
	s := &StorageSpec{Kind: "memcached"}
	if err := ValidateStorage(s); err == nil {
		t.Error("expected unknown-kind error")
	}
}

// u9k.5: an unset backend defaults to kopia; tar is honored when explicit.
func TestAgentFSSpec_EffectiveBackend(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"", "kopia"}, {"kopia", "kopia"}, {"tar", "tar"},
	} {
		if got := (AgentFSSpec{Backend: tc.in}).EffectiveBackend(); got != tc.want {
			t.Errorf("EffectiveBackend(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
