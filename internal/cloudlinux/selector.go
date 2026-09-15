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

	"github.com/bnixvn/opanel-ent/internal/platform/svc"
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
	// cageFSSkeleton is the template every cage is built from. Its presence
	// is how "has --init already run here?" is answered.
	cageFSSkeleton = "/usr/share/cagefs-skeleton"
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

// InstallCageFS puts CageFS on a converted host and builds its skeleton.
//
// cldeploy does not install it. A freshly converted host has lve-utils and
// lvemanager and neither cagefs nor alt-php, so every step that assumes
// CageFS -- PHP Selector above all, which cannot exist without it -- fails on
// exactly the host the documented sequence produces. Found by running that
// sequence.
//
// What this does not do is put any account inside a cage. That is a change in
// what a customer's shell and file manager can see, and it belongs to a
// decision about one account rather than to setting the subsystem up.
func InstallCageFS(ctx context.Context, install func(context.Context, string) error) error {
	if !fileExists(cageFSCtl) {
		if install == nil {
			return fmt.Errorf("cloudlinux: %s is not here and nothing was given to install it", cageFSCtl)
		}
		if err := install(ctx, "cagefs"); err != nil {
			return fmt.Errorf("cloudlinux: install cagefs: %w", err)
		}
	}
	if !fileExists(cageFSCtl) {
		return fmt.Errorf("cloudlinux: cagefs installed but %s is still not there", cageFSCtl)
	}

	// --init builds the skeleton: a few gigabytes of the system copied into
	// the template every cage is made from, and several minutes of work.
	//
	// Only when there is not one already. --init is not idempotent -- it
	// refuses outright on a host that has a skeleton and says to use --reinit
	// -- so running it unconditionally turns the second run of a resumable
	// command into a failure. Refreshing an existing skeleton is what
	// --force-update does, and SetupSelector does that after the interpreters
	// are in, which is the moment it actually matters.
	if !dirExists(cageFSSkeleton) {
		if out, err := exec.CommandContext(ctx, cageFSCtl, "--init").CombinedOutput(); err != nil {
			return fmt.Errorf("cloudlinux: build the CageFS skeleton: %w (%s)", err, lastLine(string(out)))
		}
	}

	// The service last, and this order is the point. Starting it while --init
	// is still building leaves it restarting against a half-built skeleton
	// until it hits systemd's rate limit and gives up -- a failed unit whose
	// message says nothing about why.
	if err := svc.Enable(ctx, "cagefs", true); err != nil {
		return fmt.Errorf("cloudlinux: start cagefs: %w", err)
	}
	return nil
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

// writeNativeConf tells the selector what "native" means on this host, when
// anything does.
//
// Measured rather than assumed, and optional rather than required. "Native" in
// CloudLinux's sense is the interpreter the distribution installed -- and a
// host running this panel may not have one at all: PHP comes from Remi under
// /opt/remi or from alt-php under /opt/alt, and neither puts anything at
// /usr/bin/php. The first fresh host to reach this step had no system PHP and
// the step refused to continue, which is the wrong answer to a question whose
// honest reply is "there is no native PHP here".
//
// So an absent system PHP writes no file and stops nothing. The selector then
// offers the alt-php versions and no "native" entry, which is exactly true:
// every interpreter a customer can choose is one of CloudLinux's. Inventing a
// native by pointing it at the panel's own default would be worse -- it would
// make "native" and "8.4" two names for one thing, and hide the fact that the
// distribution's PHP is not installed.
// nativeConfHeader explains the file to whoever opens it next.
const nativeConfHeader = `# Written by OPanel. Do not edit.
#
# What PHP Selector hands a customer who chooses "native": the interpreter the
# distribution installed, not the one this panel runs sites under. Only paths
# that exist are listed -- a path here that is not there is offered to a
# customer as a working choice and then fails.
`

func writeNativeConf() error {
	var b strings.Builder
	b.WriteString(nativeConfHeader)
	found := false
	for _, c := range nativeCandidates {
		if !fileExists(c.path) {
			continue
		}
		fmt.Fprintf(&b, "%s = %s\n", c.key, c.path)
		found = true
	}
	if !found {
		// Nothing to declare. Any stale file goes with it, so a host that
		// loses its system PHP does not keep offering it.
		if err := os.Remove(nativeConfPath); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("cloudlinux: remove the stale %s: %w", nativeConfPath, err)
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(nativeConfPath), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(nativeConfPath, []byte(b.String()), 0o644); err != nil {
		return fmt.Errorf("cloudlinux: write %s: %w", nativeConfPath, err)
	}
	return nil
}

// dirExists reports whether p is a directory.
func dirExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}
