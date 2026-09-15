package apache

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bnixvn/opanel-ent/internal/platform/run"
	"github.com/bnixvn/opanel-ent/internal/platform/svc"
	"github.com/bnixvn/opanel-ent/internal/webserver"
)

// Apply writes a render, validates it, and reloads.
//
// If validation or reload fails, the previous configuration is restored and
// reloaded again. A control panel that leaves a host serving nothing after a
// bad edit is worse than one that refuses the edit.
func (b *Backend) Apply(ctx context.Context, r webserver.Rendered) error {
	if err := webserver.EnsureSharedRoots(); err != nil {
		return err
	}
	if err := ensureDistroTLSCert(ctx); err != nil {
		return err
	}
	if err := b.ensureUnitOverride(ctx); err != nil {
		return err
	}
	if err := b.ensureApacheMode(); err != nil {
		return err
	}
	if err := b.snapshot(); err != nil {
		return fmt.Errorf("apache: snapshot current config: %w", err)
	}

	if err := b.write(r); err != nil {
		// Nothing has been reloaded yet, but the files on disk are now a
		// mixture of old and new. Put the previous set back before returning.
		if rerr := b.restore(); rerr != nil {
			return errors.Join(err, fmt.Errorf("apache: rollback also failed: %w", rerr))
		}
		return err
	}

	if err := b.TestConfig(ctx); err != nil {
		if rerr := b.rollback(ctx); rerr != nil {
			return errors.Join(err, rerr)
		}
		return err
	}

	if err := b.Reload(ctx); err != nil {
		if rerr := b.rollback(ctx); rerr != nil {
			return errors.Join(err, rerr)
		}
		return err
	}
	return nil
}

func (b *Backend) rollback(ctx context.Context) error {
	if err := b.restore(); err != nil {
		return fmt.Errorf("apache: rollback failed: %w", err)
	}
	if err := b.Reload(ctx); err != nil {
		return fmt.Errorf("apache: reload after rollback failed: %w", err)
	}
	return nil
}

// write puts the rendered files in place and removes managed files the render
// no longer contains, which is how a deleted site's config goes away.
func (b *Backend) write(r webserver.Rendered) error {
	if err := os.MkdirAll(filepath.Join(b.managedDir, "vhosts"), dirMode); err != nil {
		return fmt.Errorf("apache: create %s: %w", b.managedDir, err)
	}
	keep := make(map[string]bool, len(r.Vhosts))
	for _, f := range r.Files() {
		if err := writeFileAtomic(f.Path, f.Content, f.Mode); err != nil {
			return err
		}
		keep[filepath.Base(f.Path)] = true
	}
	return b.prune(keep)
}

// prune deletes vhost files for sites that no longer exist.
//
// Scoped to the panel's own directory and to *.conf. Apache's include is a
// glob over a directory nothing else writes to, so "not in this render" is
// the same as "not a site any more".
func (b *Backend) prune(keep map[string]bool) error {
	base := filepath.Join(b.managedDir, "vhosts")
	entries, err := os.ReadDir(base)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("apache: read %s: %w", base, err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".conf") || keep[e.Name()] {
			continue
		}
		if err := os.Remove(filepath.Join(base, e.Name())); err != nil {
			return fmt.Errorf("apache: remove stale vhost %s: %w", e.Name(), err)
		}
	}
	return nil
}

// snapshot copies the current configuration aside so a failed apply can be
// undone.
func (b *Backend) snapshot() error {
	if err := os.RemoveAll(b.backupDir); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(b.backupDir, 0o750); err != nil {
		return err
	}
	if data, err := os.ReadFile(b.confPath); err == nil {
		if err := os.WriteFile(filepath.Join(b.backupDir, "opanel.conf"), data, fileMode); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return copyTree(filepath.Join(b.managedDir, "vhosts"), filepath.Join(b.backupDir, "vhosts"))
}

// restore puts the snapshot back.
func (b *Backend) restore() error {
	data, err := os.ReadFile(filepath.Join(b.backupDir, "opanel.conf"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// No snapshot means this was the first apply; there is nothing
			// to go back to, and saying so is more useful than a bare error.
			return errors.New("no previous configuration to restore")
		}
		return err
	}
	if err := writeFileAtomic(b.confPath, data, fileMode); err != nil {
		return err
	}
	live := filepath.Join(b.managedDir, "vhosts")
	if err := os.RemoveAll(live); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return copyTree(filepath.Join(b.backupDir, "vhosts"), live)
}

// unitOverride lets Apache serve sites that live under /home.
//
// The distribution's unit sets ProtectHome=read-only, which is right for a
// server whose content is in /var/www and wrong for a hosting panel, where
// every site is /home/<account>/<domain>. Without this, Apache cannot open a
// site's log file and refuses to start -- with an error that names the log
// file and says "Read-only file system", which sends you looking at
// permissions that are perfectly correct.
//
// Only that one restriction is lifted. The rest of the unit's sandboxing is
// worth having and stays.
const (
	unitOverrideDir  = "/etc/systemd/system/httpd.service.d"
	unitOverridePath = unitOverrideDir + "/opanel.conf"
)

var unitOverride = []byte(`# Written by OPanel. Do not edit.
#
# Sites live under /home, and the packaged unit mounts /home read-only inside
# the service. Apache cannot open a site's log file, let alone let PHP write
# an upload. Every other restriction the unit sets is left in place.
[Service]
ProtectHome=no
`)

func (b *Backend) ensureUnitOverride(ctx context.Context) error {
	// Only Apache's own unit. LiteSpeed Enterprise ships its own, without
	// this restriction, and writing a drop-in for a unit that does not need
	// one would be one more thing to explain to whoever finds it.
	if b.unit != "httpd" {
		return nil
	}
	if old, err := os.ReadFile(unitOverridePath); err == nil && bytes.Equal(old, unitOverride) {
		return nil
	}
	if err := os.MkdirAll(unitOverrideDir, 0o755); err != nil {
		return fmt.Errorf("apache: create %s: %w", unitOverrideDir, err)
	}
	if err := os.WriteFile(unitOverridePath, unitOverride, 0o644); err != nil {
		return fmt.Errorf("apache: write %s: %w", unitOverridePath, err)
	}
	if err := svc.DaemonReload(ctx); err != nil {
		return fmt.Errorf("apache: reload unit files: %w", err)
	}
	// A drop-in changes how the process is launched, so a running server has
	// to be restarted rather than reloaded to pick it up.
	if st, err := svc.Get(ctx, b.unit); err == nil && st.Running() {
		if err := svc.Restart(ctx, b.unit); err != nil {
			return fmt.Errorf("apache: restart after unit change: %w", err)
		}
	}
	return nil
}

// distroTLSCert is the certificate the distribution's own ssl.conf points at.
//
// It is generated by httpd-init.service, which normally runs just before
// httpd starts. The panel tests its configuration before anything starts, so
// on a host where httpd has never run the test fails on a file the panel does
// not manage and did not break -- with an error that reads as if the panel's
// own vhosts were wrong.
const distroTLSCert = "/etc/pki/tls/certs/localhost.crt"

func ensureDistroTLSCert(ctx context.Context) error {
	if _, err := os.Stat(distroTLSCert); err == nil {
		return nil
	}
	if st, err := svc.Get(ctx, "httpd-init"); err != nil || st.Enabled == "not-found" {
		return nil // no mod_ssl on this host; its ssl.conf is not there either
	}
	if err := svc.Start(ctx, "httpd-init"); err != nil {
		return fmt.Errorf("apache: generate the distribution TLS certificate: %w", err)
	}
	return nil
}

// TestConfig asks Apache to parse the configuration.
//
// Unlike OpenLiteSpeed's, this is a real check: httpd -t fails on an unknown
// directive, a missing module and a certificate file that is not there. It is
// the reason a bad render is caught before anything is reloaded.
func (b *Backend) TestConfig(ctx context.Context) error {
	res, err := run.Cmd(ctx, []string{binHTTPD, "-t"}, run.Timeout(60*time.Second), run.AllowExit(1))
	if err != nil {
		return fmt.Errorf("apache: config test could not run: %w", err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("apache: configuration rejected:\n%s", strings.TrimSpace(res.Output()))
	}
	return nil
}

// Reload applies configuration without dropping connections. Workers finish
// the request they are on and the next one starts on the new configuration.
func (b *Backend) Reload(ctx context.Context) error {
	// Only when it is already running. A reload of a stopped unit is an
	// error on some systemd versions and a silent no-op on others, and the
	// first apply of a fresh install happens before anything has started.
	if st, err := svc.Get(ctx, b.unit); err == nil && !st.Running() {
		return svc.Start(ctx, b.unit)
	}
	if err := svc.Reload(ctx, b.unit); err != nil {
		return fmt.Errorf("apache: reload %s: %w", b.unit, err)
	}
	return nil
}

// writeFileAtomic writes through a temporary file in the same directory, so a
// reader never sees a half-written config and a crash mid-write leaves the
// previous file intact.
func writeFileAtomic(path string, content []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return fmt.Errorf("apache: create %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".opanel-*")
	if err != nil {
		return fmt.Errorf("apache: temp file in %s: %w", dir, err)
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
	if err := os.Chmod(tmpName, mode); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("apache: install %s: %w", path, err)
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
		return fmt.Errorf("apache: %s is not a directory", src)
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
