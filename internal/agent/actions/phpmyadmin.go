package actions

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bnixvn/opanel-ent/internal/agent"
	"github.com/bnixvn/opanel-ent/internal/dbms"
	"github.com/bnixvn/opanel-ent/internal/platform/run"
)

// Where phpMyAdmin lives. Outside any customer's home, served by its own
// vhost, so one customer's PHP cannot read another's session files.
const (
	pmaRoot   = "/usr/share/phpmyadmin"
	pmaConfig = pmaRoot + "/config.inc.php"
	pmaSignon = pmaRoot + "/opanel-sso.php"
	// Not under /var/lib/opanel: that directory is 0750 and owned by the
	// panel, so the webserver account cannot traverse into it, and
	// phpMyAdmin then reports an unusable temp directory on every page it
	// draws. /var/cache is world-traversable, which is what this needs.
	pmaTmpDir  = "/var/cache/opanel-phpmyadmin"
	pmaVersion = "5.2.2"
	pmaURL     = "https://files.phpmyadmin.net/phpMyAdmin/" + pmaVersion +
		"/phpMyAdmin-" + pmaVersion + "-all-languages.tar.gz"
	pmaSHA256URL = pmaURL + ".sha256"
)

// PMAStatus reports what is installed.
type PMAStatus struct {
	Installed bool   `json:"installed"`
	Version   string `json:"version,omitempty"`
	Root      string `json:"root"`
}

// PMASignonRequest creates a throwaway MariaDB account for one sign-in.
type PMASignonRequest struct {
	// Prefix is the owner's username, used to build a recognisable account
	// name so an operator watching the process list can tell whose it is.
	Prefix string `json:"prefix"`
	// Databases are the ones the account may reach.
	Databases []string `json:"databases"`
}

// Validate checks the prefix and every database name.
func (r *PMASignonRequest) Validate() error {
	if !dbms.ValidUserName(r.Prefix) {
		return fmt.Errorf("%q is not an acceptable account prefix", r.Prefix)
	}
	for _, d := range r.Databases {
		if !dbms.ValidDatabaseName(d) {
			return fmt.Errorf("%q is not an acceptable database name", d)
		}
	}
	return nil
}

// PMASignonResult is the throwaway account.
type PMASignonResult struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// PMADropRequest removes throwaway accounts.
type PMADropRequest struct {
	Usernames []string `json:"usernames"`
}

// Validate checks each name.
func (r *PMADropRequest) Validate() error {
	for _, u := range r.Usernames {
		if !dbms.ValidUserName(u) {
			return fmt.Errorf("%q is not an acceptable account name", u)
		}
	}
	return nil
}

func registerPMA(r *agent.Registry) {
	agent.Register(r, "pma.status", 1, func(_ context.Context, _ struct{}) (PMAStatus, error) {
		return pmaStatus(), nil
	})

	agent.RegisterSlow(r, "pma.install", 1, 15*time.Minute,
		func(ctx context.Context, _ struct{}) (PMAStatus, error) {
			if err := installPMA(ctx); err != nil {
				return PMAStatus{}, err
			}
			return pmaStatus(), nil
		})

	agent.Register(r, "pma.signon", 1, func(ctx context.Context, in PMASignonRequest) (PMASignonResult, error) {
		return createSignonAccount(ctx, in)
	})

	agent.Register(r, "pma.drop", 1, func(ctx context.Context, in PMADropRequest) (struct{}, error) {
		for _, u := range in.Usernames {
			// IF EXISTS: the sweep runs on a timer and may see the same
			// account twice if a previous run was interrupted.
			if _, err := run.Cmd(ctx, []string{
				"mariadb", "-e", "DROP USER IF EXISTS " + quoteUser(u),
			}, run.Timeout(time.Minute)); err != nil {
				return struct{}{}, err
			}
		}
		return struct{}{}, nil
	})
}

func pmaStatus() PMAStatus {
	st := PMAStatus{Root: pmaRoot}
	if _, err := os.Stat(filepath.Join(pmaRoot, "index.php")); err != nil {
		return st
	}
	st.Installed = true
	if v, err := os.ReadFile(filepath.Join(pmaRoot, "opanel-version")); err == nil {
		st.Version = strings.TrimSpace(string(v))
	}
	return st
}

// installPMA downloads phpMyAdmin and writes a configuration that only the
// panel can sign in to.
func installPMA(ctx context.Context) error {
	tmp, err := os.MkdirTemp("", "pma-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(tmp) }()

	archive := filepath.Join(tmp, "pma.tar.gz")
	if _, err := run.Cmd(ctx, []string{
		"curl", "-fsSL", "--max-time", "600", "-o", archive, pmaURL,
	}, run.Timeout(11*time.Minute)); err != nil {
		return fmt.Errorf("download phpMyAdmin: %w", err)
	}

	// The published checksum comes from the same origin as the tarball, so
	// this catches a truncated download rather than a compromised mirror --
	// which is still the failure that actually happens.
	want, err := run.Cmd(ctx, []string{"curl", "-fsSL", "--max-time", "60", pmaSHA256URL},
		run.Timeout(2*time.Minute))
	if err != nil {
		return fmt.Errorf("fetch the phpMyAdmin checksum: %w", err)
	}
	got, err := run.Cmd(ctx, []string{"sha256sum", archive})
	if err != nil {
		return err
	}
	wantSum := strings.Fields(want.Stdout)
	gotSum := strings.Fields(got.Stdout)
	if len(wantSum) == 0 || len(gotSum) == 0 || !strings.EqualFold(wantSum[0], gotSum[0]) {
		return errors.New("the phpMyAdmin download does not match its published checksum")
	}

	if _, err := run.Cmd(ctx, []string{"tar", "xzf", archive, "-C", tmp},
		run.Timeout(5*time.Minute)); err != nil {
		return fmt.Errorf("unpack phpMyAdmin: %w", err)
	}
	extracted := filepath.Join(tmp, "phpMyAdmin-"+pmaVersion+"-all-languages")
	if _, err := os.Stat(extracted); err != nil {
		return fmt.Errorf("the archive did not contain %s", filepath.Base(extracted))
	}

	// Replaced wholesale: an upgrade that merged into the old tree would
	// leave files from the previous version behind, and phpMyAdmin has had
	// vulnerabilities in files that were supposed to be gone.
	_ = os.RemoveAll(pmaRoot)
	if err := os.MkdirAll(filepath.Dir(pmaRoot), 0o755); err != nil {
		return err
	}
	if _, err := run.Cmd(ctx, []string{"cp", "-a", extracted, pmaRoot}); err != nil {
		return fmt.Errorf("install phpMyAdmin: %w", err)
	}
	if err := os.WriteFile(filepath.Join(pmaRoot, "opanel-version"),
		[]byte(pmaVersion+"\n"), 0o644); err != nil {
		return err
	}

	// Its own temp directory, owned by the webserver, so session and
	// template files never land in a shared /tmp where another account
	// could read them.
	if err := os.MkdirAll(pmaTmpDir, 0o700); err != nil {
		return err
	}
	if err := chownToWebserver(pmaTmpDir); err != nil {
		return err
	}
	// Proved rather than assumed: every symptom of an unreachable temp
	// directory appears inside phpMyAdmin, where the cause is invisible.
	if err := checkWebserverCanWrite(pmaTmpDir); err != nil {
		return err
	}

	secret, err := randomSecret(32)
	if err != nil {
		return err
	}
	if err := os.WriteFile(pmaConfig, []byte(pmaConfigFile(secret)), 0o640); err != nil {
		return fmt.Errorf("write %s: %w", pmaConfig, err)
	}
	if err := chownToWebserver(pmaConfig); err != nil {
		return err
	}
	if err := os.WriteFile(pmaSignon, []byte(signonScript), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", pmaSignon, err)
	}

	// setup/ writes configuration from the browser. The panel writes the
	// configuration, so this is only an attack surface.
	_ = os.RemoveAll(filepath.Join(pmaRoot, "setup"))
	return nil
}

// createSignonAccount mints a throwaway MariaDB account for one session.
//
// The panel cannot re-use the customer's own account because it never keeps
// their password -- only MariaDB has the hash. Minting one is better than
// storing passwords anyway: it is scoped to exactly the databases the
// customer owns and it disappears on its own.
func createSignonAccount(ctx context.Context, in PMASignonRequest) (PMASignonResult, error) {
	suffix, err := randomSecret(6)
	if err != nil {
		return PMASignonResult{}, err
	}
	username := "pma_" + in.Prefix + "_" + strings.ToLower(suffix)
	if len(username) > dbms.MaxUserName {
		username = username[:dbms.MaxUserName]
	}
	if !dbms.ValidUserName(username) {
		return PMASignonResult{}, fmt.Errorf("generated an unusable account name %q", username)
	}
	password, err := randomSecret(24)
	if err != nil {
		return PMASignonResult{}, err
	}

	stmts := []string{
		fmt.Sprintf("CREATE USER %s IDENTIFIED BY %s", quoteUser(username), quoteLiteral(password)),
	}
	for _, d := range in.Databases {
		stmts = append(stmts,
			fmt.Sprintf("GRANT ALL PRIVILEGES ON %s.* TO %s", quoteIdent(d), quoteUser(username)))
	}
	// No global grants at all: this account can see its own databases and
	// nothing else, which is what phpMyAdmin will show.
	for _, stmt := range stmts {
		if _, err := run.Cmd(ctx, []string{"mariadb", "-e", stmt}, run.Timeout(time.Minute)); err != nil {
			_, _ = run.Cmd(ctx, []string{"mariadb", "-e", "DROP USER IF EXISTS " + quoteUser(username)})
			return PMASignonResult{}, fmt.Errorf("create the sign-in account: %w", err)
		}
	}
	return PMASignonResult{Username: username, Password: password}, nil
}

// randomSecret returns URL-safe random text.
func randomSecret(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	// Only letters and digits: the value becomes part of a MariaDB account
	// name in one use and a configuration literal in another.
	s := base64.RawURLEncoding.EncodeToString(b)
	s = strings.NewReplacer("-", "", "_", "").Replace(s)
	if len(s) < n {
		return randomSecret(n)
	}
	return s[:n], nil
}

func quoteUser(u string) string  { return "'" + u + "'@'localhost'" }
func quoteIdent(s string) string { return "`" + s + "`" }
func quoteLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// chownToWebserver hands a path to the account phpMyAdmin's interpreter runs
// as.
//
// That is the pool's user, not the webserver's. This used to say "nobody",
// which was the account OpenLiteSpeed ran as -- and OpenLiteSpeed was removed
// some time ago. Nothing noticed until phpMyAdmin was installed on a host
// where the two differ: the configuration was handed to nobody, the pool runs
// as opanel, and phpMyAdmin greeted every visitor with "Existing
// configuration file is not readable".
func chownToWebserver(p string) error {
	// The account that reads the configuration is the one that runs the code
	// in it, and it is the only account that should be able to: the file
	// holds the signon secret.
	res, err := run.Cmd(context.Background(), []string{"id", "-u", pmaPoolUser})
	if err != nil {
		return err
	}
	uid := strings.TrimSpace(res.Stdout)
	_, err = run.Cmd(context.Background(), []string{"chown", "-R", uid + ":" + uid, p})
	return err
}

// PMAPort is the loopback port the phpMyAdmin vhost listens on. Nothing
// outside the machine can reach it; the panel proxies after checking the
// session.
const PMAPort = 8081

// PMAPHPVersion is the interpreter phpMyAdmin runs under.
//
// Pinned rather than following a site's choice: phpMyAdmin is the panel's
// own software, and it should not stop working because a customer moved
// their site to an older PHP.
const PMAPHPVersion = "8.4"

// checkWebserverCanWrite proves the webserver account can actually use a
// directory, by being that account for one write.
//
// Ownership is not enough: a directory owned by the right account inside one
// the account cannot traverse is unreachable, and every symptom of that
// appears inside phpMyAdmin rather than here.
func checkWebserverCanWrite(dir string) error {
	probe := filepath.Join(dir, ".opanel-write-test")
	if _, err := run.Cmd(context.Background(), []string{
		"runuser", "-u", pmaPoolUser, "--", "touch", probe,
	}, run.Timeout(30*time.Second)); err != nil {
		return fmt.Errorf("%s is not usable by %q, the account phpMyAdmin's "+
			"interpreter runs as; it needs a temp directory it can write to", dir, pmaPoolUser)
	}
	_ = os.Remove(probe)
	return nil
}
