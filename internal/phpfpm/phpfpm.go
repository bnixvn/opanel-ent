// Package phpfpm writes PHP-FPM pools and keeps their services in step.
//
// One pool per site, running as that site's own Linux account. That is the
// whole security argument for this shape: a shared interpreter would run
// every customer's code as one user, and file permissions would then be the
// only thing between them -- which is not enough, because their code chooses
// what to open.
//
// It sits beside the webserver backend rather than inside it. Apache and
// LiteSpeed Enterprise both proxy to these sockets, so pools survive a switch
// between the two untouched; only OpenLiteSpeed, which spawns its own
// interpreter, has no use for them.
package phpfpm

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/template"

	"github.com/bnixvn/opanel-ent/internal/phpmgr"
	"github.com/bnixvn/opanel-ent/internal/platform/svc"
	"github.com/bnixvn/opanel-ent/internal/webserver"
)

// Pool is one site's interpreter.
type Pool struct {
	// Name is the pool's identity: the site's domain, which is already
	// unique and already safe as a filename.
	Name        string
	Version     string
	User        string
	Group       string
	Socket      string
	MaxChildren int
	// LogDir is where this pool writes PHP's own errors. The site's log
	// directory, not a shared one under /var/log: the customer can then read
	// their own PHP errors through the file manager, and the space those
	// logs take counts against the account that produced them.
	LogDir string
	// SocketOwner is the account the webserver runs as. It owns the socket;
	// the pool behind it runs as the site owner. That direction is the whole
	// arrangement: the server may connect, and the site's code may not reach
	// another site's pool.
	SocketOwner string
	// Settings are the site's php.ini overrides, already checked against
	// internal/phpini by the caller.
	Settings map[string]string
}

// PoolsFor turns rendered sites into the pools they need.
//
// Sites that do not run PHP get none, and a suspended site gets none either:
// its vhost serves a notice page, so a pool for it would be idle processes
// held open for a customer who is not being served.
func PoolsFor(p phpmgr.FPMProvider, sites []webserver.Site, serverUser string, memMB int) []Pool {
	out := make([]Pool, 0, len(sites))
	for _, s := range sites {
		if !s.NeedsPHP() || s.Suspended || s.FPMSocket == "" {
			continue
		}
		out = append(out, Pool{
			Name:        s.Domain,
			Version:     s.PHPVersion,
			User:        s.OwnerUser,
			Group:       s.OwnerGroup,
			Socket:      p.SocketPath(s.PHPVersion, s.Domain),
			MaxChildren: maxChildren(memMB),
			LogDir:      s.LogDir(),
			SocketOwner: serverUser,
			Settings:    s.PHPSettings,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// maxChildren caps one site's interpreter processes.
//
// Sized against the host rather than fixed, and deliberately not generous:
// on ondemand these are an upper bound, not a reservation, and the number
// that matters on a shared box is how much one busy site can take before the
// others notice. Roughly 32 MB per PHP process, a quarter of the machine to
// any single site, and never fewer than 5 or the first burst queues.
func maxChildren(memMB int) int {
	n := memMB / 4 / 32
	switch {
	case n < 5:
		return 5
	case n > 50:
		return 50
	default:
		return n
	}
}

//go:embed pool.conf.tmpl
var poolTemplate string

var tmpl = template.Must(template.New("pool").Parse(poolTemplate))

// Render produces the configuration file for one pool.
func Render(p phpmgr.FPMProvider, pool Pool) (webserver.File, error) {
	if !phpmgr.ValidVersion(pool.Version) {
		return webserver.File{}, fmt.Errorf("phpfpm: pool %q has a malformed php version %q", pool.Name, pool.Version)
	}
	if !webserver.ValidDomain(pool.Name) {
		return webserver.File{}, fmt.Errorf("phpfpm: %q is not usable as a pool name", pool.Name)
	}
	if !webserver.ValidUnixName(pool.User) || !webserver.ValidUnixName(pool.Group) {
		return webserver.File{}, fmt.Errorf("phpfpm: pool %q has an unusable owner", pool.Name)
	}
	if pool.LogDir == "" {
		return webserver.File{}, fmt.Errorf("phpfpm: pool %q has no log directory", pool.Name)
	}
	if !webserver.ValidUnixName(pool.SocketOwner) {
		return webserver.File{}, fmt.Errorf("phpfpm: pool %q has an unusable socket owner %q",
			pool.Name, pool.SocketOwner)
	}
	// Settings reach the file verbatim, as they do in a vhost, so the same
	// rule applies: nothing that could close the directive and open another.
	keys := make([]string, 0, len(pool.Settings))
	for k, v := range pool.Settings {
		if strings.ContainsAny(k, "\r\n[]=") || strings.ContainsAny(v, "\r\n[]") {
			return webserver.File{}, fmt.Errorf("phpfpm: pool %q has an unusable php setting %q", pool.Name, k)
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, struct {
		Pool
		Keys      []string
		SocketDir string
	}{Pool: pool, Keys: keys, SocketDir: filepath.Dir(pool.Socket)}); err != nil {
		return webserver.File{}, fmt.Errorf("phpfpm: render pool %q: %w", pool.Name, err)
	}
	return webserver.File{Path: p.PoolFile(pool.Version, pool.Name), Content: buf.Bytes(), Mode: 0o644}, nil
}

// Apply writes every pool, removes the ones that are gone, and restarts the
// versions whose files changed.
//
// Restart rather than reload: PHP-FPM's reload re-reads pool files, but a
// pool whose socket path or user changed needs the old processes gone, and
// telling the two cases apart is more machinery than a two-second restart of
// a service that is idle most of the time.
func Apply(ctx context.Context, p phpmgr.FPMProvider, pools []Pool) error {
	if err := ensureSocketDir(pools); err != nil {
		return err
	}
	for _, pool := range pools {
		if err := ensureLogDir(pool); err != nil {
			return err
		}
	}
	if err := dropVendorWants(ctx, p); err != nil {
		return err
	}

	byVersion := make(map[string][]Pool)
	for _, pool := range pools {
		byVersion[pool.Version] = append(byVersion[pool.Version], pool)
	}

	changed := make(map[string]bool)
	for _, version := range p.Supported() {
		dir := p.PoolDir(version)
		if _, err := os.Stat(dir); err != nil {
			// The version is not installed. A pool asking for it is a
			// caller-side mistake worth reporting rather than ignoring.
			if len(byVersion[version]) > 0 {
				return fmt.Errorf("phpfpm: php %s is not installed but %d site(s) ask for it",
					version, len(byVersion[version]))
			}
			continue
		}

		wanted := make(map[string][]byte, len(byVersion[version]))
		for _, pool := range byVersion[version] {
			f, err := Render(p, pool)
			if err != nil {
				return err
			}
			wanted[filepath.Base(f.Path)] = f.Content
		}

		dirty, err := syncDir(dir, wanted)
		if err != nil {
			return err
		}
		if dirty || !socketsMatch(byVersion[version]) {
			changed[version] = true
		}
	}

	for _, version := range p.Supported() {
		unit := p.ServiceUnit(version)
		switch {
		case len(byVersion[version]) == 0:
			// Nothing uses this version. Stopping it is not tidiness: each
			// idle pool manager holds a master process and its children, and
			// seven installed versions all running is most of a gigabyte
			// spent serving nobody.
			if st, err := svc.Get(ctx, unit); err == nil && st.Running() {
				if err := svc.Stop(ctx, unit); err != nil {
					return fmt.Errorf("phpfpm: stop %s: %w", unit, err)
				}
			}
		case changed[version]:
			if err := svc.Enable(ctx, unit, false); err != nil {
				return fmt.Errorf("phpfpm: enable %s: %w", unit, err)
			}
			if err := svc.Restart(ctx, unit); err != nil {
				return fmt.Errorf("phpfpm: restart %s: %w", unit, err)
			}
		default:
			if st, err := svc.Get(ctx, unit); err == nil && !st.Running() {
				if err := svc.Start(ctx, unit); err != nil {
					return fmt.Errorf("phpfpm: start %s: %w", unit, err)
				}
			}
		}
	}
	return nil
}

// ensureSocketDir creates the directory the sockets live in, traversable by
// the webserver and by nobody else.
//
// The mode is what matters here. A socket is only as private as the path to
// it, and these are 0660 owned by the webserver -- but on a host where
// customers have shell accounts, a world-traversable directory plus one
// mistake in a socket mode is a way to run code as another customer. 0710
// means the question never comes up.
func ensureSocketDir(pools []Pool) error {
	dir := phpmgr.FPMSocketDir
	if err := os.MkdirAll(dir, 0o710); err != nil {
		return fmt.Errorf("phpfpm: create %s: %w", dir, err)
	}
	owner := ""
	for _, p := range pools {
		owner = p.SocketOwner
		break
	}
	if owner != "" {
		g, err := user.LookupGroup(owner)
		if err != nil {
			return fmt.Errorf("phpfpm: look up group %q: %w", owner, err)
		}
		gid, err := strconv.Atoi(g.Gid)
		if err != nil {
			return err
		}
		if err := os.Chown(dir, 0, gid); err != nil {
			return fmt.Errorf("phpfpm: chown %s: %w", dir, err)
		}
	}
	return os.Chmod(dir, 0o710)
}

// dropVendorWants removes the drop-ins that tie every installed PHP version
// to the webserver's own unit.
//
// Remi's packages install /etc/systemd/system/httpd.service.d/phpNN-php-fpm.conf,
// which makes httpd want that version's pool manager. It is a sensible default
// for a server with one PHP version and wrong here: the panel decides which
// versions run, and a version with no pools cannot start at all -- it exits
// with "No pool defined". The result is that starting Apache leaves a row of
// failed services behind, one for every version nobody is using.
func dropVendorWants(ctx context.Context, p phpmgr.FPMProvider) error {
	removed := false
	for _, version := range p.Supported() {
		unit := p.ServiceUnit(version)
		for _, server := range []string{"httpd", "nginx"} {
			path := filepath.Join("/etc/systemd/system", server+".service.d", unit+".conf")
			if err := os.Remove(path); err == nil {
				removed = true
			} else if !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("phpfpm: remove %s: %w", path, err)
			}
		}
	}
	if !removed {
		return nil
	}
	return svc.DaemonReload(ctx)
}

// ensureLogDir makes sure the pool can open its own log files.
//
// PHP-FPM refuses to start when it cannot create its slow log, and the error
// it gives names the file rather than the directory -- so a missing directory
// reads as a broken pool. Created here, owned by the account the pool runs
// as, because that account is what will be writing to it.
func ensureLogDir(pool Pool) error {
	if err := os.MkdirAll(pool.LogDir, 0o751); err != nil {
		return fmt.Errorf("phpfpm: create %s: %w", pool.LogDir, err)
	}
	u, err := user.Lookup(pool.User)
	if err != nil {
		return fmt.Errorf("phpfpm: look up %q: %w", pool.User, err)
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return err
	}
	gid, err := strconv.Atoi(u.Gid)
	if err != nil {
		return err
	}
	if err := os.Chown(pool.LogDir, uid, gid); err != nil {
		return fmt.Errorf("phpfpm: chown %s: %w", pool.LogDir, err)
	}
	return nil
}

// syncDir makes one version's pool directory match wanted, and reports
// whether anything actually changed.
//
// Only files the panel owns are touched: everything it writes is prefixed,
// and the packaged www.conf is the one file in here that is not. That one is
// removed rather than left -- it defines a pool running as the webserver's
// own account with no site behind it, which is both idle processes and a way
// to execute PHP outside every account boundary this panel draws.
func syncDir(dir string, wanted map[string][]byte) (bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false, fmt.Errorf("phpfpm: read %s: %w", dir, err)
	}
	changed := false
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".conf") {
			continue
		}
		if name == "www.conf" {
			if err := os.Remove(filepath.Join(dir, name)); err != nil {
				return false, fmt.Errorf("phpfpm: remove %s: %w", name, err)
			}
			changed = true
			continue
		}
		if !strings.HasPrefix(name, "opanel-") || wanted[name] != nil {
			continue
		}
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			return false, fmt.Errorf("phpfpm: remove stale pool %s: %w", name, err)
		}
		changed = true
	}

	for name, content := range wanted {
		p := filepath.Join(dir, name)
		if old, err := os.ReadFile(p); err == nil && bytes.Equal(old, content) {
			continue
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
		if err := os.WriteFile(p, content, 0o644); err != nil {
			return false, fmt.Errorf("phpfpm: write %s: %w", p, err)
		}
		changed = true
	}
	return changed, nil
}
