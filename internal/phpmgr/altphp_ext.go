package phpmgr

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Extension handling for alt-php, which is not like anyone else's.
//
// CloudLinux ships three directories per version: php.d is what the
// interpreter loads, and php.d.std and php.d.all are catalogues. On a fresh
// install php.d holds four files. The other ninety-odd extensions are
// installed, on disk, and not loaded -- because the system-wide interpreter is
// meant to be minimal and PHP Selector composes a set per customer inside
// their CageFS cage.
//
// This panel runs alt-php through per-site FPM pools outside any cage, so the
// system-wide set is the set every site gets. Left alone it is four
// extensions: no gd, no mbstring, no dom, no intl, no Phar. WordPress limps,
// and WP-CLI -- which is a .phar -- cannot start at all.
//
// So the standard set is made the active set. Found the hard way: sites were
// migrated onto alt-php and the check that should have caught this compared
// alt-php under Apache with alt-php under LiteSpeed. Both were equally
// crippled, so the comparison was identical and proved nothing. The
// comparison that mattered was against the interpreter being replaced.
const (
	altPHPActiveDir   = "etc/php.d"
	altPHPStandardDir = "etc/php.d.std"
	// altPHPDefaultsFile sorts before the extension files so a setting here
	// can be overridden by one of them rather than the other way round.
	altPHPDefaultsFile = "00-opanel-defaults.ini"
	// panelIniFile is the panel's own override file, which this must not
	// delete while mirroring.
	panelIniFile = "99-opanel.ini"
)

// extensionFailure matches the two ways a bad extension announces itself.
//
// Both are warnings on stderr and neither stops PHP, which is why they go
// unnoticed: the interpreter starts, serves, and is missing something.
var extensionFailure = regexp.MustCompile(
	`Unable to load dynamic library '([^']+?)(?:\.so)?'|` +
		`Cannot load module "([^"]+)" because conflicting`)

// EnableStandardExtensions makes CloudLinux's standard set the active one for
// a version, and drops anything that will not load.
//
// The pruning is not defensive tidiness. Installing the alt-php group brings
// every PECL package CloudLinux builds, and on a real host three of them do
// not work: two are built against symbols this PHP does not export, and
// imagick and gmagick cannot be loaded together. Their warnings appear on
// stderr of every CLI invocation, which is how they end up inside the output
// of WP-CLI and anything else that parses what PHP prints.
func (p *AltPHP) EnableStandardExtensions(ctx context.Context, version string) error {
	if err := checkVersion(p, version); err != nil {
		return err
	}
	prefix := p.prefix(version)
	active := path.Join(prefix, altPHPActiveDir)
	standard := path.Join(prefix, altPHPStandardDir)
	if _, err := os.Stat(standard); err != nil {
		return nil // this version does not ship a catalogue; nothing to do
	}

	if err := mirrorExtensionDir(standard, active); err != nil {
		return err
	}
	if err := writeAltPHPDefaults(active); err != nil {
		return err
	}
	return pruneBrokenExtensions(ctx, p.CLIBinary(version), active)
}

// mirrorExtensionDir makes active hold exactly what standard holds, plus the
// panel's own files.
//
// Mirrored rather than merged. The four files a fresh php.d starts with name
// the same extensions the catalogue does under different filenames, so
// merging loaded mysqlnd and mysqli twice and PHP said so on every start.
func mirrorExtensionDir(standard, active string) error {
	entries, err := os.ReadDir(standard)
	if err != nil {
		return fmt.Errorf("phpmgr: read %s: %w", standard, err)
	}
	if err := os.MkdirAll(active, 0o755); err != nil {
		return err
	}

	existing, err := os.ReadDir(active)
	if err != nil {
		return fmt.Errorf("phpmgr: read %s: %w", active, err)
	}
	for _, e := range existing {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".ini") {
			continue
		}
		if e.Name() == panelIniFile || e.Name() == altPHPDefaultsFile {
			continue // the panel's own; not the catalogue's to replace
		}
		if err := os.Remove(path.Join(active, e.Name())); err != nil {
			return fmt.Errorf("phpmgr: remove %s: %w", e.Name(), err)
		}
	}

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".ini") {
			continue
		}
		data, err := os.ReadFile(path.Join(standard, e.Name()))
		if err != nil {
			return err
		}
		if err := os.WriteFile(path.Join(active, e.Name()), data, 0o644); err != nil {
			return fmt.Errorf("phpmgr: write %s: %w", e.Name(), err)
		}
	}
	return nil
}

// writeAltPHPDefaults sets what alt-php leaves unset and PHP then complains
// about on every start.
func writeAltPHPDefaults(active string) error {
	const body = `; Written by OPanel. Do not edit.
;
; alt-php ships date.timezone empty, and PHP warns about that on stderr every
; time it starts -- which lands in the middle of the output of anything that
; reads what PHP printed. WP-CLI returning a warning where JSON was expected
; is the version of this that gets reported.
date.timezone = UTC
`
	return os.WriteFile(path.Join(active, altPHPDefaultsFile), []byte(body), 0o644)
}

// pruneBrokenExtensions removes the ini files whose extensions fail to load.
//
// Bounded, and it stops as soon as a run is quiet. Removing one extension can
// let another load -- gmagick and imagick conflict, so which one fails depends
// on which loaded first -- which is why this is a loop rather than one pass.
func pruneBrokenExtensions(ctx context.Context, php, active string) error {
	for pass := 0; pass < 3; pass++ {
		out, err := exec.CommandContext(ctx, php, "-m").CombinedOutput()
		if err != nil && len(out) == 0 {
			return fmt.Errorf("phpmgr: run %s: %w", php, err)
		}
		bad := failedExtensions(string(out))
		if len(bad) == 0 {
			return nil
		}
		removed := 0
		for _, name := range bad {
			matches, _ := filepath.Glob(path.Join(active, "*"+name+"*.ini"))
			for _, m := range matches {
				if path.Base(m) == panelIniFile || path.Base(m) == altPHPDefaultsFile {
					continue
				}
				if os.Remove(m) == nil {
					removed++
				}
			}
		}
		if removed == 0 {
			// Something is complaining that this cannot act on. Leaving it is
			// better than looping: the extensions that do work are loaded.
			return nil
		}
	}
	return nil
}

// failedExtensions pulls the extension names out of PHP's startup warnings.
func failedExtensions(out string) []string {
	seen := make(map[string]bool)
	for _, m := range extensionFailure.FindAllStringSubmatch(out, -1) {
		for _, g := range m[1:] {
			if g != "" && !seen[g] {
				seen[g] = true
			}
		}
	}
	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// EnableStandardExtensionsAll does it for every installed version.
func (p *AltPHP) EnableStandardExtensionsAll(ctx context.Context) error {
	versions, err := p.Available(ctx)
	if err != nil {
		return err
	}
	for _, v := range versions {
		if !v.Installed {
			continue
		}
		if err := p.EnableStandardExtensions(ctx, v.Version); err != nil {
			return fmt.Errorf("php %s: %w", v.Version, err)
		}
	}
	return nil
}
