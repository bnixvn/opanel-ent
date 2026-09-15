package actions

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
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
	agent.Register(r, "php.ini.read", 1,
		func(ctx context.Context, in PHPVersionRequest) (PHPIniReadResult, error) {
			return readPHPIni(ctx, deps, in.Version)
		})

	agent.RegisterSlow(r, "php.ini.write", 1, 5*time.Minute,
		func(ctx context.Context, in PHPIniRequest) (PHPIniResult, error) {
			return writePHPIni(ctx, deps, in)
		})
}

// writePHPIni renders the drop-in and restarts the interpreters.
func writePHPIni(ctx context.Context, deps Deps, in PHPIniRequest) (PHPIniResult, error) {
	path := deps.PHP().IniDropIn(in.Version)

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
	reloaded := false
	if ws, err := deps.Backend(); err == nil {
		reloaded = ws.Reload(ctx) == nil
	}
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

// PHPIniReadResult reports what an interpreter is configured with.
type PHPIniReadResult struct {
	Version string `json:"version"`
	// Values is what the interpreter will start with: php.ini plus every
	// file in its scan directory, later files winning.
	Values map[string]string `json:"values"`
	// Managed is only what the panel's own drop-in sets. The panel replaces
	// that file wholesale when it writes, so the caller needs to know what
	// is in it before deciding that an empty form means "set nothing".
	Managed map[string]string `json:"managed"`
}

// readPHPIni reads the configuration files rather than asking the
// interpreter.
//
// Asking would be simpler and is wrong: the command-line interpreter
// overrides several directives for itself -- max_execution_time is 0 on the
// CLI whatever php.ini says, and ini_get_all reports that as the master
// value too -- so a panel that asked would tell somebody their websites have
// no time limit when in fact they have thirty seconds.
func readPHPIni(_ context.Context, deps Deps, version string) (PHPIniReadResult, error) {
	out := PHPIniReadResult{
		Version: version,
		Values:  map[string]string{},
		Managed: map[string]string{},
	}
	dropIn := deps.PHP().IniDropIn(version)
	scanDir := filepath.Dir(dropIn)
	if _, err := os.Stat(scanDir); err != nil {
		return out, fmt.Errorf("php %s is not installed on this server", version)
	}
	mainIni := filepath.Join(filepath.Dir(scanDir), "php.ini")

	wanted := map[string]bool{}
	for _, d := range phpini.Catalogue() {
		wanted[d.Name] = true
	}

	// php.ini first, then the scan directory in the order the interpreter
	// reads it, which is alphabetical. Later files win, which is why the
	// panel's own drop-in is named 99-.
	files := []string{mainIni}
	if entries, err := os.ReadDir(scanDir); err == nil {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".ini") {
				names = append(names, e.Name())
			}
		}
		sort.Strings(names)
		for _, n := range names {
			files = append(files, filepath.Join(scanDir, n))
		}
	}

	for _, f := range files {
		for name, value := range parseIniFile(f, wanted) {
			out.Values[name] = value
			if f == dropIn {
				out.Managed[name] = value
			}
		}
	}
	return out, nil
}

// iniLine matches "name = value", with optional quotes and a trailing
// comment. php.ini is not INI in any strict sense, but the directives this
// reads are always written this way.
var iniLine = regexp.MustCompile(`^\s*([A-Za-z0-9_.]+)\s*=\s*(.*)$`)

// parseIniFile pulls the wanted directives out of one file.
func parseIniFile(path string, wanted map[string]bool) map[string]string {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	out := map[string]string{}
	for _, line := range strings.Split(string(body), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, ";") || strings.HasPrefix(trimmed, "#") ||
			strings.HasPrefix(trimmed, "[") {
			continue
		}
		m := iniLine.FindStringSubmatch(trimmed)
		if m == nil || !wanted[m[1]] {
			continue
		}
		value := strings.TrimSpace(m[2])
		// A trailing comment, but only one introduced by a semicolon: a
		// value may legitimately contain a hash.
		if i := strings.Index(value, ";"); i >= 0 {
			value = strings.TrimSpace(value[:i])
		}
		value = strings.Trim(value, `"'`)
		if value != "" {
			out[m[1]] = value
		}
	}
	return out
}
