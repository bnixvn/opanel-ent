package actions

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/bnixvn/opanel-ent/internal/agent"
	"github.com/bnixvn/opanel-ent/internal/platform/linuxuser"
	"github.com/bnixvn/opanel-ent/internal/platform/run"
)

// SiteMoveBudget bounds a move. A rename within one filesystem is instant,
// but the ownership pass walks every file, and a site with a large media
// library takes a while.
const SiteMoveBudget = 30 * time.Minute

// SiteMoveRequest hands a website's files to another account.
type SiteMoveRequest struct {
	Domain string `json:"domain"`
	From   string `json:"from"`
	To     string `json:"to"`
}

// Validate checks both accounts and the site name.
func (r *SiteMoveRequest) Validate() error {
	if !linuxuser.ValidName(r.From) {
		return fmt.Errorf("%q is not an acceptable account name", r.From)
	}
	if !linuxuser.ValidName(r.To) {
		return fmt.Errorf("%q is not an acceptable account name", r.To)
	}
	if r.From == r.To {
		return errors.New("the site already belongs to that account")
	}
	// The domain becomes a directory name under each home, so it must not be
	// able to name anything else.
	if r.Domain == "" || strings.ContainsAny(r.Domain, "/\\") || strings.Contains(r.Domain, "..") {
		return fmt.Errorf("%q is not an acceptable site name", r.Domain)
	}
	return nil
}

// SiteMoveResult reports where the files ended up.
type SiteMoveResult struct {
	VhostRoot    string `json:"vhost_root"`
	DocumentRoot string `json:"document_root"`
	Files        int64  `json:"files"`
}

func registerSiteMove(r *agent.Registry) {
	agent.RegisterSlow(r, "site.move", 1, SiteMoveBudget, moveSite)
}

// moveSite relocates a site's directory into another account's home.
//
// A rename is tried first and almost never succeeds: XFS refuses to rename a
// directory across project quota boundaries, and every home is its own
// project once quotas are enforced. The copy below is therefore the usual
// path, and the source is only removed once the copy has finished.
func moveSite(ctx context.Context, in SiteMoveRequest) (SiteMoveResult, error) {
	from, err := linuxuser.Lookup(in.From)
	if err != nil {
		return SiteMoveResult{}, err
	}
	to, err := linuxuser.Lookup(in.To)
	if err != nil {
		return SiteMoveResult{}, err
	}
	if from == nil || to == nil {
		return SiteMoveResult{}, errors.New("both accounts must exist on this server")
	}

	src := path.Join(from.Home, in.Domain)
	dst := path.Join(to.Home, in.Domain)

	info, err := os.Stat(src)
	if err != nil {
		return SiteMoveResult{}, fmt.Errorf("the site directory %s is not there: %w", src, err)
	}
	if !info.IsDir() {
		return SiteMoveResult{}, fmt.Errorf("%s is not a directory", src)
	}
	if _, err := os.Stat(dst); err == nil {
		return SiteMoveResult{}, fmt.Errorf(
			"%s already has a directory called %s; rename or remove it first", in.To, in.Domain)
	}

	if err := os.Rename(src, dst); err != nil {
		// EXDEV here is not what it looks like. Both homes are on the same
		// filesystem, but XFS refuses to rename a directory into a tree with
		// a different project id -- which is exactly what every home is once
		// disk quotas are enforced. So the quota feature makes the fast path
		// impossible and the copy is the normal route, not the exception.
		if !errors.Is(err, syscall.EXDEV) {
			return SiteMoveResult{}, fmt.Errorf("move %s to %s: %w", src, dst, err)
		}
		if err := copyTree(ctx, src, dst); err != nil {
			// Whatever was copied is removed again: a half-copied site in
			// the new home would be served as though it were complete.
			_ = os.RemoveAll(dst)
			return SiteMoveResult{}, fmt.Errorf("copy %s to %s: %w", src, dst, err)
		}
		if err := os.RemoveAll(src); err != nil {
			return SiteMoveResult{}, fmt.Errorf(
				"the site was copied to %s but %s could not be removed: %w", dst, src, err)
		}
	}

	files, err := chownTree(dst, int(to.UID), int(to.GID))
	if err != nil {
		return SiteMoveResult{}, fmt.Errorf(
			"the files were moved to %s but could not be handed to %s: %w", dst, in.To, err)
	}

	// The modes that make the site servable are set again, because the
	// directory came from another account's tree and the panel's own rules
	// for them are what the webserver depends on.
	docRoot := path.Join(dst, "public_html")
	for _, d := range []struct {
		path string
		mode fs.FileMode
	}{
		{dst, 0o711},
		{path.Join(dst, "logs"), 0o751},
		{docRoot, 0o755},
	} {
		if _, err := os.Stat(d.path); err != nil {
			continue
		}
		if err := os.Chmod(d.path, d.mode); err != nil {
			return SiteMoveResult{}, fmt.Errorf("chmod %s: %w", d.path, err)
		}
	}

	return SiteMoveResult{VhostRoot: dst, DocumentRoot: docRoot, Files: files}, nil
}

// copyTree duplicates a directory with its permissions, timestamps and
// symlinks, using cp because it already handles every file type correctly
// and does it in C rather than one syscall per file from Go.
//
// -a keeps modes, ownership and links; --reflink=auto asks XFS to share the
// blocks where it can, which on a copy-on-write filesystem makes a large
// site nearly free instead of doubling the disk it uses.
func copyTree(ctx context.Context, src, dst string) error {
	_, err := run.Cmd(ctx, []string{"cp", "-a", "--reflink=auto", src, dst},
		run.Timeout(SiteMoveBudget))
	return err
}

// chownTree hands every file under root to an account.
func chownTree(root string, uid, gid int) (int64, error) {
	var n int64
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// Lchown, not Chown: following a symlink here would change the
		// ownership of whatever it points at, which may be outside the site
		// entirely.
		if err := os.Lchown(p, uid, gid); err != nil {
			return fmt.Errorf("chown %s: %w", p, err)
		}
		n++
		return nil
	})
	return n, err
}
