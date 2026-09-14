package installer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bnixvn/opanel-ent/internal/platform/run"
)

// ComposerPath is where composer lands. On the PATH, unlike WP-CLI, because
// a terminal session runs it directly rather than the panel running it for
// somebody.
const ComposerPath = "/usr/local/bin/composer"

const (
	composerSetupURL = "https://getcomposer.org/installer"
	composerSigURL   = "https://composer.github.io/installer.sig"
)

func checkComposer(_ context.Context, _ *Options) (bool, error) {
	_, err := os.Stat(ComposerPath)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
}

// stepComposer installs composer, which is how a Laravel project gets its
// dependencies and how half of modern PHP is distributed.
//
// The setup script is checked against the signature composer publishes
// before it is run. Both come from the same project, so this is not a
// defence against a compromised getcomposer.org; it is a defence against a
// truncated download, which is the failure that happens -- and running an
// unverified downloaded script as root is not a thing to do on the strength
// of "it probably arrived intact".
func stepComposer(ctx context.Context, opts *Options) error {
	php := newestPHP()
	if php == "" {
		return errors.New("composer needs a PHP interpreter and none is installed yet")
	}

	dir, err := os.MkdirTemp("", "composer-setup-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	setup := filepath.Join(dir, "installer.php")

	if _, err := run.Cmd(ctx, []string{
		"curl", "-fsSL", "--max-time", "120", "-o", setup, composerSetupURL,
	}, run.Timeout(3*time.Minute)); err != nil {
		return fmt.Errorf("download the composer installer: %w", err)
	}

	want, err := run.Cmd(ctx, []string{"curl", "-fsSL", "--max-time", "60", composerSigURL})
	if err != nil {
		return fmt.Errorf("fetch the composer installer signature: %w", err)
	}
	expected := strings.TrimSpace(want.Stdout)
	if len(expected) != 96 {
		return fmt.Errorf("the composer signature endpoint returned something unexpected: %q", expected)
	}

	got, err := run.Cmd(ctx, []string{php, "-r", "echo hash_file('sha384', $argv[1]);", setup})
	if err != nil {
		return fmt.Errorf("hash the composer installer: %w", err)
	}
	if !strings.EqualFold(strings.TrimSpace(got.Stdout), expected) {
		return errors.New("the composer installer did not match its published signature; refusing to run it")
	}

	if _, err := run.Cmd(ctx, []string{
		php, setup, "--quiet",
		"--install-dir=" + filepath.Dir(ComposerPath),
		"--filename=" + filepath.Base(ComposerPath),
	}, run.Timeout(5*time.Minute)); err != nil {
		return fmt.Errorf("run the composer installer: %w", err)
	}
	_ = opts
	return os.Chmod(ComposerPath, 0o755)
}

// newestPHP finds an interpreter. There is no /usr/bin/php on a server whose
// PHP comes from the webserver's own builds.
func newestPHP() string {
	if st, err := os.Stat("/usr/bin/php"); err == nil && !st.IsDir() {
		return "/usr/bin/php"
	}
	best := ""
	for _, pattern := range []string{
		"/usr/local/lsws/lsphp*/bin/php",
		"/opt/alt/php*/usr/bin/php",
	} {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			continue
		}
		for _, m := range matches {
			if st, err := os.Stat(m); err == nil && !st.IsDir() && m > best {
				best = m
			}
		}
	}
	return best
}
