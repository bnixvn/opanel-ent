package backups

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/bnixvn/opanel-ent/internal/backuparchive"
	"github.com/bnixvn/opanel-ent/internal/db"
)

func at(day int, hour int) time.Time {
	// 2026-09-14 is a Monday, so day 14 is weekday 1.
	return time.Date(2026, 9, day, hour, 30, 0, 0, time.UTC)
}

func TestDue(t *testing.T) {
	daily := func(lastRun time.Time) *db.BackupSchedule {
		return &db.BackupSchedule{Enabled: true, Frequency: Daily, Hour: 3, Keep: 7,
			LastRunAt: lastRun, IncludeFiles: true}
	}

	cases := []struct {
		name string
		sc   *db.BackupSchedule
		now  time.Time
		want bool
	}{
		{"before the hour", daily(at(13, 3)), at(14, 2), false},
		{"at the hour, not yet run today", daily(at(13, 3)), at(14, 3), true},
		{"already ran today", daily(at(14, 3)), at(14, 4), false},
		// The case that matters: the server was off at 3am and came back at
		// 9. A schedule that only fired within its own hour would skip the
		// day entirely, which is how people discover they have no backups.
		{"late tick still runs", daily(at(13, 3)), at(14, 9), true},
		{"never run", daily(time.Time{}), at(14, 3), true},
		{"disabled", &db.BackupSchedule{Enabled: false, Frequency: Daily, Hour: 3}, at(14, 9), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Due(tc.sc, tc.now); got != tc.want {
				t.Fatalf("Due = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestDueWeekly(t *testing.T) {
	// Weekday 1 is Monday; 2026-09-14 is a Monday and the 15th a Tuesday.
	sc := &db.BackupSchedule{Enabled: true, Frequency: Weekly, Weekday: 1, Hour: 3,
		Keep: 4, IncludeFiles: true, LastRunAt: at(7, 3)}

	if Due(sc, at(15, 9)) {
		t.Error("a weekly schedule fired on the wrong weekday")
	}
	if !Due(sc, at(14, 3)) {
		t.Error("a weekly schedule did not fire on its weekday")
	}
	sc.LastRunAt = at(14, 3)
	if Due(sc, at(14, 20)) {
		t.Error("a weekly schedule fired twice on the same day")
	}
}

func TestValidateSchedule(t *testing.T) {
	ok := &db.BackupSchedule{Frequency: Daily, Hour: 3, Weekday: 0, Keep: 7, IncludeFiles: true}
	if err := ValidateSchedule(ok); err != nil {
		t.Fatalf("a valid schedule was rejected: %v", err)
	}

	bad := []struct {
		name string
		mut  func(*db.BackupSchedule)
	}{
		{"unknown frequency", func(s *db.BackupSchedule) { s.Frequency = "hourly" }},
		{"hour too large", func(s *db.BackupSchedule) { s.Hour = 24 }},
		{"negative hour", func(s *db.BackupSchedule) { s.Hour = -1 }},
		{"weekday out of range", func(s *db.BackupSchedule) { s.Weekday = 7 }},
		{"keep of zero", func(s *db.BackupSchedule) { s.Keep = 0 }},
		{"keep beyond the cap", func(s *db.BackupSchedule) { s.Keep = MaxKeep + 1 }},
		{"nothing included", func(s *db.BackupSchedule) { s.IncludeFiles = false; s.IncludeDatabases = false }},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			s := *ok
			tc.mut(&s)
			if err := ValidateSchedule(&s); err == nil {
				t.Fatalf("%s was accepted", tc.name)
			}
		})
	}
}

func TestArchiveNameIsSafe(t *testing.T) {
	// An account name with a space and a capital would otherwise reach a
	// file path and an exec argument as it was typed.
	n := archiveName("Cust Omer_1")
	if !strings.HasPrefix(n, "cust-omer_1-") {
		t.Fatalf("archive name %q did not normalise the account name", n)
	}
	if !strings.HasSuffix(n, backuparchive.Extension) {
		t.Fatalf("archive name %q has the wrong extension", n)
	}
	if !actionsBackupName.MatchString(n) {
		t.Fatalf("archive name %q would be refused by the agent", n)
	}
}

// actionsBackupName mirrors the agent's own rule. Duplicated on purpose: if
// the two ever drift, every backup starts failing at the agent boundary, and
// this test says so at build time instead.
var actionsBackupName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{2,120}$`)
