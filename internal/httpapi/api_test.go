package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/bnixvn/opanel-ent/internal/agentclient"
	"github.com/bnixvn/opanel-ent/internal/auth"
	"github.com/bnixvn/opanel-ent/internal/config"
	"github.com/bnixvn/opanel-ent/internal/db"
	"github.com/bnixvn/opanel-ent/internal/httpapi"
	"github.com/bnixvn/opanel-ent/internal/sites"
	"github.com/bnixvn/opanel-ent/internal/webserver"
)

type fixture struct {
	srv *httptest.Server
	db  *db.DB
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()

	database, err := db.Open(ctx, filepath.Join(t.TempDir(), "api.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	cfg := &config.Config{
		Env:              config.EnvDev, // no Secure flag, so httptest over HTTP keeps the cookie
		DataDir:          t.TempDir(),
		ListenAddr:       "127.0.0.1:0",
		WebserverBackend: config.BackendOLS,
		LogLevel:         "error",
		SessionTTL:       time.Hour,
		SessionIdleTTL:   30 * time.Minute,
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config: %v", err)
	}

	authSvc := auth.NewService(database, cfg.SessionTTL, cfg.SessionIdleTTL)
	// Points at a socket that does not exist: agent-backed routes must fail
	// cleanly rather than hang or panic when the agent is down.
	ac := agentclient.New(filepath.Join(t.TempDir(), "absent.sock"), 2*time.Second)

	siteSvc := sites.New(database, ac, webserver.DefaultServerConfig(), discardLogger())
	api := httpapi.New(cfg, database, authSvc, ac, siteSvc, discardLogger())
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)

	return &fixture{srv: srv, db: database}
}

func (f *fixture) createAdmin(t *testing.T, username, password string) {
	t.Helper()
	hash, err := auth.HashPassword(password)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if _, err := f.db.CreateUser(context.Background(), &db.User{
		Username: username, PasswordHash: hash, Role: string(auth.RoleAdmin),
	}); err != nil {
		t.Fatalf("create admin: %v", err)
	}
}

func (f *fixture) client(t *testing.T) *http.Client {
	t.Helper()
	jar := newJar()
	return &http.Client{Jar: jar, Timeout: 10 * time.Second}
}

func post(t *testing.T, c *http.Client, url string, body any) *http.Response {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	resp, err := c.Post(url, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	return resp
}

func decode(t *testing.T, resp *http.Response, v any) {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if v == nil {
		return
	}
	if err := json.Unmarshal(body, v); err != nil {
		t.Fatalf("decode %q: %v", string(body), err)
	}
}

func TestLoginLogoutFlow(t *testing.T) {
	f := newFixture(t)
	f.createAdmin(t, "admin", "a-good-password")
	c := f.client(t)

	// Unauthenticated access is refused.
	resp, err := c.Get(f.srv.URL + "/api/auth/me")
	if err != nil {
		t.Fatalf("GET me: %v", err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated /me returned %d, want 401", resp.StatusCode)
	}
	decode(t, resp, nil)

	// Wrong password.
	resp = post(t, c, f.srv.URL+"/api/auth/login",
		map[string]string{"username": "admin", "password": "nope"})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad login returned %d, want 401", resp.StatusCode)
	}
	var errBody struct {
		Code string `json:"code"`
	}
	decode(t, resp, &errBody)
	if errBody.Code != "invalid_credentials" {
		t.Fatalf("bad login code = %q", errBody.Code)
	}

	// Correct password.
	resp = post(t, c, f.srv.URL+"/api/auth/login",
		map[string]string{"username": "admin", "password": "a-good-password"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login returned %d, want 200", resp.StatusCode)
	}
	var login struct {
		User struct {
			Username string `json:"username"`
			Role     string `json:"role"`
		} `json:"user"`
	}
	decode(t, resp, &login)
	if login.User.Username != "admin" || login.User.Role != "admin" {
		t.Fatalf("login body = %+v", login)
	}

	// The session cookie now authenticates.
	resp, err = c.Get(f.srv.URL + "/api/auth/me")
	if err != nil {
		t.Fatalf("GET me: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("authenticated /me returned %d, want 200", resp.StatusCode)
	}
	decode(t, resp, nil)

	// Logout revokes it.
	resp = post(t, c, f.srv.URL+"/api/auth/logout", struct{}{})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("logout returned %d", resp.StatusCode)
	}
	decode(t, resp, nil)

	resp, err = c.Get(f.srv.URL + "/api/auth/me")
	if err != nil {
		t.Fatalf("GET me: %v", err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("/me after logout returned %d, want 401", resp.StatusCode)
	}
	decode(t, resp, nil)
}

func TestLoginNeverLeaksThatAUserExists(t *testing.T) {
	f := newFixture(t)
	f.createAdmin(t, "admin", "pw")
	c := f.client(t)

	known := post(t, c, f.srv.URL+"/api/auth/login",
		map[string]string{"username": "admin", "password": "wrong"})
	var a struct{ Error, Code string }
	decode(t, known, &a)

	unknown := post(t, c, f.srv.URL+"/api/auth/login",
		map[string]string{"username": "ghost", "password": "wrong"})
	var b struct{ Error, Code string }
	decode(t, unknown, &b)

	if known.StatusCode != unknown.StatusCode || a != b {
		t.Fatalf("responses differ and reveal which usernames exist: %d %+v vs %d %+v",
			known.StatusCode, a, unknown.StatusCode, b)
	}
}

func TestLoginRateLimit(t *testing.T) {
	f := newFixture(t)
	f.createAdmin(t, "admin", "pw")
	c := f.client(t)

	var got429 bool
	// The limiter allows 5 per minute; the 6th must be refused.
	for i := 0; i < 8; i++ {
		resp := post(t, c, f.srv.URL+"/api/auth/login",
			map[string]string{"username": "admin", "password": "wrong"})
		code := resp.StatusCode
		decode(t, resp, nil)
		if code == http.StatusTooManyRequests {
			got429 = true
			break
		}
	}
	if !got429 {
		t.Fatal("repeated failed logins were never rate limited")
	}
}

func TestRoleGate(t *testing.T) {
	f := newFixture(t)
	hash, _ := auth.HashPassword("pw")
	if _, err := f.db.CreateUser(context.Background(), &db.User{
		Username: "user", PasswordHash: hash, Role: string(auth.RoleEndUser),
	}); err != nil {
		t.Fatalf("create user: %v", err)
	}
	c := f.client(t)

	resp := post(t, c, f.srv.URL+"/api/auth/login",
		map[string]string{"username": "user", "password": "pw"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login returned %d", resp.StatusCode)
	}
	decode(t, resp, nil)

	r, err := c.Get(f.srv.URL + "/api/system/audit")
	if err != nil {
		t.Fatalf("GET audit: %v", err)
	}
	if r.StatusCode != http.StatusForbidden {
		t.Fatalf("end_user reached an admin route: %d", r.StatusCode)
	}
	decode(t, r, nil)
}

func TestHealthReportsAgentDown(t *testing.T) {
	f := newFixture(t)
	resp, err := http.Get(f.srv.URL + "/api/health")
	if err != nil {
		t.Fatalf("GET health: %v", err)
	}
	var h struct {
		OK    bool   `json:"ok"`
		DB    string `json:"db"`
		Agent string `json:"agent"`
	}
	decode(t, resp, &h)

	// The fixture points at a socket that does not exist.
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("health returned %d with the agent down, want 503", resp.StatusCode)
	}
	if h.DB != "ok" {
		t.Fatalf("db reported as %q", h.DB)
	}
	if h.Agent != "unavailable" {
		t.Fatalf("agent reported as %q, want unavailable", h.Agent)
	}
}

func TestAdminRouteFailsCleanlyWhenAgentIsDown(t *testing.T) {
	f := newFixture(t)
	f.createAdmin(t, "admin", "pw")
	c := f.client(t)

	resp := post(t, c, f.srv.URL+"/api/auth/login",
		map[string]string{"username": "admin", "password": "pw"})
	decode(t, resp, nil)

	r, err := c.Get(f.srv.URL + "/api/system/services")
	if err != nil {
		t.Fatalf("GET services: %v", err)
	}
	var body struct{ Code string }
	decode(t, r, &body)
	if r.StatusCode != http.StatusServiceUnavailable || body.Code != "agent_unavailable" {
		t.Fatalf("agent down produced %d %q, want 503 agent_unavailable", r.StatusCode, body.Code)
	}
}

func TestUnknownRouteAndMethod(t *testing.T) {
	f := newFixture(t)
	resp, err := http.Get(f.srv.URL + "/api/does-not-exist")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown route returned %d", resp.StatusCode)
	}
	decode(t, resp, nil)

	resp, err = http.Get(f.srv.URL + "/api/auth/login")
	if err != nil {
		t.Fatalf("GET login: %v", err)
	}
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET on a POST-only route returned %d", resp.StatusCode)
	}
	decode(t, resp, nil)
}

func TestLoginRejectsUnknownField(t *testing.T) {
	f := newFixture(t)
	c := f.client(t)
	resp := post(t, c, f.srv.URL+"/api/auth/login",
		map[string]string{"username": "a", "password": "b", "passwrod": "typo"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown field returned %d, want 400", resp.StatusCode)
	}
	decode(t, resp, nil)
}
