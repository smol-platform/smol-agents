package agentfs

import (
	"context"
	"log/slog"
	"time"
)

// Scheduler runs full backups + WAL snapshots on a cadence and enforces
// retention. It is started by the agent runtime once per Pod.
type Scheduler struct {
	Manager      *Manager
	WALInterval  time.Duration // 0 disables WAL uploads
	FullInterval time.Duration // 0 disables full backups
	Logger       *slog.Logger

	// BackupOnShutdown runs one final full backup when ctx is cancelled (SIGTERM),
	// before Run returns — so a short-lived run's work is captured even if it
	// never reached a full-backup tick. The pod's terminationGracePeriodSeconds
	// must allow time for it (the operator sets a floor when a durable AgentFS
	// sidecar is attached).
	BackupOnShutdown bool

	// ShutdownBackupTimeout bounds the final backup so it can't outrun the pod's
	// termination grace. Defaults to 90s.
	ShutdownBackupTimeout time.Duration
}

// Run blocks until ctx is cancelled. Two tickers are driven in
// lock-step inside the same goroutine to keep ordering deterministic
// (a full snapshot before a WAL upload that follows immediately).
func (s *Scheduler) Run(ctx context.Context) error {
	if s.Logger == nil {
		s.Logger = slog.Default()
	}
	walT := disabledTicker()
	if s.WALInterval > 0 {
		t := time.NewTicker(s.WALInterval)
		defer t.Stop()
		walT = t.C
	}
	fullT := disabledTicker()
	if s.FullInterval > 0 {
		t := time.NewTicker(s.FullInterval)
		defer t.Stop()
		fullT = t.C
	}
	for {
		select {
		case <-ctx.Done():
			if s.BackupOnShutdown {
				s.finalBackup()
			}
			return ctx.Err()
		case <-fullT:
			if v, err := s.Manager.Backup(); err != nil {
				s.Logger.Warn("full backup failed", "err", err)
			} else {
				s.Logger.Info("full backup uploaded", "version", v.ID, "size", v.SizeBytes)
				if n, err := s.Manager.EnforceRetention(); err == nil && n > 0 {
					s.Logger.Info("retention enforced", "deleted", n)
				}
			}
		case <-walT:
			if _, ok, err := s.Manager.SnapshotWAL(); err != nil {
				s.Logger.Warn("wal snapshot failed", "err", err)
			} else if ok {
				s.Logger.Debug("wal snapshot uploaded")
			}
		}
	}
}

// finalBackup runs one full backup on shutdown (ctx is already cancelled),
// bounded by shutdownBackupTimeout so it can't outrun the pod's termination
// grace. Manager.Backup uses its own context internally (the S3 client does not
// capture the cancelled signal ctx), so the upload still proceeds after SIGTERM.
func (s *Scheduler) finalBackup() {
	type result struct {
		v   Version
		err error
	}
	ch := make(chan result, 1)
	go func() {
		v, err := s.Manager.Backup()
		ch <- result{v, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			s.Logger.Warn("final backup on shutdown failed", "err", r.err)
			return
		}
		s.Logger.Info("final backup on shutdown uploaded", "version", r.v.ID, "size", r.v.SizeBytes)
	case <-time.After(s.shutdownBackupTimeout()):
		s.Logger.Warn("final backup on shutdown timed out; relying on the periodic backup",
			"timeout", s.shutdownBackupTimeout())
	}
}

func (s *Scheduler) shutdownBackupTimeout() time.Duration {
	if s.ShutdownBackupTimeout > 0 {
		return s.ShutdownBackupTimeout
	}
	return 90 * time.Second
}

// disabledTicker returns a channel that never fires.
func disabledTicker() <-chan time.Time {
	return make(chan time.Time)
}
