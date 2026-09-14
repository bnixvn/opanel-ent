package httpapi_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/bnixvn/opanel-ent/internal/auth"
	"github.com/bnixvn/opanel-ent/internal/db"
)

func makeUser(t *testing.T, f *fixture, username, role string, parent int64) *db.User {
	t.Helper()
	hash, err := auth.HashPassword("pw-" + username)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	u := &db.User{Username: username, PasswordHash: hash, Role: role}
	if parent != 0 {
		u.ParentID = &parent
	}
	created, err := f.db.CreateUser(context.Background(), u)
	if err != nil {
		t.Fatalf("create %s: %v", username, err)
	}
	return created
}

func signIn(t *testing.T, f *fixture, username string) *http.Client {
	t.Helper()
	c := f.client(t)
	resp := post(t, c, f.srv.URL+"/api/auth/login",
		map[string]string{"username": username, "password": "pw-" + username})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login as %s returned %d", username, resp.StatusCode)
	}
	decode(t, resp, nil)
	return c
}

func whoAmI(t *testing.T, f *fixture, c *http.Client) map[string]any {
	t.Helper()
	r, err := c.Get(f.srv.URL + "/api/auth/me")
	if err != nil {
		t.Fatalf("GET me: %v", err)
	}
	if r.StatusCode != http.StatusOK {
		t.Fatalf("GET me returned %d", r.StatusCode)
	}
	var out map[string]any
	decode(t, r, &out)
	return out
}

// The whole feature in one test: an operator signs in as a customer, the
// session becomes the customer's, and the way back returns the operator's
// own account rather than asking them to sign in again.
func TestImpersonateRoundTrip(t *testing.T) {
	f := newFixture(t)
	makeUser(t, f, "root", string(auth.RoleAdmin), 0)
	customer := makeUser(t, f, "carol", string(auth.RoleEndUser), 0)
	c := signIn(t, f, "root")

	resp := post(t, c, f.srv.URL+"/api/users/"+itoa64(customer.ID)+"/impersonate", map[string]any{})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("impersonate returned %d", resp.StatusCode)
	}
	decode(t, resp, nil)

	me := whoAmI(t, f, c)
	if me["username"] != "carol" {
		t.Fatalf("session is %v, want carol", me["username"])
	}
	if me["impersonated"] != true {
		t.Fatal("the session does not say it is an impersonation")
	}
	if me["impersonated_by"] != "root" {
		t.Fatalf("impersonated_by = %v, want root", me["impersonated_by"])
	}

	// Now a customer, so an admin route has to be closed.
	r, err := c.Get(f.srv.URL + "/api/system/audit")
	if err != nil {
		t.Fatalf("GET audit: %v", err)
	}
	if r.StatusCode != http.StatusForbidden {
		t.Fatalf("an impersonated session reached an admin route: %d", r.StatusCode)
	}
	decode(t, r, nil)

	back := post(t, c, f.srv.URL+"/api/auth/impersonate/stop", map[string]any{})
	if back.StatusCode != http.StatusOK {
		t.Fatalf("stop returned %d", back.StatusCode)
	}
	decode(t, back, nil)

	me = whoAmI(t, f, c)
	if me["username"] != "root" {
		t.Fatalf("after returning, the session is %v, want root", me["username"])
	}
	if me["impersonated"] == true {
		t.Fatal("the returned session still says it is an impersonation")
	}
}

// The rules that keep this from being a privilege-escalation route.
func TestImpersonateRefusals(t *testing.T) {
	f := newFixture(t)
	makeUser(t, f, "root", string(auth.RoleAdmin), 0)
	resellerA := makeUser(t, f, "resa", string(auth.RoleReseller), 0)
	makeUser(t, f, "resb", string(auth.RoleReseller), 0)
	mine := makeUser(t, f, "mine", string(auth.RoleEndUser), resellerA.ID)
	theirs := makeUser(t, f, "theirs", string(auth.RoleEndUser), 0)
	admin2 := makeUser(t, f, "root2", string(auth.RoleAdmin), 0)

	t.Run("a customer cannot impersonate at all", func(t *testing.T) {
		c := signIn(t, f, "mine")
		resp := post(t, c, f.srv.URL+"/api/users/"+itoa64(theirs.ID)+"/impersonate", map[string]any{})
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("got %d, want 403", resp.StatusCode)
		}
	})

	t.Run("a reseller cannot reach outside their own subtree", func(t *testing.T) {
		c := signIn(t, f, "resa")
		resp := post(t, c, f.srv.URL+"/api/users/"+itoa64(theirs.ID)+"/impersonate", map[string]any{})
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode == http.StatusOK {
			t.Fatal("a reseller signed in as somebody else's customer")
		}
	})

	t.Run("nobody signs in as another administrator", func(t *testing.T) {
		c := signIn(t, f, "root")
		resp := post(t, c, f.srv.URL+"/api/users/"+itoa64(admin2.ID)+"/impersonate", map[string]any{})
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("got %d, want 403", resp.StatusCode)
		}
	})

	t.Run("a suspended account cannot be worn", func(t *testing.T) {
		if err := f.db.SetUserSuspended(context.Background(), mine.ID, true); err != nil {
			t.Skipf("cannot suspend in this fixture: %v", err)
		}
		c := signIn(t, f, "root")
		resp := post(t, c, f.srv.URL+"/api/users/"+itoa64(mine.ID)+"/impersonate", map[string]any{})
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode == http.StatusOK {
			t.Fatal("a suspended account was impersonated")
		}
		_ = f.db.SetUserSuspended(context.Background(), mine.ID, false)
	})

	t.Run("a second hop is refused", func(t *testing.T) {
		c := signIn(t, f, "root")
		first := post(t, c, f.srv.URL+"/api/users/"+itoa64(mine.ID)+"/impersonate", map[string]any{})
		if first.StatusCode != http.StatusOK {
			t.Fatalf("first hop returned %d", first.StatusCode)
		}
		decode(t, first, nil)
		// The session is now a customer, so the route is closed by role
		// before the second-hop check is even reached. Either refusal is
		// correct; what matters is that it is refused.
		second := post(t, c, f.srv.URL+"/api/users/"+itoa64(theirs.ID)+"/impersonate", map[string]any{})
		defer func() { _ = second.Body.Close() }()
		if second.StatusCode == http.StatusOK {
			t.Fatal("an impersonated session impersonated somebody else")
		}
	})
}

// Credentials must not be changeable from inside somebody else's account:
// the change would be recorded against the customer, and the account holder
// could not tell it from something they did themselves.
func TestImpersonatedSessionCannotChangeCredentials(t *testing.T) {
	f := newFixture(t)
	makeUser(t, f, "root", string(auth.RoleAdmin), 0)
	customer := makeUser(t, f, "carol", string(auth.RoleEndUser), 0)
	c := signIn(t, f, "root")

	resp := post(t, c, f.srv.URL+"/api/users/"+itoa64(customer.ID)+"/impersonate", map[string]any{})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("impersonate returned %d", resp.StatusCode)
	}
	decode(t, resp, nil)

	for _, path := range []string{"/api/auth/password", "/api/auth/2fa/setup", "/api/auth/2fa/disable"} {
		t.Run(path, func(t *testing.T) {
			r := post(t, c, f.srv.URL+path, map[string]string{
				"current_password": "pw-carol", "new_password": "something-else-entirely",
			})
			defer func() { _ = r.Body.Close() }()
			if r.StatusCode != http.StatusForbidden {
				t.Fatalf("%s returned %d, want 403", path, r.StatusCode)
			}
		})
	}
}

// Returning from a session that was never an impersonation must not mint a
// new session for user 0 or anybody else.
func TestStopWithoutImpersonating(t *testing.T) {
	f := newFixture(t)
	makeUser(t, f, "root", string(auth.RoleAdmin), 0)
	c := signIn(t, f, "root")

	resp := post(t, c, f.srv.URL+"/api/auth/impersonate/stop", map[string]any{})
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", resp.StatusCode)
	}
	if me := whoAmI(t, f, c); me["username"] != "root" {
		t.Fatalf("the session changed to %v", me["username"])
	}
}

func itoa64(n int64) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
