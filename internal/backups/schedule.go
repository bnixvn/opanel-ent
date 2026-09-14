package backups

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/bnixvn/opanel-ent/internal/db"
)

// Schedule frequencies.
const (
	Daily  = "daily"
	Weekly = "weekly"
)

// MaxKeep caps retention. Without a ceiling one customer's "keep 3650" fills
// the disk for everybody on the server.
const MaxKeep = 60

// ValidateSchedule checks a schedule and normalises what it can.
func ValidateSchedule(s *db.BackupSchedule) error {
	switch s.Frequency {
	case Daily, Weekly:
	default:
		return fmt.Errorf("frequency must be %q or %q", Daily, Weekly)
	}
	if s.Hour < 0 || s.Hour > 23 {
		return errors.New("hour must be between 0 and 23")
	}
	if s.Weekday < 0 || s.Weekday > 6 {
		return errors.New("weekday must be between 0 (Sunday) and 6")
	}
	if s.Keep < 1 || s.Keep > MaxKeep {
		return fmt.Errorf("keep must be between 1 and %d", MaxKeep)
	}
	if !s.IncludeFiles && !s.IncludeDatabases {
		return errors.New("a schedule must include the files, the databases, or both")
	}
	return nil
}

// Due reports whether a schedule should fire at now.
//
// The rule is "the hour has arrived and we have not run since the start of
// this period", rather than an exact timestamp: the sweep ticks every few
// minutes, the server may have been asleep, and a backup that is an hour late
// is infinitely better than one that was skipped because the tick missed its
// minute.
func Due(s *db.BackupSchedule, now time.Time) bool {
	if !s.Enabled {
		return false
	}
	if now.Hour() < s.Hour {
		return false
	}
	if s.Frequency == Weekly && int(now.Weekday()) != s.Weekday {
		return false
	}

	periodStart := time.Date(now.Year(), now.Month(), now.Day(), s.Hour, 0, 0, 0, now.Location())
	if s.Frequency == Weekly {
		// Same weekday, so the period began at this morning's hour; a run
		// any time since then counts.
		return s.LastRunAt.Before(periodStart)
	}
	return s.LastRunAt.Before(periodStart)
}

// RunDue takes every backup the schedules call for and prunes what retention
// no longer needs.
//
// One account's failure does not stop the others: a customer whose database
// is corrupt should not silently cost everyone else their nightly backup.
func (s *Service) RunDue(ctx context.Context, now time.Time) (started int, errs []error) {
	schedules, err := s.db.ListBackupSchedules(ctx)
	if err != nil {
		return 0, []error{err}
	}
	for _, sc := range schedules {
		if !Due(sc, now) {
			continue
		}
		// Stamped before the run, not after: a backup that fails must not be
		// retried on every tick for the rest of the day.
		if err := s.db.RecordScheduleRun(ctx, sc.OwnerID, now, ""); err != nil {
			errs = append(errs, err)
			continue
		}

		_, err := s.Start(ctx, Request{
			Owner:            Owner{ID: sc.OwnerID, Username: sc.OwnerUsername},
			IncludeFiles:     sc.IncludeFiles,
			IncludeDatabases: sc.IncludeDatabases,
			Kind:             db.BackupScheduled,
		})
		if err != nil {
			s.log.Error("backups: scheduled run failed to start",
				"owner", sc.OwnerUsername, "err", err)
			_ = s.db.RecordScheduleRun(ctx, sc.OwnerID, now, err.Error())
			errs = append(errs, fmt.Errorf("%s: %w", sc.OwnerUsername, err))
			continue
		}
		s.log.Info("backups: scheduled run started", "owner", sc.OwnerUsername)
		started++
	}
	return started, errs
}

// Prune removes scheduled archives beyond each account's retention.
//
// Run separately from RunDue and after it, because a backup started a moment
// ago is still running: pruning to "keep 7" while the eighth is being written
// would delete the oldest before the newest exists.
func (s *Service) Prune(ctx context.Context) (removed int, errs []error) {
	schedules, err := s.db.ListBackupSchedules(ctx)
	if err != nil {
		return 0, []error{err}
	}
	for _, sc := range schedules {
		old, err := s.db.PrunableBackups(ctx, sc.OwnerID, sc.Keep)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, b := range old {
			if err := s.Delete(ctx, b); err != nil {
				s.log.Warn("backups: cannot prune", "file", b.Filename, "err", err)
				errs = append(errs, err)
				continue
			}
			s.log.Info("backups: pruned", "owner", sc.OwnerUsername, "file", b.Filename)
			removed++
		}
	}
	return removed, errs
}
