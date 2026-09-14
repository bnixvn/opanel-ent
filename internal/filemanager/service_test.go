package filemanager

import (
	"context"
	"errors"
	"testing"

	"github.com/bnixvn/opanel-ent/internal/auth"
	"github.com/bnixvn/opanel-ent/internal/db"
)

func TestClean(t *testing.T) {
	cases := map[string]string{
		"":                        "",
		"/":                       "",
		".":                       "",
		"  public_html  ":         "public_html",
		"/public_html/":           "public_html",
		"public_html//index.php":  "public_html/index.php",
		"./public_html/./x":       "public_html/x",
		"public_html/wp/../index": "public_html/index",
	}
	for in, want := range cases {
		if got := clean(in); got != want {
			t.Errorf("clean(%q) = %q, want %q", in, got, want)
		}
	}
}

func uid(n int64) *int64 { return &n }

func TestResolveOwnerKeepsEndUsersInTheirOwnHome(t *testing.T) {
	// A nil database is deliberate: an end user naming somebody else must be
	// refused before any lookup happens, so this passing means the check is
	// where it should be rather than after a round trip.
	s := &Service{}
	actor := &db.User{ID: 7, Username: "customer1", Role: string(auth.RoleEndUser), LinuxUID: uid(1001)}

	got, err := s.ResolveOwner(context.Background(), actor, "")
	if err != nil {
		t.Fatalf("own home was refused: %v", err)
	}
	if got.Username != "customer1" {
		t.Fatalf("resolved to %q, want customer1", got.Username)
	}

	// Naming yourself explicitly is the same request.
	if _, err := s.ResolveOwner(context.Background(), actor, "customer1"); err != nil {
		t.Fatalf("naming own account was refused: %v", err)
	}

	if _, err := s.ResolveOwner(context.Background(), actor, "customer2"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("end user reached another account: err = %v", err)
	}
}

func TestResolveOwnerRejectsAnAccountWithNoHome(t *testing.T) {
	s := &Service{}
	admin := &db.User{ID: 1, Username: "admin", Role: string(auth.RoleAdmin)}
	if _, err := s.ResolveOwner(context.Background(), admin, ""); !errors.Is(err, ErrNoAccount) {
		t.Fatalf("an account with no Linux user resolved: err = %v", err)
	}
}
