package db

import (
	"context"
	"errors"
	"testing"
	"time"
)

// ticketOwner makes the account the tickets hang off. Tickets carry a
// foreign key to it so a deleted account takes its live sessions with it.
func ticketOwner(t *testing.T, d *DB) int64 {
	t.Helper()
	u, err := d.CreateUser(context.Background(), &User{
		Username: "ticketowner", PasswordHash: "h", Role: "end_user",
	})
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	return u.ID
}

func newTicket(t *testing.T, d *DB, owner int64, hash, dbUser string, expires time.Time) {
	t.Helper()
	if err := d.CreateSSOTicket(context.Background(), &SSOTicket{
		TokenHash: hash, UserID: owner, DBUser: dbUser,
		DBPassword: "secret", ExpiresAt: expires,
	}); err != nil {
		t.Fatalf("create ticket %s: %v", hash, err)
	}
}

// A ticket is worth exactly one sign-in. If it were worth two, a link sitting
// in browser history or a proxy log would still open somebody's databases.
func TestRedeemSSOTicketIsSingleUse(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()
	owner := ticketOwner(t, d)
	newTicket(t, d, owner, "hash-a", "pma_alice_x", time.Now().Add(time.Hour))

	got, err := d.RedeemSSOTicket(ctx, "hash-a")
	if err != nil {
		t.Fatalf("first redeem: %v", err)
	}
	if got.DBUser != "pma_alice_x" || got.DBPassword != "secret" {
		t.Fatalf("redeem returned %q/%q", got.DBUser, got.DBPassword)
	}
	if got.UsedAt.IsZero() {
		t.Error("used_at was not stamped, so the ticket still looks unused")
	}

	if _, err := d.RedeemSSOTicket(ctx, "hash-a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second redeem: err = %v, want ErrNotFound", err)
	}
}

func TestRedeemSSOTicketRejectsUnknownAndExpired(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()
	owner := ticketOwner(t, d)
	newTicket(t, d, owner, "hash-old", "pma_bob_x", time.Now().Add(-time.Minute))

	for _, tc := range []struct{ name, hash string }{
		{"never existed", "hash-nope"},
		{"expired", "hash-old"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := d.RedeemSSOTicket(ctx, tc.hash); !errors.Is(err, ErrNotFound) {
				t.Fatalf("err = %v, want ErrNotFound", err)
			}
		})
	}
}

// The sweep drops accounts by expiry, not by use: a redeemed ticket is
// backing a live phpMyAdmin session, and dropping its account would log the
// customer out in the middle of a query.
func TestExpiredSSOAccountsIgnoresLiveSessions(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()
	now := time.Now()
	owner := ticketOwner(t, d)
	newTicket(t, d, owner, "hash-live", "pma_live", now.Add(time.Hour))
	newTicket(t, d, owner, "hash-done", "pma_done", now.Add(-time.Hour))

	// Redeeming the live one must not make it a sweep candidate.
	if _, err := d.RedeemSSOTicket(ctx, "hash-live"); err != nil {
		t.Fatalf("redeem: %v", err)
	}

	got, err := d.ExpiredSSOAccounts(ctx, now)
	if err != nil {
		t.Fatalf("expired: %v", err)
	}
	if len(got) != 1 || got[0] != "pma_done" {
		t.Fatalf("expired accounts = %v, want [pma_done]", got)
	}

	if err := d.DeleteSSOTickets(ctx, got); err != nil {
		t.Fatalf("delete: %v", err)
	}
	after, err := d.ExpiredSSOAccounts(ctx, now)
	if err != nil {
		t.Fatalf("expired after delete: %v", err)
	}
	if len(after) != 0 {
		t.Fatalf("still expired after delete: %v", after)
	}
}
