package auth_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/pquerna/otp/totp"

	"github.com/bnixvn/opanel-ent/internal/auth"
	"github.com/bnixvn/opanel-ent/internal/db"
)

func newFixture(t *testing.T) (*db.DB, *auth.Service) {
	t.Helper()
	d, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "auth.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	// *db.DB must satisfy auth.Store; this assignment is the compile-time check.
	var store auth.Store = d
	return d, auth.NewService(store, time.Hour, 30*time.Minute)
}

func mustUser(t *testing.T, d *db.DB, username, password string, role auth.Role) *db.User {
	t.Helper()
	hash, err := auth.HashPassword(password)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	u, err := d.CreateUser(context.Background(), &db.User{
		Username: username, PasswordHash: hash, Role: string(role),
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	return u
}

func TestLoginSuccess(t *testing.T) {
	d, svc := newFixture(t)
	mustUser(t, d, "alice", "s3cret-password", auth.RoleAdmin)

	res, err := svc.Login(context.Background(), "alice", "s3cret-password", "", "192.0.2.1", "go-test")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if res.Cookie == "" {
		t.Fatal("login returned an empty cookie")
	}
	if res.Session.ID == res.Cookie {
		t.Fatal("the stored session id equals the cookie value; it must be a hash of it")
	}
	if !res.Session.ExpiresAt.After(time.Now()) {
		t.Fatal("session already expired")
	}
}

func TestLoginWrongPasswordAndUnknownUserAreIndistinguishable(t *testing.T) {
	d, svc := newFixture(t)
	mustUser(t, d, "alice", "right", auth.RoleAdmin)
	ctx := context.Background()

	_, errWrong := svc.Login(ctx, "alice", "wrong", "", "", "")
	_, errUnknown := svc.Login(ctx, "nobody", "wrong", "", "", "")

	if !errors.Is(errWrong, auth.ErrInvalidCredentials) {
		t.Fatalf("wrong password: got %v", errWrong)
	}
	if !errors.Is(errUnknown, auth.ErrInvalidCredentials) {
		t.Fatalf("unknown user: got %v", errUnknown)
	}
	if errWrong.Error() != errUnknown.Error() {
		t.Fatalf("errors differ and would leak which usernames exist: %q vs %q",
			errWrong, errUnknown)
	}
}

func TestLoginRejectsSuspendedAccount(t *testing.T) {
	d, svc := newFixture(t)
	u := mustUser(t, d, "alice", "pw", auth.RoleEndUser)
	ctx := context.Background()
	if _, err := d.ExecContext(ctx, `UPDATE users SET suspended = 1 WHERE id = ?`, u.ID); err != nil {
		t.Fatalf("suspend: %v", err)
	}

	if _, err := svc.Login(ctx, "alice", "pw", "", "", ""); !errors.Is(err, auth.ErrSuspended) {
		t.Fatalf("got %v, want ErrSuspended", err)
	}
	// A suspended account must not be disclosed to someone with the wrong
	// password: that would turn the endpoint into an account oracle.
	if _, err := svc.Login(ctx, "alice", "wrong", "", "", ""); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("wrong password on suspended account: got %v, want ErrInvalidCredentials", err)
	}
}

func TestValidateSession(t *testing.T) {
	d, svc := newFixture(t)
	mustUser(t, d, "alice", "pw", auth.RoleAdmin)
	ctx := context.Background()

	res, err := svc.Login(ctx, "alice", "pw", "", "", "")
	if err != nil {
		t.Fatalf("login: %v", err)
	}

	u, _, err := svc.ValidateSession(ctx, res.Cookie)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if u.Username != "alice" {
		t.Fatalf("validated as %q", u.Username)
	}

	if _, _, err := svc.ValidateSession(ctx, "not-a-real-cookie"); !errors.Is(err, auth.ErrSessionInvalid) {
		t.Fatalf("bogus cookie: got %v, want ErrSessionInvalid", err)
	}
	if _, _, err := svc.ValidateSession(ctx, ""); !errors.Is(err, auth.ErrSessionInvalid) {
		t.Fatalf("empty cookie: got %v, want ErrSessionInvalid", err)
	}
}

func TestLogoutInvalidatesSession(t *testing.T) {
	d, svc := newFixture(t)
	mustUser(t, d, "alice", "pw", auth.RoleAdmin)
	ctx := context.Background()

	res, _ := svc.Login(ctx, "alice", "pw", "", "", "")
	if err := svc.Logout(ctx, res.Cookie); err != nil {
		t.Fatalf("logout: %v", err)
	}
	if _, _, err := svc.ValidateSession(ctx, res.Cookie); !errors.Is(err, auth.ErrSessionInvalid) {
		t.Fatalf("session survived logout: %v", err)
	}
}

func TestSuspensionRevokesLiveSessions(t *testing.T) {
	d, svc := newFixture(t)
	u := mustUser(t, d, "alice", "pw", auth.RoleEndUser)
	ctx := context.Background()

	res, _ := svc.Login(ctx, "alice", "pw", "", "", "")
	if _, err := d.ExecContext(ctx, `UPDATE users SET suspended = 1 WHERE id = ?`, u.ID); err != nil {
		t.Fatalf("suspend: %v", err)
	}

	// Suspension must take effect immediately, not at the next login.
	if _, _, err := svc.ValidateSession(ctx, res.Cookie); !errors.Is(err, auth.ErrSuspended) {
		t.Fatalf("got %v, want ErrSuspended", err)
	}
}

func TestLoginWithTOTP(t *testing.T) {
	d, svc := newFixture(t)
	u := mustUser(t, d, "alice", "pw", auth.RoleAdmin)
	ctx := context.Background()

	setup, err := auth.NewTOTPSetup("OPanel", "alice")
	if err != nil {
		t.Fatalf("totp setup: %v", err)
	}
	if err := d.SetTOTP(ctx, u.ID, setup.Secret, true); err != nil {
		t.Fatalf("enable totp: %v", err)
	}
	if err := d.ReplaceRecoveryCodes(ctx, u.ID, auth.HashRecoveryCodes(setup.RecoveryCodes)); err != nil {
		t.Fatalf("store recovery codes: %v", err)
	}

	if _, err := svc.Login(ctx, "alice", "pw", "", "", ""); !errors.Is(err, auth.ErrTOTPRequired) {
		t.Fatalf("missing code: got %v, want ErrTOTPRequired", err)
	}
	if _, err := svc.Login(ctx, "alice", "pw", "000000", "", ""); !errors.Is(err, auth.ErrTOTPInvalid) {
		t.Fatalf("bad code: got %v, want ErrTOTPInvalid", err)
	}

	code, err := totp.GenerateCode(setup.Secret, time.Now())
	if err != nil {
		t.Fatalf("generate code: %v", err)
	}
	if _, err := svc.Login(ctx, "alice", "pw", code, "", ""); err != nil {
		t.Fatalf("valid code: %v", err)
	}
}

func TestRecoveryCodeWorksOnceAsSecondFactor(t *testing.T) {
	d, svc := newFixture(t)
	u := mustUser(t, d, "alice", "pw", auth.RoleAdmin)
	ctx := context.Background()

	setup, _ := auth.NewTOTPSetup("OPanel", "alice")
	_ = d.SetTOTP(ctx, u.ID, setup.Secret, true)
	_ = d.ReplaceRecoveryCodes(ctx, u.ID, auth.HashRecoveryCodes(setup.RecoveryCodes))

	rc := setup.RecoveryCodes[0]
	if _, err := svc.Login(ctx, "alice", "pw", rc, "", ""); err != nil {
		t.Fatalf("first use of recovery code: %v", err)
	}
	if _, err := svc.Login(ctx, "alice", "pw", rc, "", ""); !errors.Is(err, auth.ErrTOTPInvalid) {
		t.Fatalf("recovery code was reusable: %v", err)
	}
}

func TestAPITokenLifecycle(t *testing.T) {
	d, svc := newFixture(t)
	u := mustUser(t, d, "billing", "pw", auth.RoleAdmin)
	ctx := context.Background()

	tok, plain, err := svc.IssueAPIToken(ctx, u.ID, "whmcs", []auth.Scope{auth.ScopeProvisioningManage}, nil)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if plain == "" {
		t.Fatal("issue returned an empty token")
	}

	got, gotTok, err := svc.AuthenticateToken(ctx, plain)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if got.ID != u.ID || gotTok.ID != tok.ID {
		t.Fatal("token resolved to the wrong user or record")
	}
	if len(gotTok.Scopes) != 1 || gotTok.Scopes[0] != string(auth.ScopeProvisioningManage) {
		t.Fatalf("scopes did not round trip: %v", gotTok.Scopes)
	}

	for name, bad := range map[string]string{
		"empty":             "",
		"wrong shape":       "not-a-token",
		"wrong prefix":      "opanel_unknownpfx_" + plain,
		"wrong secret":      "opanel_" + tok.Prefix + "_wrongsecret",
		"foreign namespace": "other_" + tok.Prefix + "_x",
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := svc.AuthenticateToken(ctx, bad); !errors.Is(err, auth.ErrTokenInvalid) {
				t.Fatalf("got %v, want ErrTokenInvalid", err)
			}
		})
	}

	if err := d.RevokeAPIToken(ctx, tok.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, _, err := svc.AuthenticateToken(ctx, plain); !errors.Is(err, auth.ErrTokenInvalid) {
		t.Fatalf("revoked token still authenticates: %v", err)
	}
}

func TestExpiredAPITokenIsRejected(t *testing.T) {
	d, svc := newFixture(t)
	u := mustUser(t, d, "billing", "pw", auth.RoleAdmin)
	ctx := context.Background()

	past := time.Now().Add(-time.Hour)
	_, plain, err := svc.IssueAPIToken(ctx, u.ID, "stale", nil, &past)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if _, _, err := svc.AuthenticateToken(ctx, plain); !errors.Is(err, auth.ErrTokenInvalid) {
		t.Fatalf("expired token accepted: %v", err)
	}
}

func TestIssueAPITokenRejectsUnknownScope(t *testing.T) {
	d, svc := newFixture(t)
	u := mustUser(t, d, "billing", "pw", auth.RoleAdmin)
	if _, _, err := svc.IssueAPIToken(context.Background(), u.ID, "bad", []auth.Scope{"root:everything"}, nil); err == nil {
		t.Fatal("an unknown scope was accepted")
	}
}

func TestRoleOrdering(t *testing.T) {
	if !auth.RoleAdmin.AtLeast(auth.RoleEndUser) {
		t.Error("admin should outrank end_user")
	}
	if auth.RoleEndUser.AtLeast(auth.RoleReseller) {
		t.Error("end_user should not outrank reseller")
	}
	if auth.Role("nonsense").AtLeast(auth.RoleEndUser) {
		t.Error("an unknown role should outrank nothing")
	}
	if auth.Role("nonsense").Valid() {
		t.Error("an unknown role should not validate")
	}
}
