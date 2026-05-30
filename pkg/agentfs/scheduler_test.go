package agentfs

import (
	"context"
	"testing"
	"time"
)

// On shutdown (ctx cancelled, i.e. SIGTERM) the scheduler takes one final
// backup when BackupOnShutdown is set, so a short run's work isn't lost between
// ticks. Manager.Backup must still succeed even though the signal ctx is gone
// (the S3 client uses its own context).
func TestScheduler_BackupOnShutdown(t *testing.T) {
	m, _, s3 := mkManager(t)
	sched := &Scheduler{
		Manager:               m,
		FullInterval:          0, // no periodic backup; only the shutdown one
		BackupOnShutdown:      true,
		ShutdownBackupTimeout: 5 * time.Second,
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // simulate SIGTERM before any tick

	_ = sched.Run(ctx)

	versions, _ := s3.ListVersions(m.keyForFull())
	if len(versions) != 1 {
		t.Errorf("BackupOnShutdown: got %d backups, want exactly 1 final backup on shutdown", len(versions))
	}
}

// Without BackupOnShutdown (the default), a cancelled ctx just stops — no
// surprise upload.
func TestScheduler_NoShutdownBackupByDefault(t *testing.T) {
	m, _, s3 := mkManager(t)
	sched := &Scheduler{Manager: m, FullInterval: 0}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_ = sched.Run(ctx)

	versions, _ := s3.ListVersions(m.keyForFull())
	if len(versions) != 0 {
		t.Errorf("default: got %d backups, want 0 (no shutdown backup unless opted in)", len(versions))
	}
}
