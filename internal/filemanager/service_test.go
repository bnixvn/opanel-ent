package filemanager

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/bnixvn/opanel-ent/internal/auth"
	"github.com/bnixvn/opanel-ent/internal/db"
)

// newService gives the service a real database, because resolving an owner
// now reads the account's home directory from it.
func newService(t *testing.T) *Service {
	t.Helper()
	database, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "fm.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return New(database, nil, nil)
}

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
	s := newService(t)
	actor := &db.User{ID: 7, Username: "customer1", Role: string(auth.RoleEndUser), LinuxUID: uid(1001)}

	got, err := s.ResolveOwner(context.Background(), actor, "")
	if err != nil {
		t.Fatalf("own home was refused: %v", err)
	}
	if got.Username != "customer1" {
		t.Fatalf("resolved to %q, want customer1", got.Username)
	}
	// The home is what the interface shows, and showing the wrong one would
	// tell a customer their files are somewhere they are not.
	if got.Home != "/home/customer1" {
		t.Fatalf("home = %q, want /home/customer1", got.Home)
	}

	// Naming yourself explicitly is the same request.
	if _, err := s.ResolveOwner(context.Background(), actor, "customer1"); err != nil {
		t.Fatalf("naming own account was refused: %v", err)
	}

	// Refused before any database lookup, which is why a service with an
	// empty database still gives the right answer here.
	if _, err := s.ResolveOwner(context.Background(), actor, "customer2"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("end user reached another account: err = %v", err)
	}
}

func TestResolveOwnerRejectsAnAccountWithNoHome(t *testing.T) {
	s := newService(t)
	admin := &db.User{ID: 1, Username: "admin", Role: string(auth.RoleAdmin)}
	if _, err := s.ResolveOwner(context.Background(), admin, ""); !errors.Is(err, ErrNoAccount) {
		t.Fatalf("an account with no Linux user resolved: err = %v", err)
	}
}
