package httpapi_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/bnixvn/opanel-ent/internal/auth"
	"github.com/bnixvn/opanel-ent/internal/db"
)

// loginEndUser creates an end user with a provisioned Linux account and
// returns a client holding its session.
func loginEndUser(t *testing.T, f *fixture, username string, linuxUID int64) *http.Client {
	t.Helper()
	hash, err := auth.HashPassword("pw")
	if err != nil {
		t.Fatal(err)
	}
	u, err := f.db.CreateUser(context.Background(), &db.User{
		Username: username, PasswordHash: hash, Role: string(auth.RoleEndUser),
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	if linuxUID > 0 {
		if err := f.db.SetUserLinuxAccount(context.Background(), u.ID, linuxUID, "/home/"+username); err != nil {
			t.Fatalf("set linux account: %v", err)
		}
	}
	c := f.client(t)
	resp := post(t, c, f.srv.URL+"/api/auth/login",
		map[string]string{"username": username, "password": "pw"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login returned %d", resp.StatusCode)
	}
	decode(t, resp, nil)
	return c
}

func TestFilesRequireAuthentication(t *testing.T) {
	f := newFixture(t)
	resp, err := http.Get(f.srv.URL + "/api/files")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous listing returned %d, want 401", resp.StatusCode)
	}
	decode(t, resp, nil)
}

// The one that matters: the query string must not be a way to read another
// customer's home. This is checked before the agent is ever called, which is
// why it passes with no agent running.
func TestFilesRefuseAnotherOwner(t *testing.T) {
	f := newFixture(t)
	c := loginEndUser(t, f, "customer1", 1001)

	resp, err := c.Get(f.srv.URL + "/api/files?owner=customer2")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("browsing another account returned %d, want 403", resp.StatusCode)
	}
	var body struct {
		Code string `json:"code"`
	}
	decode(t, resp, &body)
	if body.Code != "forbidden" {
		t.Fatalf("code = %q, want forbidden", body.Code)
	}
}

func TestFilesRejectAnAccountWithNoHome(t *testing.T) {
	f := newFixture(t)
	// No Linux account, which is the normal state for staff.
	c := loginEndUser(t, f, "customer3", 0)

	resp, err := c.Get(f.srv.URL + "/api/files")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("returned %d, want 400", resp.StatusCode)
	}
	var body struct {
		Code string `json:"code"`
	}
	decode(t, resp, &body)
	if body.Code != "no_account" {
		t.Fatalf("code = %q, want no_account", body.Code)
	}
}

// With a home but no agent the request must fail as a server error, not hang
// and not panic — the same contract the other agent-backed routes hold to.
func TestFilesFailCleanlyWhenAgentIsDown(t *testing.T) {
	f := newFixture(t)
	c := loginEndUser(t, f, "customer4", 1004)

	resp, err := c.Get(f.srv.URL + "/api/files")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("returned %d, want 500", resp.StatusCode)
	}
	decode(t, resp, nil)
}

func TestFileLimitsAreReported(t *testing.T) {
	f := newFixture(t)
	c := loginEndUser(t, f, "customer5", 1005)

	resp, err := c.Get(f.srv.URL + "/api/files/limits")
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		MaxUpload int64 `json:"max_upload_bytes"`
		MaxEdit   int64 `json:"max_edit_bytes"`
	}
	decode(t, resp, &body)
	if body.MaxUpload == 0 || body.MaxEdit == 0 {
		t.Fatalf("limits came back empty: %+v", body)
	}
}
