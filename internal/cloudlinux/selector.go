package cloudlinux

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// PHP Selector, as CloudLinux installs it on a panel they do not support.
//
// Three steps, and they are theirs rather than ours: the alt-php interpreters
// are installed as a package group, cagefsctl registers whichever of them are
// present with the selector, and the cage skeleton is rebuilt so a customer
// inside CageFS can see them. The panel runs them in that order because the
// second reads what the first installed and the third copies what the second
// wrote.
const (
	selectorDir      = "/etc/cl.selector"
	nativeConfPath   = selectorDir + "/native.conf"
	selectorConfPath = selectorDir + "/selector.conf"
	selectorCtlPath  = "/usr/bin/selectorctl"
	altPHPGroup      = "alt-php"
	selectorSetupArg = "--setup-cl-selector"
)

// nativeCandidates are the interpreters CloudLinux asks about, and where the
// distribution puts them. Each is written to native.conf only if it is there:
// a path in that file that does not exist is worse than an absent line, since
// the selector will hand it to a customer as a working choice.
var nativeCandidates = []struct{ key, path string }{
	{"php", "/usr/bin/php"},
	{"php-cli", "/usr/bin/php"},
	{"php-cgi", "/usr/bin/php-cgi"},
	{"php-fpm", "/usr/sbin/php-fpm"},
	{"php.ini", "/etc/php.ini"},
}

// SelectorStatus is what the panel can say about PHP Selector.
type SelectorStatus struct {
	// Available is whether the tools are installed at all.
	Available bool `json:"available"`
	// Versions are the interpreters the selector will offer, newest first.
	Versions []string `json:"versions"`
	// Native is the interpreter a customer gets when they choose "native",
	// empty when native.conf has not been written.
	Native string `json:"native,omitempty"`
}

// SelectorState reads what the selector currently offers.
//
// From the selector's own registry rather than by running selectorctl. Same
// answer -- that file is what selectorctl reads, and it is what
// --setup-cl-selector writes, so a version disabled by an administrator is
// absent from both. The difference is that selectorctl takes five seconds to
// walk /opt/alt and this takes microseconds, and this runs on every load of
// the CloudLinux page.
func SelectorState(_ context.Context) SelectorStatus {
	st := SelectorStatus{Available: fileExists(selectorCtlPath) && fileExists(cageFSCtl)}
	if !st.Available {
		return st
	}
	data, err := os.ReadFile(selectorConfPath)
	if err == nil {
		seen := make(map[string]bool)
		for _, line := range strings.Split(string(data), "\n") {
			// "php 8.4 8.4.25 /opt/alt/php84/usr/bin/php-cgi", one line per
			// interpreter per version. Only the php-cgi rows: the others are
			// the same version reached a different way, and listing them
			// would show every version four times.
			f := strings.Fields(line)
			if len(f) < 4 || f[0] != "php" || seen[f[1]] {
				continue
			}
			seen[f[1]] = true
			st.Versions = append(st.Versions, f[1])
		}
		sortVersionsDescending(st.Versions)
	}
	if data, err := os.ReadFile(nativeConfPath); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if k, v, ok := strings.Cut(line, "="); ok && strings.TrimSpace(k) == "php" {
				st.Native = strings.TrimSpace(v)
			}
		}
	}
	return st
}

// sortVersionsDescending orders "8.10" after "8.9" rather than before it.
//
// A string sort is right for every version CloudLinux ships today and wrong
// the first time a minor reaches ten, which is the kind of thing that is
// cheaper to get right now than to notice later.
func sortVersionsDescending(v []string) {
	sort.Slice(v, func(i, j int) bool {
		ai, bi := versionParts(v[i]), versionParts(v[j])
		if ai[0] != bi[0] {
			return ai[0] > bi[0]
		}
		return ai[1] > bi[1]
	})
}

func versionParts(s string) [2]int {
	var out [2]int
	major, minor, _ := strings.Cut(s, ".")
	out[0], _ = strconv.Atoi(major)
	out[1], _ = strconv.Atoi(minor)
	return out
}

// SetupSelector installs the alt-php interpreters and registers them.
//
// Installing the whole group rather than a list the panel chooses: the point
// of the selector is that a customer picks a version, and a set curated here
// would be this panel deciding which of CloudLinux's interpreters exist. It
// is about two gigabytes, which is the price of the feature.
func SetupSelector(ctx context.Context, install func(context.Context, string) error) error {
	if !fileExists(cageFSCtl) {
		return fmt.Errorf("cloudlinux: %s is not here; CageFS has to be installed before PHP Selector", cageFSCtl)
	}
	if install != nil {
		if err := install(ctx, altPHPGroup); err != nil {
			return fmt.Errorf("cloudlinux: install the alt-php interpreters: %w", err)
		}
	}
	if err := writeNativeConf(); err != nil {
		return err
	}
	// --setup-cl-selector registers every alt-php that is installed, and
	// re-registers on every run, which is why installing a version later is
	// a matter of running this again rather than of undoing anything.
	if out, err := exec.CommandContext(ctx, cageFSCtl, selectorSetupArg).CombinedOutput(); err != nil {
		return fmt.Errorf("cloudlinux: register the interpreters: %w (%s)", err, lastLine(string(out)))
	}
	// The skeleton is what a customer inside CageFS actually sees. Without
	// this the selector offers versions that are not in their cage.
	if out, err := exec.CommandContext(ctx, cageFSCtl, "--force-update").CombinedOutput(); err != nil {
		return fmt.Errorf("cloudlinux: refresh the CageFS skeleton: %w (%s)", err, lastLine(string(out)))
	}
	return nil
}

// writeNativeConf tells the selector what "native" means on this host.
//
// Measured rather than assumed. The native interpreter is whatever the
// distribution put at /usr/bin/php, which on this panel is not the version
// sites run: those get a per-site PHP-FPM pool. A customer choosing "native"
// is choosing the system one, and the file has to say which that is or the
// choice resolves to nothing.
func writeNativeConf() error {
	var b strings.Builder
	b.WriteString("# Written by OPanel. Do not edit.\n")
	b.WriteString("#\n")
	b.WriteString("# What PHP Selector hands a customer who chooses \"native\": the\n")
	b.WriteString("# interpreter the distribution installed, not the one this panel runs\n")
	b.WriteString("# sites under. Only paths that exist are listed -- a path here that is\n")
	b.WriteString("# not there is offered as a working choice and then fails.\n")
	found := false
	for _, c := range nativeCandidates {
		if !fileExists(c.path) {
			continue
		}
		fmt.Fprintf(&b, "%s = %s\n", c.key, c.path)
		found = true
	}
	if !found {
		return fmt.Errorf("cloudlinux: no system PHP found, so \"native\" would mean nothing; install php-cli first")
	}
	if err := os.MkdirAll(filepath.Dir(nativeConfPath), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(nativeConfPath, []byte(b.String()), 0o644); err != nil {
		return fmt.Errorf("cloudlinux: write %s: %w", nativeConfPath, err)
	}
	return nil
}
