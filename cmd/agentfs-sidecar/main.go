// Command agentfs-sidecar is the AgentFS storage sidecar/init container.
//
//	agentfs-sidecar init  --mount <dir>   # restore the tree from S3 (or fresh)
//	agentfs-sidecar serve --mount <dir>   # periodic full backups to S3, plus one
//	                                      # final backup on SIGTERM, then exit
//
// It wraps pkg/agentfs (Manager + AWSS3 + Scheduler) with FilesystemStorage,
// reading its S3 destination / restore policy from the AGENTFS_* env the
// operator injects (see operator/internal/builders/storage_mount.go). AWS
// credentials come from the standard SDK chain (the operator projects them as
// AWS_* env). Backups are full snapshots (gzipped tar of the mount); SQLite WAL
// streaming is not implemented here.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/smol-platform/smol-agents/internal/version"
	"github.com/smol-platform/smol-agents/pkg/agentfs"
	v1 "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
	"github.com/smol-platform/smol-agents/pkg/observability"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "--version" {
		os.Stdout.WriteString(version.String() + "\n")
		return
	}
	if len(os.Args) < 2 {
		fail("usage: agentfs-sidecar <init|serve> --mount <dir>")
	}
	verb := os.Args[1]

	fs := flag.NewFlagSet(verb, flag.ExitOnError)
	mount := fs.String("mount", "/var/agentfs", "AgentFS mount directory")
	logLevel := fs.String("log-level", "info", "debug|info|warn|error")
	_ = fs.Parse(os.Args[2:])

	level := slog.LevelInfo
	_ = level.UnmarshalText([]byte(*logLevel))
	logger := observability.MustLogger(level)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	mgr, err := buildManager(ctx, *mount)
	if err != nil {
		logger.Error("agentfs-sidecar init", "err", err)
		os.Exit(2)
	}

	switch verb {
	case "init":
		ver, err := mgr.Restore()
		if err != nil {
			logger.Error("restore", "err", err)
			os.Exit(1)
		}
		logger.Info("restore complete", "versionID", ver.ID, "key", ver.Key, "mount", *mount)
	case "serve":
		sched := &agentfs.Scheduler{
			Manager:      mgr,
			FullInterval: scheduleInterval(os.Getenv("AGENTFS_BACKUP_SCHEDULE")),
			WALInterval:  0, // FilesystemStorage does full snapshots only
			// One final backup on SIGTERM (when an S3 destination is configured) so
			// a short run's work isn't lost between ticks. The native-sidecar grace
			// period (set by the operator) gives this time before SIGKILL.
			BackupOnShutdown: os.Getenv("AGENTFS_S3_BUCKET") != "",
			Logger:           logger,
		}
		logger.Info("serving AgentFS backups", "mount", *mount,
			"fullInterval", sched.FullInterval, "backupOnShutdown", sched.BackupOnShutdown)
		if err := sched.Run(ctx); err != nil && ctx.Err() == nil {
			logger.Error("scheduler", "err", err)
			os.Exit(1)
		}
		// After the final backup, publish declared artifacts (M2.25). Never
		// fatal — the run already completed; artifacts are observability-only.
		if m, ok := collectArtifactsOnShutdown(*mount, mgr.S3); ok {
			b, _ := json.Marshal(m)
			logger.Info("artifacts collected", "state", m.State, "count", len(m.Refs), "manifest", string(b))
			// Record the manifest where the operator folds it into AgentRun.status
			// (M2.26) — the sidecar's termination message, read from pod status.
			writeArtifactTerminationMessage(m)
		}
	default:
		fail("unknown verb %q (init|serve)", verb)
	}
}

// buildManager reconstructs the AgentFSSpec + S3 client from the AGENTFS_* env.
func buildManager(ctx context.Context, mount string) (*agentfs.Manager, error) {
	bucket := os.Getenv("AGENTFS_S3_BUCKET")
	spec := v1.AgentFSSpec{
		MountPath: mount,
		Backup: &v1.BackupPolicy{
			S3: &v1.S3BackupSpec{
				Bucket:       bucket,
				Prefix:       os.Getenv("AGENTFS_S3_PREFIX"),
				Region:       os.Getenv("AGENTFS_S3_REGION"),
				EndpointURL:  os.Getenv("AGENTFS_S3_ENDPOINT"),
				SSEAlgorithm: os.Getenv("AGENTFS_S3_SSE"),
				KMSKeyARN:    os.Getenv("AGENTFS_S3_KMS_KEY_ARN"),
				Versioning:   true,
			},
			Schedule:            os.Getenv("AGENTFS_BACKUP_SCHEDULE"),
			WALSnapshotInterval: os.Getenv("AGENTFS_WAL_INTERVAL"),
			Retention: v1.RetentionPolicy{
				MaxVersions: int32(atoiDefault(os.Getenv("AGENTFS_RETENTION_MAX_VERSIONS"), 0)),
				MinAge:      os.Getenv("AGENTFS_RETENTION_MIN_AGE"),
			},
		},
		Restore: &v1.RestorePolicy{
			Mode:        os.Getenv("AGENTFS_RESTORE_MODE"),
			VersionID:   os.Getenv("AGENTFS_RESTORE_VERSION_ID"),
			PointInTime: os.Getenv("AGENTFS_RESTORE_POINT_IN_TIME"),
			IfMissing:   os.Getenv("AGENTFS_RESTORE_IF_MISSING"),
		},
	}

	s3, err := agentfs.NewAWSS3(ctx, agentfs.AWSS3Config{
		Bucket:         bucket,
		Region:         spec.Backup.S3.Region,
		Endpoint:       spec.Backup.S3.EndpointURL,
		ForcePathStyle: spec.Backup.S3.EndpointURL != "", // path-style for MinIO/LocalStack
		SSEAlgorithm:   spec.Backup.S3.SSEAlgorithm,
		KMSKeyARN:      spec.Backup.S3.KMSKeyARN,
	})
	if err != nil {
		return nil, err
	}
	mgr := &agentfs.Manager{
		Spec:    spec,
		Storage: agentfs.FilesystemStorage{Root: mount},
		S3:      s3,
		Now:     time.Now,
	}
	// kopia backend: content-addressed snapshots (dedup, history, diff, rollback)
	// supersede the tar path. AWS creds + the repo password come from env the
	// operator projects from the CredentialsRef secret (never inlined). The
	// existing Scheduler then drives periodic snapshots (AGENTFS_BACKUP_SCHEDULE)
	// plus the final one on SIGTERM.
	if os.Getenv("AGENTFS_BACKEND") == "kopia" {
		mgr.Backend = &agentfs.KopiaStore{
			Bucket:          bucket,
			Prefix:          spec.Backup.S3.Prefix,
			Region:          spec.Backup.S3.Region,
			Endpoint:        spec.Backup.S3.EndpointURL,
			AccessKeyID:     os.Getenv("AWS_ACCESS_KEY_ID"),
			SecretAccessKey: os.Getenv("AWS_SECRET_ACCESS_KEY"),
			SessionToken:    os.Getenv("AWS_SESSION_TOKEN"),
			Password:        os.Getenv("AGENTFS_KOPIA_PASSWORD"),
		}
	}
	return mgr, nil
}

// scheduleInterval maps a backup schedule string to a poll interval. Accepts
// cron shorthands (@hourly/@daily/@weekly), "@every <dur>", or a Go duration;
// defaults to 1h.
func scheduleInterval(s string) time.Duration {
	switch strings.TrimSpace(s) {
	case "", "@hourly":
		return time.Hour
	case "@daily", "@midnight":
		return 24 * time.Hour
	case "@weekly":
		return 7 * 24 * time.Hour
	}
	if rest, ok := strings.CutPrefix(s, "@every "); ok {
		s = strings.TrimSpace(rest)
	}
	if d, err := time.ParseDuration(s); err == nil && d > 0 {
		return d
	}
	return time.Hour
}

func atoiDefault(s string, def int) int {
	if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
		return n
	}
	return def
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, strings.TrimRight(format, "\n")+"\n", args...)
	os.Exit(2)
}
