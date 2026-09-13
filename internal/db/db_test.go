package db

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func newTestDB(t *testing.T) *DB {
	t.Helper()
	d, err := Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

func TestOpenAppliesMigrations(t *testing.T) {
	d := newTestDB(t)
	v, err := d.SchemaVersion(context.Background())
	if err != nil {
		t.Fatalf("schema version: %v", err)
	}
	if v < 1 {
		t.Fatalf("schema version = %d, want >= 1", v)
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	// Open already migrates; running again must be a no-op rather than an
	// error, because opanel-api migrates on every start.
	d := newTestDB(t)
	ctx := context.Background()
	before, _ := d.SchemaVersion(ctx)
	if err := d.Migrate(ctx); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	after, _ := d.SchemaVersion(ctx)
	if before != after {
		t.Fatalf("schema moved from %d to %d on re-migrate", before, after)
	}
}

func TestUserRoundTrip(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()

	created, err := d.CreateUser(ctx, &User{
		Username: "alice", Email: "a@example.test",
		PasswordHash: "hash", Role: "admin",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.ID == 0 {
		t.Fatal("create returned id 0")
	}

	got, err := d.UserByUsername(ctx, "alice")
	if err != nil {
		t.Fatalf("by username: %v", err)
	}
	if got.ID != created.ID || got.Role != "admin" || got.Email != "a@example.test" {
		t.Fatalf("round trip mismatch: %+v", got)
	}
	if got.CreatedAt.IsZero() {
		t.Fatal("created_at did not round trip")
	}

	if _, err := d.UserByUsername(ctx, "nobody"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing user: got %v, want ErrNotFound", err)
	}
}

func TestUserRoleIsConstrained(t *testing.T) {
	// The CHECK constraint is the last line of defence if a caller skips
	// validation, so verify the database really rejects a bad role.
	d := newTestDB(t)
	_, err := d.CreateUser(context.Background(), &User{
		Username: "bob", PasswordHash: "h", Role: "superuser",
	})
	if err == nil {
		t.Fatal("expected role CHECK constraint to reject 'superuser'")
	}
}

func TestSessionLifecycle(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()
	u, _ := d.CreateUser(ctx, &User{Username: "carol", PasswordHash: "h", Role: "end_user"})

	now := time.Now().UTC().Truncate(time.Second)
	s := &Session{
		ID: "hash-1", UserID: u.ID,
		CreatedAt: now, ExpiresAt: now.Add(time.Hour), LastSeenAt: now,
		IP: "192.0.2.1",
	}
	if err := d.CreateSession(ctx, s); err != nil {
		t.Fatalf("create session: %v", err)
	}

	got, err := d.SessionByID(ctx, "hash-1")
	if err != nil {
		t.Fatalf("session by id: %v", err)
	}
	if got.UserID != u.ID || !got.ExpiresAt.Equal(s.ExpiresAt) {
		t.Fatalf("session mismatch: %+v", got)
	}

	if err := d.DeleteSession(ctx, "hash-1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := d.SessionByID(ctx, "hash-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after delete: got %v, want ErrNotFound", err)
	}
}

func TestPurgeExpiredSessions(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()
	u, _ := d.CreateUser(ctx, &User{Username: "dave", PasswordHash: "h", Role: "end_user"})

	now := time.Now().UTC()
	live := &Session{ID: "live", UserID: u.ID, CreatedAt: now, ExpiresAt: now.Add(time.Hour), LastSeenAt: now}
	dead := &Session{ID: "dead", UserID: u.ID, CreatedAt: now.Add(-2 * time.Hour), ExpiresAt: now.Add(-time.Hour), LastSeenAt: now.Add(-2 * time.Hour)}
	_ = d.CreateSession(ctx, live)
	_ = d.CreateSession(ctx, dead)

	n, err := d.PurgeExpiredSessions(ctx, now)
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if n != 1 {
		t.Fatalf("purged %d sessions, want 1", n)
	}
	if _, err := d.SessionByID(ctx, "live"); err != nil {
		t.Fatalf("live session was removed: %v", err)
	}
}

func TestSessionsCascadeOnUserDelete(t *testing.T) {
	// foreign_keys=ON is set as a pragma; if it silently failed to apply,
	// deleting a user would orphan sessions that still authenticate.
	d := newTestDB(t)
	ctx := context.Background()
	u, _ := d.CreateUser(ctx, &User{Username: "erin", PasswordHash: "h", Role: "end_user"})
	now := time.Now().UTC()
	_ = d.CreateSession(ctx, &Session{ID: "s1", UserID: u.ID, CreatedAt: now, ExpiresAt: now.Add(time.Hour), LastSeenAt: now})

	if _, err := d.ExecContext(ctx, `DELETE FROM users WHERE id = ?`, u.ID); err != nil {
		t.Fatalf("delete user: %v", err)
	}
	if _, err := d.SessionByID(ctx, "s1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("session survived user deletion: %v", err)
	}
}

func TestRecoveryCodesAreSingleUse(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()
	u, _ := d.CreateUser(ctx, &User{Username: "frank", PasswordHash: "h", Role: "end_user"})

	if err := d.ReplaceRecoveryCodes(ctx, u.ID, []string{"a", "b"}); err != nil {
		t.Fatalf("replace: %v", err)
	}
	n, _ := d.CountRecoveryCodes(ctx, u.ID)
	if n != 2 {
		t.Fatalf("have %d codes, want 2", n)
	}

	ok, err := d.RedeemRecoveryCode(ctx, u.ID, "a")
	if err != nil || !ok {
		t.Fatalf("first redeem: ok=%v err=%v", ok, err)
	}
	ok, err = d.RedeemRecoveryCode(ctx, u.ID, "a")
	if err != nil {
		t.Fatalf("second redeem: %v", err)
	}
	if ok {
		t.Fatal("a recovery code was accepted twice")
	}
}

func TestAuditAppend(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()
	for i := range 3 {
		if err := d.AppendAudit(ctx, AuditEntry{
			ActorType: "system", Action: "test", OK: i%2 == 0,
		}); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	got, err := d.RecentAudit(ctx, 10)
	if err != nil {
		t.Fatalf("recent: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d entries, want 3", len(got))
	}
	// Newest first.
	if got[0].ID < got[len(got)-1].ID {
		t.Fatal("RecentAudit did not return newest first")
	}
}
