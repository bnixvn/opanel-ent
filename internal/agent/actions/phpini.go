package actions

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/bnixvn/opanel-ent/internal/agent"
	"github.com/bnixvn/opanel-ent/internal/phpini"
	"github.com/bnixvn/opanel-ent/internal/phpmgr"
)

// PHPIniRequest writes the panel's ini drop-in for one PHP version.
type PHPIniRequest struct {
	Version  string            `json:"version"`
	Settings map[string]string `json:"settings"`
}

// Validate checks the version and every directive.
//
// Checked again here rather than trusted from the panel, because this is the
// process that writes the file: a value carrying a newline would become a
// second directive, and php.ini has directives that load shared objects.
func (r *PHPIniRequest) Validate() error {
	if !phpmgr.ValidVersion(r.Version) {
		return fmt.Errorf("php version %q is malformed", r.Version)
	}
	if _, err := phpini.Check(r.Settings); err != nil {
		return err
	}
	return nil
}

// PHPIniResult reports what was written.
type PHPIniResult struct {
	Path     string `json:"path"`
	Lines    int    `json:"lines"`
	Reloaded bool   `json:"reloaded"`
}

func registerPHPIni(r *agent.Registry, deps Deps) {
	agent.RegisterSlow(r, "php.ini.write", 1, 5*time.Minute,
		func(ctx context.Context, in PHPIniRequest) (PHPIniResult, error) {
			return writePHPIni(ctx, deps, in)
		})
}

// writePHPIni renders the drop-in and restarts the interpreters.
func writePHPIni(ctx context.Context, deps Deps, in PHPIniRequest) (PHPIniResult, error) {
	path := deps.PHP.IniDropIn(in.Version)

	// A version that is not installed has no scan directory, and creating one
	// would leave a file that takes effect later, silently, if the version is
	// ever installed.
	if _, err := os.Stat(filepath.Dir(path)); err != nil {
		return PHPIniResult{}, fmt.Errorf("php %s is not installed on this server", in.Version)
	}

	checked, err := phpini.Check(in.Settings)
	if err != nil {
		return PHPIniResult{}, err
	}
	body := renderIniFile(in.Version, checked)

	if len(checked) == 0 {
		// Nothing set means no file, not an empty one: leaving a stale file
		// behind is how a setting nobody can find in the panel keeps
		// applying.
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return PHPIniResult{}, err
		}
	} else if err := writeFileAtomic(path, body, 0o644); err != nil {
		return PHPIniResult{}, err
	}

	// php.ini is read when an interpreter starts, so nothing changes until
	// the external applications are restarted. Reported rather than assumed:
	// a failure here means the file is right and the running processes are
	// not, which the operator has to know.
	reloaded := deps.Webserver.Reload(ctx) == nil
	return PHPIniResult{Path: path, Lines: len(checked), Reloaded: reloaded}, nil
}

// renderIniFile builds the drop-in.
func renderIniFile(version string, settings map[string]string) []byte {
	names := make([]string, 0, len(settings))
	for name := range settings {
		names = append(names, name)
	}
	sort.Strings(names)

	var b strings.Builder
	b.WriteString("; Managed by OPanel. Edits here are replaced on the next save.\n")
	b.WriteString("; These apply to every website on PHP " + version + ".\n")
	b.WriteString("; A website can override one of these from its own settings,\n")
	b.WriteString("; which the webserver applies after this file is read.\n\n")
	for _, name := range names {
		b.WriteString(name)
		b.WriteString(" = ")
		b.WriteString(settings[name])
		b.WriteString("\n")
	}
	return []byte(b.String())
}

// writeFileAtomic writes through a temporary file in the same directory, so
// an interpreter starting mid-write never reads half a file.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".opanel-ini-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(name, mode); err != nil {
		return err
	}
	return os.Rename(name, path)
}
