package actions

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/bnixvn/opanel-ent/internal/agent"
	"github.com/bnixvn/opanel-ent/internal/platform/pkgmgr"
	"github.com/bnixvn/opanel-ent/internal/platform/run"
	"github.com/bnixvn/opanel-ent/internal/platform/svc"
)

// LSWS layout on the host. The installer has no prefix option worth using:
// every LiteSpeed tool, and every piece of documentation, assumes this path.
const (
	LSWSRoot    = "/usr/local/lsws"
	lswsBinary  = LSWSRoot + "/bin/lshttpd"
	lswsConfDir = LSWSRoot + "/conf"
	lswsUnit    = "lshttpd"
)

// LSWSVersion is the release the panel installs.
//
// Pinned rather than "latest": an installer that silently changes which
// version it lays down makes two hosts built a month apart different in a way
// nobody recorded. Upgrades are a decision, not a side effect.
const LSWSVersion = "6.3.3"

// lswsTarballURL is where LiteSpeed publishes it.
func lswsTarballURL(version string) string {
	return fmt.Sprintf("https://www.litespeedtech.com/packages/6.0/lsws-%s-ent-x86_64-linux.tar.gz", version)
}

// trialKeyURL is LiteSpeed's public 15-day evaluation key.
//
// Public on purpose -- it is how the vendor lets anybody try the server -- and
// it is the only licence the panel can obtain on an operator's behalf. A
// production serial has to be bought and typed in.
const trialKeyURL = "https://license.litespeedtech.com/reseller/trial.key"

// serialPattern is the shape of a LiteSpeed serial number.
var serialPattern = regexp.MustCompile(`^[A-Za-z0-9]{4,12}(-[A-Za-z0-9]{4,12}){1,5}$`)

// LSWSInstallRequest installs LiteSpeed Enterprise.
type LSWSInstallRequest struct {
	// Serial is the licence bought from LiteSpeed. Empty asks for the
	// vendor's trial key, which expires in fifteen days.
	Serial string `json:"serial"`
	// Version overrides the pinned release.
	Version string `json:"version"`
}

// Validate checks the serial's shape and the version's.
func (r *LSWSInstallRequest) Validate() error {
	if r.Serial != "" && !serialPattern.MatchString(r.Serial) {
		return fmt.Errorf("%q does not look like a LiteSpeed serial number", r.Serial)
	}
	if r.Version != "" && !regexp.MustCompile(`^[0-9]+\.[0-9]+(\.[0-9]+)?$`).MatchString(r.Version) {
		return fmt.Errorf("%q is not a version number", r.Version)
	}
	return nil
}

// LSWSStatusResult reports what is installed.
type LSWSStatusResult struct {
	Installed bool   `json:"installed"`
	Version   string `json:"version,omitempty"`
	// Licence is "serial", "trial" or empty. A trial that has run out looks
	// exactly like one that has not until the server refuses to start, so
	// the expiry is reported too where the key carries one.
	Licence string `json:"licence,omitempty"`
	Expires string `json:"expires,omitempty"`
	Running bool   `json:"running"`
}

func registerLSWS(r *agent.Registry) {
	agent.Register(r, "lsws.status", 1, func(ctx context.Context, _ struct{}) (LSWSStatusResult, error) {
		return lswsStatus(ctx), nil
	})

	// Slow: thirty megabytes over the network, then a build-and-install
	// script that relinks modules. Two minutes is not enough and failing at
	// two minutes would leave a half-installed server.
	agent.RegisterSlow(r, "lsws.install", 1, 20*time.Minute,
		func(ctx context.Context, in LSWSInstallRequest) (LSWSStatusResult, error) {
			if err := installLSWS(ctx, in); err != nil {
				return LSWSStatusResult{}, err
			}
			return lswsStatus(ctx), nil
		})
}

// installLSWS downloads, licenses and installs LiteSpeed Enterprise.
//
// It deliberately leaves the server stopped. Installing a webserver and
// switching to it are two decisions, and the installer's own script starts
// what it installs -- which on a host already serving traffic means two
// daemons fighting over port 80 the moment the install finishes.
func installLSWS(ctx context.Context, in LSWSInstallRequest) error {
	// Both LiteSpeed servers install into /usr/local/lsws. Running the
	// Enterprise installer over OpenLiteSpeed would replace a running
	// webserver with a different one, in place, without being asked -- so it
	// is refused, and removing the free one stays the operator's decision.
	if fileExists(LSWSRoot + "/bin/openlitespeed") {
		return &agent.DeniedError{Reason: "OpenLiteSpeed occupies /usr/local/lsws; " +
			"remove it first (dnf remove openlitespeed) before installing LiteSpeed Enterprise"}
	}

	version := in.Version
	if version == "" {
		version = LSWSVersion
	}

	dir, err := os.MkdirTemp("", "opanel-lsws-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()

	tarball := filepath.Join(dir, "lsws.tar.gz")
	if _, err := run.Cmd(ctx, []string{"curl", "-fsSL", "--retry", "3", "--retry-all-errors",
		"--connect-timeout", "20", "--max-time", "600", "-o", tarball, lswsTarballURL(version)},
		run.Timeout(11*time.Minute)); err != nil {
		return fmt.Errorf("download LiteSpeed %s: %w", version, err)
	}
	// The download is checked by unpacking it, not by a checksum: LiteSpeed
	// publishes no digest for these tarballs. A truncated transfer is the
	// realistic failure and tar catches that; TLS to the vendor is what
	// stands between this and a substituted file.
	if _, err := run.Cmd(ctx, []string{"tar", "xzf", tarball, "-C", dir}, run.Timeout(5*time.Minute)); err != nil {
		return fmt.Errorf("unpack LiteSpeed %s: %w", version, err)
	}

	src := filepath.Join(dir, "lsws-"+version)
	if _, err := os.Stat(filepath.Join(src, "install.sh")); err != nil {
		return fmt.Errorf("the LiteSpeed archive did not contain %s/install.sh", src)
	}

	if err := writeLicence(ctx, src, in.Serial); err != nil {
		return err
	}

	// LiteSpeed's binaries are linked against libcrypt.so.1, which EL10 no
	// longer ships -- glibc moved to libxcrypt and the old soname now lives
	// in a compatibility package. Without it the installer's own licence
	// check cannot run its binary, and the error it prints is about a shared
	// library rather than about anything an operator did.
	if err := pkgmgr.Install(ctx, "libxcrypt-compat"); err != nil {
		return fmt.Errorf("install the libcrypt compatibility library LiteSpeed needs: %w", err)
	}

	// The vendor's script is interactive and has no unattended mode, so the
	// answers are fed to it in order. Fragile by nature: a new prompt in a
	// future release shifts everything after it. That is why the version is
	// pinned, why the install is verified afterwards by looking for the
	// binary rather than by trusting the exit code, and why the script's
	// output comes back in the error when it does not appear.
	shim, err := pagerShim(dir)
	if err != nil {
		return err
	}
	res, err := run.Cmd(ctx, []string{"./install.sh"},
		run.Dir(src), run.Stdin(installAnswers()),
		// TERM, because the script calls clear(1), which exits non-zero
		// without one and takes the install down with it. "dumb" is the
		// honest description of what it is talking to.
		run.Env("PATH="+shim+":"+os.Getenv("PATH"), "TERM=dumb"),
		run.Timeout(15*time.Minute))
	if err != nil {
		return fmt.Errorf("LiteSpeed installer failed: %w (%s)", err, lastLines(res.Output(), 5))
	}
	if _, err := os.Stat(lswsBinary); err != nil {
		return fmt.Errorf("the LiteSpeed installer finished but %s is not there:\n%s",
			lswsBinary, lastLines(res.Output(), 10))
	}

	// Its own installer starts it. Stop it again: this host already has a
	// webserver on port 80, and which one runs is the switch action's
	// decision, not this one's.
	_ = svc.Stop(ctx, lswsUnit)
	if err := svc.Disable(ctx, lswsUnit, false); err != nil {
		return fmt.Errorf("disable %s after install: %w", lswsUnit, err)
	}
	return nil
}

// pagerShim makes `more` behave like `cat` for the installer.
//
// The script shows its licence with `more ./LICENSE`, and `more` reads stdin
// looking for paging keys. Fed a script instead of a keyboard, it swallows
// the answers meant for the questions that follow -- the licence prompt then
// rejects three lines that were never meant for it and aborts. Replacing the
// pager for this one command is narrower than trying to guess how many lines
// it will eat.
func pagerShim(dir string) (string, error) {
	bin := filepath.Join(dir, "shim")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		return "", err
	}
	script := "#!/bin/sh\nexec cat \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "more"), []byte(script), 0o755); err != nil {
		return "", err
	}
	return bin, nil
}

// installAnswers is the sequence the vendor's installer prompts for.
//
// The WebAdmin console gets a random password that is deliberately not
// reported anywhere: the panel configures this server, the console is not
// how an operator is meant to reach it, and the firewall does not open 7080.
// Somebody who needs the console can set a password with
// /usr/local/lsws/admin/misc/admpass.sh.
//
// PHP is declined. LiteSpeed's own build would take twenty minutes and go
// unused -- sites here run in PHP-FPM pools, which this server reaches the
// same way Apache does.
func installAnswers() string {
	const lf = "\n"
	answers := []string{
		"Yes",            // the licence
		"",               // destination: /usr/local/lsws
		"",               // WebAdmin user: admin
		randomPassword(), // WebAdmin password
		"",               // ...retyped, filled in below
		"apache",         // the account the server runs as
		"apache",         // ...and its group
		"",               // HTTP port: the installer's default, not 80
		"",               // admin console port: 7080
		"",               // notification email: root@localhost
		"n",              // do not build PHP
		"",               // no change to the opcode cache
		"n",              // no AWStats
		"n",              // do not start at boot; the panel decides that
	}
	answers[4] = answers[3]
	// Trailing blanks, so a prompt this list does not know about takes its
	// default instead of reading EOF and aborting the install.
	return strings.Join(answers, lf) + strings.Repeat(lf, 12)
}

// randomPassword is for a console nobody is meant to log into.
func randomPassword() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		// Only reached if the kernel's entropy source fails, and a fixed
		// string would be worse than refusing: it would be a known password
		// on every host that hit this.
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// writeLicence puts the serial or the trial key where install.sh looks.
func writeLicence(ctx context.Context, dir, serial string) error {
	if serial != "" {
		return os.WriteFile(filepath.Join(dir, "serial.no"), []byte(serial+"\n"), 0o600)
	}
	key := filepath.Join(dir, "trial.key")
	if _, err := run.Cmd(ctx, []string{"curl", "-fsSL", "--connect-timeout", "20",
		"--max-time", "120", "-o", key, trialKeyURL}, run.Timeout(3*time.Minute)); err != nil {
		return fmt.Errorf("fetch the LiteSpeed trial key: %w", err)
	}
	st, err := os.Stat(key)
	if err != nil || st.Size() == 0 {
		return fmt.Errorf("the LiteSpeed trial key came back empty; a licence is needed to install")
	}
	return nil
}

// lswsStatus reads what is installed without asking the network.
func lswsStatus(ctx context.Context) LSWSStatusResult {
	out := LSWSStatusResult{}
	if st, err := os.Stat(lswsBinary); err != nil || st.IsDir() {
		return out
	}
	out.Installed = true

	if res, err := run.Cmd(ctx, []string{lswsBinary, "-v"}, run.Timeout(30*time.Second)); err == nil {
		// "LiteSpeed/6.3.3 Enterprise ..." -- the version is the part after
		// the slash on the first word that has one.
		for _, field := range strings.Fields(res.Output()) {
			if name, ver, ok := strings.Cut(field, "/"); ok && strings.EqualFold(name, "LiteSpeed") {
				out.Version = ver
				break
			}
		}
	}

	switch {
	case fileExists(filepath.Join(lswsConfDir, "serial.no")):
		out.Licence = "serial"
	case fileExists(filepath.Join(lswsConfDir, "trial.key")):
		out.Licence = "trial"
		if st, err := os.Stat(filepath.Join(lswsConfDir, "trial.key")); err == nil {
			// The key itself is opaque. Its age is not, and a trial lasts
			// fifteen days from the day it was issued -- which is near
			// enough the day it was fetched to be worth showing.
			out.Expires = st.ModTime().AddDate(0, 0, 15).Format("2006-01-02")
		}
	}

	if st, err := svc.Get(ctx, lswsUnit); err == nil {
		out.Running = st.Running()
	}
	return out
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
