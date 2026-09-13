// Package linuxuser creates and removes the Linux accounts that own sites.
//
// Each panel user of role end_user gets one Linux account. PHP for that
// user's sites runs as it, so the account boundary is what separates one
// customer's files from another's.
package linuxuser

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path"
	"regexp"
	"slices"
	"strconv"
	"time"

	"github.com/bnixvn/opanel-ent/internal/platform/run"
)

// HomeBase is where site accounts get their home directory.
const HomeBase = "/home"

// Shell is what a site account gets. Site owners reach the server over SFTP,
// not an interactive shell; giving them one would be an unnecessary grant.
const Shell = "/sbin/nologin"

// SFTPGroup is the supplementary group whose members are chrooted into their
// home by the sshd configuration the installer writes.
const SFTPGroup = "opanel-sftp"

// namePattern is stricter than useradd's own rules. Leading digits, dots and
// trailing dollar signs are legal in some tools and cause trouble in others,
// so the panel accepts only the conservative subset.
var namePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{2,31}$`)

// reserved lists names that must never be handed to a customer. Taking one
// over would let a site's PHP run as a system account.
var reserved = []string{
	"root", "bin", "daemon", "adm", "lp", "sync", "shutdown", "halt", "mail",
	"operator", "games", "ftp", "nobody", "systemd-network", "dbus", "polkitd",
	"sshd", "chrony", "mysql", "mariadb", "valkey", "redis", "clamav", "apache",
	"nginx", "www-data", "lsadm", "opanel", "opanel-web", "postfix", "named",
	"admin", "test", "user", "guest",
}

// ValidName reports whether n may be used for a site account.
func ValidName(n string) bool {
	return namePattern.MatchString(n) && !slices.Contains(reserved, n)
}

// ErrExists means the account is already present.
var ErrExists = errors.New("linuxuser: account already exists")

// Account describes a provisioned Linux user.
type Account struct {
	Username string `json:"username"`
	UID      int64  `json:"uid"`
	GID      int64  `json:"gid"`
	Home     string `json:"home"`
}

// Home returns the home directory a given account name would get.
func Home(username string) string { return path.Join(HomeBase, username) }

// Lookup returns an existing account, or nil when it does not exist.
func Lookup(username string) (*Account, error) {
	if !ValidName(username) {
		return nil, fmt.Errorf("linuxuser: %q is not an acceptable account name", username)
	}
	u, err := user.Lookup(username)
	var unknown user.UnknownUserError
	if errors.As(err, &unknown) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	uid, err := strconv.ParseInt(u.Uid, 10, 64)
	if err != nil {
		return nil, err
	}
	gid, err := strconv.ParseInt(u.Gid, 10, 64)
	if err != nil {
		return nil, err
	}
	return &Account{Username: u.Username, UID: uid, GID: gid, Home: u.HomeDir}, nil
}

// Create provisions an account with its own primary group and a home
// directory. It is idempotent: an existing account is returned unchanged.
func Create(ctx context.Context, username string) (*Account, error) {
	if !ValidName(username) {
		return nil, fmt.Errorf("linuxuser: %q is not an acceptable account name", username)
	}
	if acct, err := Lookup(username); err != nil {
		return nil, err
	} else if acct != nil {
		return acct, nil
	}

	if err := ensureGroup(ctx, SFTPGroup); err != nil {
		return nil, err
	}

	argv := []string{
		"useradd",
		"--create-home",
		"--home-dir", Home(username),
		"--shell", Shell,
		"--user-group",
		"--groups", SFTPGroup,
		"--comment", "OPanel site owner",
		username,
	}
	if _, err := run.Cmd(ctx, argv, run.Timeout(60*time.Second)); err != nil {
		return nil, fmt.Errorf("linuxuser: create %q: %w", username, err)
	}

	acct, err := Lookup(username)
	if err != nil {
		return nil, err
	}
	if acct == nil {
		return nil, fmt.Errorf("linuxuser: %q was not present after useradd", username)
	}

	// The home directory has to satisfy three parties at once:
	//
	//   sshd     ChrootDirectory requires every component to be owned by root
	//            and writable by no one else, or it refuses the session with
	//            "bad ownership or modes for chroot directory".
	//   the      Its worker runs as its own account, not as the site owner, so
	//   webserver it must be able to traverse in.
	//   the user They must not be able to replace the directory that chroots
	//            them, which is exactly what root ownership prevents.
	//
	// root:<user> 0751 satisfies all three. Root-owned with no group or other
	// write keeps sshd happy; the group bit is r-x so the owner can still list
	// their own home over SFTP, which 0711 refused with "permission denied" on
	// the very first directory listing; other keeps --x so the webserver can
	// traverse without reading. The user owns everything inside -- the site
	// directories the panel creates and the skeleton files useradd left -- so
	// they lose nothing but the ability to write in the home root itself.
	if err := os.Chown(acct.Home, 0, int(acct.GID)); err != nil {
		return nil, fmt.Errorf("linuxuser: chown home: %w", err)
	}
	if err := os.Chmod(acct.Home, 0o751); err != nil {
		return nil, fmt.Errorf("linuxuser: chmod home: %w", err)
	}
	return acct, nil
}

// Delete removes an account. The home directory goes with it only when
// removeHome is set, so an accidental delete does not destroy customer data.
func Delete(ctx context.Context, username string, removeHome bool) error {
	if !ValidName(username) {
		return fmt.Errorf("linuxuser: %q is not an acceptable account name", username)
	}
	acct, err := Lookup(username)
	if err != nil {
		return err
	}
	if acct == nil {
		return nil
	}
	argv := []string{"userdel"}
	if removeHome {
		argv = append(argv, "--remove")
	}
	argv = append(argv, username)
	if _, err := run.Cmd(ctx, argv, run.Timeout(120*time.Second)); err != nil {
		return fmt.Errorf("linuxuser: delete %q: %w", username, err)
	}
	return nil
}

// SetPassword sets the account password, used for SFTP access.
func SetPassword(ctx context.Context, username, password string) error {
	if !ValidName(username) {
		return fmt.Errorf("linuxuser: %q is not an acceptable account name", username)
	}
	if password == "" {
		return errors.New("linuxuser: password must not be empty")
	}
	// chpasswd reads "user:password" on stdin, which keeps the password out
	// of the process table where a command-line argument would show it.
	_, err := run.Cmd(ctx, []string{"chpasswd"},
		run.Stdin(username+":"+password+"\n"), run.Timeout(30*time.Second))
	if err != nil {
		return fmt.Errorf("linuxuser: set password for %q: %w", username, err)
	}
	return nil
}

// ensureGroup creates a system group when it is missing.
func ensureGroup(ctx context.Context, name string) error {
	if _, err := user.LookupGroup(name); err == nil {
		return nil
	}
	if _, err := run.Cmd(ctx, []string{"groupadd", "--system", name}, run.Timeout(30*time.Second)); err != nil {
		return fmt.Errorf("linuxuser: create group %q: %w", name, err)
	}
	return nil
}
