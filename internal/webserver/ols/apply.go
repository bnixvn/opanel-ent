package ols

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"time"

	"github.com/bnixvn/opanel-ent/internal/platform/run"
	"github.com/bnixvn/opanel-ent/internal/webserver"
)

// Apply writes a render, validates it, and reloads.
//
// If validation or reload fails, the previous configuration is restored and
// reloaded again. A control panel that leaves a host serving nothing after a
// bad edit is worse than one that refuses the edit.
func (b *Backend) Apply(ctx context.Context, r webserver.Rendered) error {
	uid, gid, err := confOwnerIDs()
	if err != nil {
		return err
	}

	if err := ensureSuspendedPage(); err != nil {
		return err
	}

	if err := b.snapshot(); err != nil {
		return fmt.Errorf("ols: snapshot current config: %w", err)
	}

	if err := b.write(r, uid, gid); err != nil {
		// Nothing has been reloaded yet, but the files on disk are now a
		// mixture of old and new. Put the previous set back before returning.
		if rerr := b.restore(uid, gid); rerr != nil {
			return errors.Join(err, fmt.Errorf("ols: rollback also failed: %w", rerr))
		}
		return err
	}

	if err := b.TestConfig(ctx); err != nil {
		if rerr := b.rollback(ctx, uid, gid); rerr != nil {
			return errors.Join(err, rerr)
		}
		return err
	}

	if err := b.Reload(ctx); err != nil {
		if rerr := b.rollback(ctx, uid, gid); rerr != nil {
			return errors.Join(err, rerr)
		}
		return err
	}
	return nil
}

func (b *Backend) rollback(ctx context.Context, uid, gid int) error {
	if err := b.restore(uid, gid); err != nil {
		return fmt.Errorf("ols: rollback failed: %w", err)
	}
	if err := b.Reload(ctx); err != nil {
		return fmt.Errorf("ols: reload after rollback failed: %w", err)
	}
	return nil
}

// ensureSuspendedPage creates the shared notice a suspended site serves.
// Written on every apply so a deleted or edited page comes back.
func ensureSuspendedPage() error {
	if err := os.MkdirAll(webserver.SuspendedRoot, 0o755); err != nil {
		return fmt.Errorf("ols: create suspended page directory: %w", err)
	}
	if err := os.Chmod(webserver.SuspendedRoot, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(webserver.SuspendedRoot, "index.html"), suspendedPage, 0o644)
}

var suspendedPage = []byte(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Account suspended</title>
<style>
  body{font:16px/1.6 system-ui,-apple-system,Segoe UI,Roboto,sans-serif;
       margin:0;min-height:100vh;display:grid;place-items:center;
       background:#fafafa;color:#1a1a1a}
  main{max-width:32rem;padding:2rem;text-align:center}
  h1{font-size:1.35rem;margin:0 0 .5rem;font-weight:600}
  p{margin:0;color:#666}
</style>
</head>
<body>
<main>
  <h1>This site is temporarily unavailable</h1>
  <p>The account has been suspended. Please contact the hosting provider.</p>
</main>
</body>
</html>
`)

// write puts the rendered files in place and removes managed directories that
// the render no longer contains, which is how a deleted site's config goes
// away.
func (b *Backend) write(r webserver.Rendered, uid, gid int) error {
	keep := make(map[string]bool, len(r.VhostDirs))
	for _, d := range r.VhostDirs {
		keep[filepath.Clean(d)] = true
		if err := ensureDir(d, uid, gid); err != nil {
			return err
		}
	}
	if err := ensureDir(filepath.Join(b.managedDir, "vhosts"), uid, gid); err != nil {
		return err
	}

	for _, f := range r.Files() {
		if err := writeFileAtomic(f.Path, f.Content, f.Mode, uid, gid); err != nil {
			return err
		}
	}
	return b.prune(keep)
}

// prune deletes vhost directories for sites that no longer exist.
func (b *Backend) prune(keep map[string]bool) error {
	base := filepath.Join(b.managedDir, "vhosts")
	entries, err := os.ReadDir(base)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("ols: read %s: %w", base, err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		p := filepath.Join(base, e.Name())
		if keep[p] {
			continue
		}
		if err := os.RemoveAll(p); err != nil {
			return fmt.Errorf("ols: remove stale vhost dir %s: %w", p, err)
		}
	}
	return nil
}

// snapshot copies the current main config and managed tree aside so a failed
// apply can be undone.
func (b *Backend) snapshot() error {
	if err := os.RemoveAll(b.backupDir); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(b.backupDir, 0o750); err != nil {
		return err
	}
	if data, err := os.ReadFile(b.confPath); err == nil {
		if err := os.WriteFile(filepath.Join(b.backupDir, "httpd_config.conf"), data, fileMode); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return copyTree(filepath.Join(b.managedDir, "vhosts"), filepath.Join(b.backupDir, "vhosts"))
}

// restore puts the snapshot back.
func (b *Backend) restore(uid, gid int) error {
	main := filepath.Join(b.backupDir, "httpd_config.conf")
	data, err := os.ReadFile(main)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// No snapshot means this was the first apply; there is nothing
			// to go back to and saying so is more useful than a bare error.
			return errors.New("no previous configuration to restore")
		}
		return err
	}
	if err := writeFileAtomic(b.confPath, data, fileMode, uid, gid); err != nil {
		return err
	}
	live := filepath.Join(b.managedDir, "vhosts")
	if err := os.RemoveAll(live); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := copyTree(filepath.Join(b.backupDir, "vhosts"), live); err != nil {
		return err
	}
	return chownTree(b.managedDir, uid, gid)
}

// TestConfig asks OpenLiteSpeed to parse the configuration.
//
// The exit code is not the usual convention and was measured on the target
// host: 1 means the configuration is fine, 2 means it is not. Treating 0 as
// success would report every valid configuration as broken.
//
// A pass here is weaker than it looks. Unknown directives produce only a
// warning, so a misspelled directive parses cleanly and then does nothing at
// runtime; a duplicate listener is likewise accepted. Render's golden-file
// tests are what catch those.
func (b *Backend) TestConfig(ctx context.Context) error {
	res, err := run.Cmd(ctx, []string{binOLS, "-t"}, run.Timeout(60*time.Second), run.AllowExit(1, 2))
	if err != nil {
		return fmt.Errorf("ols: config test could not run: %w", err)
	}
	combined := res.Stdout + res.Stderr
	if res.ExitCode == 2 || bytes.Contains([]byte(combined), []byte("[ERROR]")) {
		return fmt.Errorf("ols: configuration rejected:\n%s", errorLines(combined))
	}
	return nil
}

// errorLines keeps only the lines worth showing an operator.
func errorLines(out string) string {
	var b bytes.Buffer
	for _, line := range bytes.Split([]byte(out), []byte("\n")) {
		if bytes.Contains(line, []byte("[ERROR]")) {
			b.Write(line)
			b.WriteByte('\n')
		}
	}
	if b.Len() == 0 {
		return out
	}
	return b.String()
}

// Reload applies configuration gracefully. OpenLiteSpeed's "restart" is a
// zero-downtime reload: workers finish their in-flight requests while new
// ones start on the new configuration.
func (b *Backend) Reload(ctx context.Context) error {
	_, err := run.Cmd(ctx, []string{binLSWSCtrl, "restart"}, run.Timeout(90*time.Second))
	if err != nil {
		return fmt.Errorf("ols: reload: %w", err)
	}
	return nil
}

// confOwnerIDs resolves the account OpenLiteSpeed reads configuration as.
func confOwnerIDs() (int, int, error) {
	u, err := user.Lookup(ConfOwner)
	if err != nil {
		return 0, 0, fmt.Errorf("ols: look up %q: %w", ConfOwner, err)
	}
	g, err := user.LookupGroup(ConfGroup)
	if err != nil {
		return 0, 0, fmt.Errorf("ols: look up group %q: %w", ConfGroup, err)
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return 0, 0, err
	}
	gid, err := strconv.Atoi(g.Gid)
	if err != nil {
		return 0, 0, err
	}
	return uid, gid, nil
}

func ensureDir(path string, uid, gid int) error {
	if err := os.MkdirAll(path, dirMode); err != nil {
		return fmt.Errorf("ols: create %s: %w", path, err)
	}
	if err := os.Chown(path, uid, gid); err != nil {
		return fmt.Errorf("ols: chown %s: %w", path, err)
	}
	if err := os.Chmod(path, dirMode); err != nil {
		return fmt.Errorf("ols: chmod %s: %w", path, err)
	}
	return nil
}

// writeFileAtomic writes through a temporary file in the same directory, so a
// reader never sees a half-written config and a crash mid-write leaves the
// previous file intact.
func writeFileAtomic(path string, content []byte, mode os.FileMode, uid, gid int) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".opanel-*")
	if err != nil {
		return fmt.Errorf("ols: temp file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chown(tmpName, uid, gid); err != nil {
		return fmt.Errorf("ols: chown %s: %w", tmpName, err)
	}
	if err := os.Chmod(tmpName, mode); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("ols: install %s: %w", path, err)
	}
	return nil
}

func copyTree(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("ols: %s is not a directory", src)
	}
	if err := os.MkdirAll(dst, dirMode); err != nil {
		return err
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range entries {
		s := filepath.Join(src, e.Name())
		d := filepath.Join(dst, e.Name())
		if e.IsDir() {
			if err := copyTree(s, d); err != nil {
				return err
			}
			continue
		}
		data, err := os.ReadFile(s)
		if err != nil {
			return err
		}
		if err := os.WriteFile(d, data, fileMode); err != nil {
			return err
		}
	}
	return nil
}

func chownTree(root string, uid, gid int) error {
	return filepath.Walk(root, func(p string, _ os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		return os.Chown(p, uid, gid)
	})
}
