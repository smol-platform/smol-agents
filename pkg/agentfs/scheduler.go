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

// disabledTicker returns a channel that never fires.
func disabledTicker() <-chan time.Time {
	return make(chan time.Time)
}
