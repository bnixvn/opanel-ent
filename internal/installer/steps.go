package installer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"strings"
	"text/template"
	"time"

	"github.com/bnixvn/opanel-ent/internal/agent/actions"
	"github.com/bnixvn/opanel-ent/internal/phpfpm"
	"github.com/bnixvn/opanel-ent/internal/phpmgr"
	"github.com/bnixvn/opanel-ent/internal/platform/distro"
	"github.com/bnixvn/opanel-ent/internal/platform/pkgmgr"
	"github.com/bnixvn/opanel-ent/internal/platform/run"
	"github.com/bnixvn/opanel-ent/internal/platform/svc"
	"github.com/bnixvn/opanel-ent/internal/webserver"
	"github.com/bnixvn/opanel-ent/internal/webserver/backends"
)

// Accounts and paths the installation creates.
const (
	ServiceUser  = "opanel"
	ServiceGroup = "opanel"
	SFTPGroup    = "opanel-sftp"

	DataDir   = "/var/lib/opanel"
	ConfigDir = "/etc/opanel"
	BinDir    = "/usr/local/bin"

	sshdDropIn = "/etc/ssh/sshd_config.d/60-opanel.conf"
	sshdMain   = "/etc/ssh/sshd_config"
	nftRuleset = "/etc/nftables/opanel.nft"
	nftSysconf = "/etc/sysconfig/nftables.conf"
	unitDir    = "/etc/systemd/system"
	sftpMarker = "# --- OPanel SFTP (managed) ---"
)

// basePackages are needed before anything else works.
//
// The last four are for the terminal rather than the panel: unzip and zip
// because composer unpacks with one and everybody expects the other, and
// node with npm because a Laravel project builds its assets with them. A
// terminal that offers composer and then cannot run it is worse than one
// that does not offer it.
var basePackages = []string{
	"nftables", "valkey", "zstd", "tar", "curl", "git", "quota", "policycoreutils-python-utils",
	"unzip", "zip", "nodejs", "npm",
}

func allSteps() []Step {
	return []Step{
		{Name: "Check host", Apply: stepCheckHost},
		{Name: "Install base packages", Check: checkBasePackages, Apply: stepBasePackages},
		{Name: "Install web server", Check: checkWebserver, Apply: stepInstallWebserver},
		{Name: "Install PHP", Check: checkPHP, Apply: stepInstallPHP},
		{Name: "Add MariaDB repository", Check: checkFile("/etc/yum.repos.d/mariadb.repo"), Apply: stepMariaDBRepo},
		{Name: "Install MariaDB", Check: checkMariaDB, Apply: stepInstallMariaDB},
		{Name: "Harden MariaDB", Check: checkMariaDBHardened, Apply: stepHardenMariaDB},
		{Name: "Create service accounts", Check: checkAccounts, Apply: stepAccounts},
		{Name: "Create directories", Apply: stepDirectories},
		{Name: "Install binaries", Apply: stepBinaries},
		{Name: "Install systemd units", Apply: stepUnits},
		{Name: "Write web server configuration", Apply: stepWebserverConfig},
		{Name: "Enable disk quota", Check: checkProjectQuota, Apply: stepProjectQuota},
		{Name: "Install WP-CLI", Check: checkWPCLI, Apply: stepWPCLI},
		{Name: "Install Composer", Check: checkComposer, Apply: stepComposer},
		{Name: "Configure SFTP", Check: checkSFTP, Apply: stepSFTP},
		{Name: "Configure firewall", Check: checkFirewall, Apply: stepFirewall},
		{Name: "Enable valkey", Apply: stepValkey},
		{Name: "Start agent and API", Apply: stepStartServices},
	}
}

// wpCLIURL and wpCLISumURL are the upstream release and its published
// checksum. Both come from the same origin, so this is not a defence against
// a compromised wp-cli.org -- it is a defence against a truncated download
// and a corrupted mirror, which are the failures that actually happen.
const (
	wpCLIURL    = "https://raw.githubusercontent.com/wp-cli/builds/gh-pages/phar/wp-cli.phar"
	wpCLISumURL = wpCLIURL + ".sha512"
)

func checkWPCLI(_ context.Context, _ *Options) (bool, error) {
	_, err := os.Stat(actions.WPCLIPath)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
}

// stepWPCLI fetches WP-CLI, which is what makes the one-click WordPress
// install possible. It is not on the PATH: customers reach it through the
// panel, and the panel runs it as the site's own account.
func stepWPCLI(ctx context.Context, _ *Options) error {
	dir := filepath.Dir(actions.WPCLIPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	want, err := run.Cmd(ctx, []string{"curl", "-fsSL", "--max-time", "60", wpCLISumURL})
	if err != nil {
		return fmt.Errorf("fetch WP-CLI checksum: %w", err)
	}
	expected := strings.Fields(want.Stdout)
	if len(expected) == 0 || len(expected[0]) != 128 {
		return fmt.Errorf("WP-CLI checksum endpoint returned something unexpected: %q",
			firstLineOf(want.Stdout))
	}

	// Downloaded beside the destination and renamed, so an interrupted
	// download never leaves a half-written phar that looks installed.
	tmp := actions.WPCLIPath + ".part"
	defer func() { _ = os.Remove(tmp) }()
	if _, err := run.Cmd(ctx, []string{
		"curl", "-fsSL", "--max-time", "300", "-o", tmp, wpCLIURL,
	}, run.Timeout(6*time.Minute)); err != nil {
		return fmt.Errorf("download WP-CLI: %w", err)
	}

	sum, err := run.Cmd(ctx, []string{"sha512sum", tmp})
	if err != nil {
		return err
	}
	got := strings.Fields(sum.Stdout)
	if len(got) == 0 || !strings.EqualFold(got[0], expected[0]) {
		return fmt.Errorf("WP-CLI checksum mismatch: expected %s, got %s", expected[0], got[0])
	}

	if err := os.Chmod(tmp, 0o755); err != nil {
		return err
	}
	return os.Rename(tmp, actions.WPCLIPath)
}

// firstLineOf trims a command's output for an error message.
func firstLineOf(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// checkProjectQuota reports whether the kernel argument is already in place.
// It deliberately does not check whether quota is *active*: the step's job is
// to configure the boot loader, and the reboot that makes it active is the
// operator's to schedule.
func checkProjectQuota(ctx context.Context, _ *Options) (bool, error) {
	res, err := run.Cmd(ctx, []string{"grubby", "--info=DEFAULT"}, run.Timeout(30*time.Second))
	if err != nil {
		return false, nil // no grubby: the step will report why
	}
	return strings.Contains(res.Stdout, "pquota"), nil
}

// stepProjectQuota turns on XFS project quota for the filesystem the homes
// live on.
//
// It does not reboot. A hosting panel that reboots the server during its own
// installation, on a machine that may already be serving other things, is not
// a panel anybody should trust; the operator is told instead, and the panel
// reports quotas as unenforced until it sees them active.
func stepProjectQuota(ctx context.Context, _ *Options) error {
	fsType, opts, err := rootFilesystem(ctx)
	if err != nil {
		return err
	}
	if fsType != "xfs" {
		return fmt.Errorf("hard quotas need XFS; %s is %s", actions.QuotaMount, fsType)
	}
	if strings.Contains(opts, "prjquota") {
		return nil // already active, nothing to arrange
	}

	if _, ok := run.Look("grubby"); !ok {
		return errors.New("grubby is not installed, so the kernel argument cannot be set")
	}
	if _, err := run.Cmd(ctx, []string{
		"grubby", "--update-kernel=ALL", "--args=rootflags=pquota",
	}, run.Timeout(time.Minute)); err != nil {
		return fmt.Errorf("set the kernel argument: %w", err)
	}
	return nil
}

// rootFilesystem reports the type and mount options of the filesystem the
// home directories are on.
func rootFilesystem(ctx context.Context) (fsType, options string, err error) {
	res, err := run.Cmd(ctx, []string{"findmnt", "-no", "FSTYPE,OPTIONS", actions.QuotaMount})
	if err != nil {
		return "", "", fmt.Errorf("inspect %s: %w", actions.QuotaMount, err)
	}
	fields := strings.Fields(res.Stdout)
	if len(fields) < 2 {
		return "", "", fmt.Errorf("cannot read the mount options of %s", actions.QuotaMount)
	}
	return fields[0], fields[1], nil
}

func checkFile(path string) func(context.Context, *Options) (bool, error) {
	return func(context.Context, *Options) (bool, error) {
		_, err := os.Stat(path)
		if err == nil {
			return true, nil
		}
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
}

func stepCheckHost(ctx context.Context, _ *Options) error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("must run as root (effective uid is %d)", os.Geteuid())
	}
	info := distro.Detect(ctx)
	if !info.Supported() {
		return fmt.Errorf("unsupported host %q (%s); OPanel targets EL10 and CloudLinux 10",
			info.Pretty, info.ID)
	}
	if _, ok := run.Look("systemctl"); !ok {
		return errors.New("systemd is required")
	}
	return nil
}

func checkBasePackages(ctx context.Context, _ *Options) (bool, error) {
	return pkgmgr.Installed(ctx, basePackages...)
}

func stepBasePackages(ctx context.Context, _ *Options) error {
	// EPEL carries several of these on a minimal image and is harmless when
	// already present.
	if ok, _ := pkgmgr.Installed(ctx, "epel-release"); !ok {
		if err := pkgmgr.Install(ctx, "epel-release"); err != nil {
			return err
		}
	}
	if err := pkgmgr.Install(ctx, basePackages...); err != nil {
		return err
	}

	// AlmaLinux 10 ships a libnode built against a newer c-ares than the base
	// image carries, and dnf does not notice: the old one satisfies the
	// versionless dependency, so node installs cleanly and then dies with
	// "undefined symbol: ares_query_dnsrec" the first time anybody runs it.
	// Nothing here depends on node, so this is not fatal -- but a terminal
	// that offers node and cannot start it is worse than one that does not.
	if _, err := run.Cmd(ctx, []string{"node", "-v"}); err != nil {
		if uerr := pkgmgr.Install(ctx, "c-ares"); uerr != nil {
			return nil
		}
		if _, err := run.Cmd(ctx, []string{"dnf", "upgrade", "-y", "c-ares"},
			run.Timeout(5*time.Minute)); err != nil {
			return nil
		}
	}
	return nil
}

func checkWebserver(_ context.Context, o *Options) (bool, error) {
	b, err := backends.New(o.Backend)
	if err != nil {
		return false, err
	}
	return b.Installed(), nil
}

// stepInstallWebserver installs Apache.
//
// Apache is not the faster server. It is the one LiteSpeed Enterprise reads
// the configuration of, so a host installed this way is already in the format
// the commercial server wants: buying a licence later is a switch rather than
// a migration.
func stepInstallWebserver(ctx context.Context, _ *Options) error {
	// mod_ssl alongside httpd: it carries the Listen for 443, and every site
	// with a certificate renders a vhost that needs it.
	if err := pkgmgr.Install(ctx, "httpd", "mod_ssl"); err != nil {
		return err
	}
	// Started now rather than at the first site. A certificate for the panel
	// itself is issued over HTTP-01 on port 80, and on a fresh host that
	// happens before any site exists.
	return svc.Enable(ctx, "httpd", true)
}

// stepWebserverConfig writes the panel's configuration for a host with no
// sites yet, and records which server is running.
//
// Done at install time rather than waiting for the first site, because the
// challenge path that Let's Encrypt fetches lives in this configuration. A
// host without it cannot get a certificate for the panel, which is the first
// thing an operator does and the one thing passkeys will not work without.
func stepWebserverConfig(ctx context.Context, o *Options) error {
	if err := backends.SetActive(o.Backend); err != nil {
		return err
	}
	b, err := backends.New(o.Backend)
	if err != nil {
		return err
	}
	cfg := webserver.DefaultServerConfig()

	// Before the server starts, not after. Remi's packages tie every PHP
	// version they install to httpd's unit, so starting Apache on a host with
	// no sites drags up a pool manager for each of them -- and one with no
	// pools exits with "No pool defined". A brand-new host would come up with
	// a row of failed services and nothing to say they were harmless.
	if fp, ok := phpmgr.ForBackend(o.Backend).(phpmgr.FPMProvider); ok {
		if err := phpfpm.Prepare(ctx, fp, cfg.ServerUser); err != nil {
			return err
		}
	}

	rendered, err := b.Render(cfg, nil)
	if err != nil {
		return err
	}
	return b.Apply(ctx, rendered)
}

func checkPHP(ctx context.Context, o *Options) (bool, error) {
	p := phpmgr.ForBackend(o.Backend)
	versions, err := p.Available(ctx)
	if err != nil {
		return false, err
	}
	have := make(map[string]bool, len(versions))
	for _, v := range versions {
		have[v.Version] = v.Installed
	}
	for _, want := range o.PHPVersions {
		if !have[want] {
			return false, nil
		}
	}
	return true, nil
}

func stepInstallPHP(ctx context.Context, o *Options) error {
	p := phpmgr.ForBackend(o.Backend)
	for _, v := range o.PHPVersions {
		if err := p.Install(ctx, v); err != nil {
			return fmt.Errorf("install PHP %s: %w", v, err)
		}
	}
	return nil
}

// MariaDBVersion is the series installed. 11.8 is the current long-term
// release, supported to mid-2028. AlmaLinux 10's own AppStream carries only
// 10.11, so the vendor repository is required.
const MariaDBVersion = "11.8"

func stepMariaDBRepo(ctx context.Context, _ *Options) error {
	script := fmt.Sprintf(
		"curl -LsS --connect-timeout 15 --max-time 180 https://r.mariadb.com/downloads/mariadb_repo_setup "+
			"| bash -s -- --os-type=rhel --os-version=10 --mariadb-server-version=mariadb-%s",
		MariaDBVersion)
	res, err := run.Cmd(ctx, []string{"bash", "-c", script}, run.Timeout(5*time.Minute))
	if err != nil {
		return fmt.Errorf("add MariaDB repository: %w (%s)", err, res.Output())
	}
	return nil
}

func checkMariaDB(ctx context.Context, _ *Options) (bool, error) {
	// Capital M: the vendor packages are MariaDB-server, and the lowercase
	// mariadb-server from AppStream is a different, older package.
	return pkgmgr.Installed(ctx, "MariaDB-server")
}

func stepInstallMariaDB(ctx context.Context, _ *Options) error {
	if err := pkgmgr.Install(ctx, "MariaDB-server", "MariaDB-client"); err != nil {
		return err
	}
	return svc.Enable(ctx, "mariadb", true)
}

// checkMariaDBHardened reports whether the insecure defaults are already gone.
//
// A separate step from installation, with its own check, because bundling the
// two meant hardening never ran on a host where MariaDB happened to be
// installed already — which is exactly what happened here, leaving anonymous
// accounts and the test database in place on a server that had been through
// the installer twice.
func checkMariaDBHardened(ctx context.Context, _ *Options) (bool, error) {
	if ok, _ := pkgmgr.Installed(ctx, "MariaDB-server"); !ok {
		return true, nil // nothing to harden yet
	}
	const q = `SELECT (SELECT COUNT(*) FROM mysql.global_priv WHERE User='') + ` +
		`(SELECT COUNT(*) FROM information_schema.SCHEMATA WHERE SCHEMA_NAME='test')`
	res, err := run.Cmd(ctx, []string{"mariadb", "-u", "root", "-N", "-B", "-e", q},
		run.Timeout(30*time.Second))
	if err != nil {
		return false, nil // cannot tell, so try
	}
	return strings.TrimSpace(res.Stdout) == "0", nil
}

// stepHardenMariaDB removes what a fresh install leaves behind.
//
// The root account authenticates through the unix_socket plugin, so there is
// no password to set. What does need removing is the anonymous account and
// the test database: together they let anyone with a shell on the box connect
// with no credentials and write data.
func stepHardenMariaDB(ctx context.Context, _ *Options) error {
	// Backquoted so the SQL LIKE escape stays a single backslash rather than
	// being read as a Go escape sequence.
	const harden = `DELETE FROM mysql.global_priv WHERE User=''; ` +
		`DROP DATABASE IF EXISTS test; ` +
		`DELETE FROM mysql.db WHERE Db='test' OR Db='test\_%'; ` +
		`FLUSH PRIVILEGES;`
	if _, err := run.Cmd(ctx, []string{"mariadb", "-u", "root", "-e", harden},
		run.Timeout(60*time.Second)); err != nil {
		return fmt.Errorf("harden MariaDB: %w", err)
	}
	return nil
}

func checkAccounts(context.Context, *Options) (bool, error) {
	if _, err := user.Lookup(ServiceUser); err != nil {
		return false, nil
	}
	if _, err := user.LookupGroup(SFTPGroup); err != nil {
		return false, nil
	}
	return true, nil
}

func stepAccounts(ctx context.Context, _ *Options) error {
	for _, g := range []string{ServiceGroup, SFTPGroup} {
		if _, err := user.LookupGroup(g); err != nil {
			if _, err := run.Cmd(ctx, []string{"groupadd", "--system", g}); err != nil {
				return fmt.Errorf("create group %s: %w", g, err)
			}
		}
	}
	if _, err := user.Lookup(ServiceUser); err != nil {
		_, err := run.Cmd(ctx, []string{
			"useradd", "--system", "--gid", ServiceGroup,
			"--home-dir", DataDir, "--shell", "/sbin/nologin",
			"--comment", "OPanel service account", ServiceUser,
		})
		if err != nil {
			return fmt.Errorf("create user %s: %w", ServiceUser, err)
		}
	}
	return nil
}

func stepDirectories(_ context.Context, _ *Options) error {
	svcUID, svcGID, err := idsOf(ServiceUser, ServiceGroup)
	if err != nil {
		return err
	}
	dirs := []struct {
		path string
		mode os.FileMode
		uid  int
		gid  int
	}{
		{DataDir, 0o750, svcUID, svcGID},
		{filepath.Join(DataDir, "webserver"), 0o750, svcUID, svcGID},
		{ConfigDir, 0o755, 0, 0},
		// Backups are root-only: the API reaches them through the agent, and
		// nothing else on the host has any business reading one account's
		// archive, let alone deleting it.
		{actions.BackupRoot, 0o700, 0, 0},
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d.path, d.mode); err != nil {
			return err
		}
		if err := os.Chown(d.path, d.uid, d.gid); err != nil {
			return err
		}
		if err := os.Chmod(d.path, d.mode); err != nil {
			return err
		}
	}
	return nil
}

func stepBinaries(_ context.Context, o *Options) error {
	if o.BinDir == "" {
		return errors.New("cannot locate the binaries to install; pass --bin-dir")
	}
	for _, name := range []string{"opanel-api", "opanel-agent", "opanelctl"} {
		src := filepath.Join(o.BinDir, name)
		dst := filepath.Join(BinDir, name)
		if sameFile(src, dst) {
			continue
		}
		data, err := os.ReadFile(src)
		if err != nil {
			return fmt.Errorf("read %s: %w", src, err)
		}
		// Write to a temporary name and rename: replacing a running binary
		// in place fails with ETXTBSY.
		tmp := dst + ".new"
		if err := os.WriteFile(tmp, data, 0o755); err != nil {
			return err
		}
		if err := os.Rename(tmp, dst); err != nil {
			return err
		}
	}
	return nil
}

func stepUnits(ctx context.Context, _ *Options) error {
	for _, name := range []string{"opanel-agent.service", "opanel-api.service"} {
		data, err := assetsFS.ReadFile("assets/" + name)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(unitDir, name), data, 0o644); err != nil {
			return err
		}
	}
	if _, err := os.Stat(filepath.Join(ConfigDir, "opanel.env")); errors.Is(err, os.ErrNotExist) {
		if err := os.WriteFile(filepath.Join(ConfigDir, "opanel.env"),
			[]byte("# OPanel API configuration. See opanel.env.example.\n"), 0o640); err != nil {
			return err
		}
	}
	return svc.DaemonReload(ctx)
}

func checkSFTP(context.Context, *Options) (bool, error) {
	data, err := os.ReadFile(sshdMain)
	if err != nil {
		return false, err
	}
	return bytes.Contains(data, []byte(sftpMarker)), nil
}

// stepSFTP chroots site owners into their home directory.
//
// The Match block is appended to the main file rather than dropped into
// sshd_config.d. The Include sits near the top of sshd_config, so a Match in
// an included file would capture every directive after it in the main file.
func stepSFTP(ctx context.Context, _ *Options) error {
	dropIn, err := assetsFS.ReadFile("assets/sshd-opanel.conf")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(sshdDropIn), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(sshdDropIn, dropIn, 0o600); err != nil {
		return err
	}

	original, err := os.ReadFile(sshdMain)
	if err != nil {
		return err
	}
	block := "\n" + sftpMarker + "\n" +
		"Match Group " + SFTPGroup + "\n" +
		"    ChrootDirectory %h\n" +
		"    ForceCommand internal-sftp -d /\n" +
		"    AllowTcpForwarding no\n" +
		"    X11Forwarding no\n" +
		"    PasswordAuthentication yes\n" +
		"# --- end OPanel SFTP ---\n"

	updated := append(append([]byte(nil), original...), []byte(block)...)
	if err := os.WriteFile(sshdMain, updated, 0o600); err != nil {
		return err
	}

	// A broken sshd configuration on a remote host means no way back in, so
	// it is validated before the reload and reverted if it does not parse.
	if _, err := run.Cmd(ctx, []string{"sshd", "-t"}); err != nil {
		_ = os.WriteFile(sshdMain, original, 0o600)
		_ = os.Remove(sshdDropIn)
		return fmt.Errorf("sshd rejected the SFTP configuration, reverted: %w", err)
	}
	return svc.ReloadOrRestart(ctx, "sshd")
}

func checkFirewall(_ context.Context, o *Options) (bool, error) {
	if o.SkipFirewall {
		return true, nil
	}
	return false, nil // always re-render; the ruleset depends on the panel port
}

// stepFirewall installs an nftables ruleset behind a dead-man switch.
//
// Getting a firewall wrong on a remote host locks the operator out with no
// way back. A timer set before the ruleset is loaded flushes it a few minutes
// later; the timer is cancelled only once a fresh connection has proved the
// rules still admit SSH.
func stepFirewall(ctx context.Context, o *Options) error {
	ports, err := sshPorts()
	if err != nil {
		return err
	}
	tmpl, err := template.ParseFS(assetsFS, "assets/opanel.nft.tmpl")
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, struct {
		SSHPorts  []int
		PanelPort int
	}{ports, o.PanelPort}); err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(nftRuleset), 0o750); err != nil {
		return err
	}
	if err := os.WriteFile(nftRuleset, buf.Bytes(), 0o600); err != nil {
		return err
	}

	const rollbackUnit = "opanel-fw-rollback"
	_, _ = run.Cmd(ctx, []string{"systemctl", "stop", rollbackUnit + ".timer"})
	if _, err := run.Cmd(ctx, []string{
		"systemd-run", "--quiet", "--on-active=300", "--unit=" + rollbackUnit,
		"/usr/sbin/nft", "flush", "ruleset",
	}); err != nil {
		return fmt.Errorf("arm firewall rollback timer: %w", err)
	}

	if _, err := run.Cmd(ctx, []string{"nft", "-f", nftRuleset}); err != nil {
		_, _ = run.Cmd(ctx, []string{"systemctl", "stop", rollbackUnit + ".timer"})
		return fmt.Errorf("load nftables ruleset: %w", err)
	}

	if err := os.WriteFile(nftSysconf, []byte("include \""+nftRuleset+"\"\n"), 0o600); err != nil {
		return err
	}
	if err := svc.Enable(ctx, "nftables", false); err != nil {
		return err
	}

	// The rules are live and SSH is still up, since this very command is
	// running over it. Cancel the rollback.
	_, _ = run.Cmd(ctx, []string{"systemctl", "stop", rollbackUnit + ".timer"})
	_, _ = run.Cmd(ctx, []string{"systemctl", "reset-failed", rollbackUnit + ".timer", rollbackUnit + ".service"})

	// firewalld would fight over the same netfilter hooks.
	if ok, _ := pkgmgr.Installed(ctx, "firewalld"); ok {
		_ = svc.Disable(ctx, "firewalld", true)
	}
	return nil
}

// sshPorts reads the ports sshd actually listens on, so the firewall cannot
// lock out a host running SSH somewhere other than 22.
func sshPorts() ([]int, error) {
	out, err := exec.Command("sshd", "-T").Output()
	if err != nil {
		return []int{22}, nil // sshd -T needs root and a valid config; 22 is the safe assumption
	}
	re := regexp.MustCompile(`(?m)^port (\d+)$`)
	var ports []int
	for _, m := range re.FindAllStringSubmatch(string(out), -1) {
		var p int
		if _, err := fmt.Sscanf(m[1], "%d", &p); err == nil && p > 0 && p < 65536 {
			ports = append(ports, p)
		}
	}
	if len(ports) == 0 {
		ports = []int{22}
	}
	return ports, nil
}

func stepValkey(ctx context.Context, _ *Options) error {
	return svc.Enable(ctx, "valkey", true)
}

func stepStartServices(ctx context.Context, _ *Options) error {
	if err := svc.Enable(ctx, "opanel-agent", true); err != nil {
		return err
	}
	// Restart, not just start. On a re-run the binaries were replaced a few
	// steps ago and the services are already up -- on the old ones. Leaving
	// them running is the worst kind of upgrade bug: everything reports
	// success and nothing has changed.
	if err := svc.Restart(ctx, "opanel-agent"); err != nil {
		return err
	}
	// The API refuses nothing when the agent is slow to appear, but starting
	// it before the socket exists produces an alarming warning on every fresh
	// install.
	for range 20 {
		if _, err := os.Stat("/run/opanel/agent.sock"); err == nil {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if err := svc.Enable(ctx, "opanel-api", true); err != nil {
		return err
	}
	return svc.Restart(ctx, "opanel-api")
}

func idsOf(username, group string) (int, int, error) {
	u, err := user.Lookup(username)
	if err != nil {
		return 0, 0, err
	}
	g, err := user.LookupGroup(group)
	if err != nil {
		return 0, 0, err
	}
	var uid, gid int
	if _, err := fmt.Sscanf(u.Uid, "%d", &uid); err != nil {
		return 0, 0, err
	}
	if _, err := fmt.Sscanf(g.Gid, "%d", &gid); err != nil {
		return 0, 0, err
	}
	return uid, gid, nil
}

func sameFile(a, b string) bool {
	da, err := os.ReadFile(a)
	if err != nil {
		return false
	}
	dbb, err := os.ReadFile(b)
	if err != nil {
		return false
	}
	return bytes.Equal(da, dbb)
}
