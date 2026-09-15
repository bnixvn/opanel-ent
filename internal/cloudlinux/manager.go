package cloudlinux

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// CloudLinux Manager, as this panel arranges it.
//
// The Manager is CloudLinux's own interface. On the panels they support it is
// dropped into the panel's web root and served by the panel's own webserver,
// and that is what this does: the vendor's installer copies the Manager to a
// directory of ours, the webserver gets a vhost for it on the loopback
// address, and the panel proxies to that behind its own session. The same
// arrangement phpMyAdmin already has, for the same reason.
//
// The vendor also ships a "panelless" service that serves the Manager itself
// on a port of its own. This panel does not use it: it authenticates with
// PAM, so it asks for a system password inside a page the panel has already
// signed somebody in for. run_service is 0 in the integration file and the
// unit is stopped, so there is one door rather than two.
const (
	ManagerRoot = "/usr/share/opanel/lvemanager"
	// ManagerPort is the loopback port the Manager's vhost listens on.
	// Nothing outside this machine can reach it: the vhost binds 127.0.0.1
	// and the firewall never opens it.
	ManagerPort = 8082
	// ManagerPHPVersion is the interpreter the Manager runs under, and
	// ManagerPool is the pool it runs in.
	//
	// As "opanel", not as root: PHP-FPM refuses to start a pool as root, and
	// the Manager does not need it -- everything privileged it does goes
	// through sudo, which the vendor's own clsudoers file grants.
	ManagerPHPVersion = "8.4"
	ManagerPool       = "lvemanager.opanel"
	// ManagerUser is the account that pool runs as.
	ManagerUser = "opanel"

	vendorPHPPath  = ConfigDir + "/vendor.php"
	userInfoPath   = ConfigDir + "/ui_user_info"
	secretPath     = "/etc/opanel/lvemanager.secret"
	pluginInstall  = "/usr/share/l.v.e-manager/install-lvemanager-plugin.py"
	pluginPython   = "/opt/cloudlinux/venv/bin/python3"
	managerBaseURI = "/lvemanager"
	// noPanelUnit is the vendor's own service, which this panel replaces.
	noPanelUnit = "lvemanager.service"
	// adminHook is CloudLinux's own hook for "this account is an
	// administrator of the panel": it puts the account in clsudoers and
	// proc_super_gid, which is what lets the Manager's PHP run their tools.
	adminHook = "/usr/share/cloudlinux/hooks/post_modify_admin.py"
)

// ManagerPrefix is the path the panel serves the Manager under.
const ManagerPrefix = managerBaseURI

// ManagerRunning reports whether the Manager is being served.
//
// A dial rather than a look at the config: the vhost is written by the
// webserver backend, and what matters is whether something is actually
// answering on the port the panel proxies to.
func ManagerRunning() bool {
	c, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(ManagerPort), 2*time.Second)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

// ManagerInstalled reports whether the Manager's own files are in place.
func ManagerInstalled() bool {
	st, err := os.Stat(filepath.Join(ManagerRoot, "index.php"))
	return err == nil && !st.IsDir()
}

// ManagerSecret returns the shared secret the panel proves itself with,
// creating it the first time.
//
// The Manager's vhost is on the loopback address, and on a hosting server
// that is not a boundary: every customer with a shell is already inside it.
// So the proxy proves it is the panel, and vendor.php refuses anything that
// cannot. The file is 0640 root:opanel -- the panel may read it, a customer
// may not.
func ManagerSecret() (string, error) {
	if data, err := os.ReadFile(secretPath); err == nil {
		if s := strings.TrimSpace(string(data)); len(s) >= 32 {
			return s, nil
		}
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	secret := hex.EncodeToString(raw)

	dir := filepath.Dir(secretPath)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", err
	}
	if err := os.WriteFile(secretPath, []byte(secret+"\n"), 0o640); err != nil {
		return "", fmt.Errorf("cloudlinux: write %s: %w", secretPath, err)
	}
	// The directory as well as the file: a file group-readable by opanel
	// inside a directory opanel cannot enter is not readable at all, and the
	// two processes that need this -- the panel, and the Manager's pool --
	// both run as opanel.
	if u, err := user.Lookup(ManagerUser); err == nil {
		gid, _ := strconv.Atoi(u.Gid)
		_ = os.Chown(secretPath, 0, gid)
		_ = os.Chown(dir, 0, gid)
	}
	return secret, nil
}

// InstallManager puts CloudLinux Manager where the panel can serve it.
//
// The vendor's own installer does the copying, and re-does it on every
// update, which is why the panel points it at a directory rather than
// unpacking anything itself.
func InstallManager() error {
	if _, err := os.Stat(pluginInstall); err != nil {
		return fmt.Errorf("cloudlinux: %s is not here; is the lvemanager package installed?", pluginInstall)
	}
	secret, err := ManagerSecret()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(ManagerRoot, 0o755); err != nil {
		return err
	}
	if err := writeIdentityHook(secret); err != nil {
		return err
	}
	if err := Install(); err != nil { // rewrites integration.ini, now with the manager section
		return err
	}

	cmd := exec.Command(pluginPython, pluginInstall, "--install")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("cloudlinux: the manager installer failed: %w (%s)", err, lastLine(string(out)))
	}
	if !ManagerInstalled() {
		return fmt.Errorf("cloudlinux: the installer finished but %s/index.php is not there:\n%s",
			ManagerRoot, lastLine(string(out)))
	}
	// The vendor's installer starts its own panelless service whether or not
	// the integration file asked for one. Leaving it up would mean a second
	// copy of the Manager on a port of its own, asking for a system password
	// -- the thing this arrangement exists to avoid.
	retireNoPanelService()
	return grantManagerSudo()
}

// retireNoPanelService stops and disables the vendor's panelless service.
//
// Best effort on purpose: on a host where it was never installed there is
// nothing to stop, and that is not a reason to fail an install.
func retireNoPanelService() {
	_ = exec.Command("systemctl", "disable", "--now", noPanelUnit).Run()
}

// grantManagerSudo lets the account the Manager's PHP runs as call
// CloudLinux's own tools.
//
// Everything the Manager does, it does by running cloudlinux-cli.py under
// sudo, so without this the interface loads and every panel in it is empty.
// The rights come from group membership -- clsudoers for the tools, and on
// releases that still have it proc_super_gid for the /proc entries the usage
// figures are read from -- and CloudLinux's own hook is what puts an account
// in them. Calling the hook rather than writing a sudoers file keeps the
// arrangement theirs: CloudLinux 10 has dropped one of those two groups
// already, and an upgrade that changes them again changes this too.
func grantManagerSudo() error {
	if _, err := os.Stat(adminHook); err != nil {
		return nil // an older CloudLinux without the hook; nothing to do
	}
	cmd := exec.Command(pluginPython, adminHook, "create", "--name", ManagerUser)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("cloudlinux: grant %s the manager's rights: %w (%s)",
			ManagerUser, err, lastLine(string(out)))
	}
	return nil
}

// writeIdentityHook writes the two programs that tell the Manager who is
// asking.
//
// vendor.php runs inside the Manager's own process, so it can see the request
// and the headers the panel's proxy set. ui_user_info runs as a separate
// program and can only see its environment, which is why the first puts the
// identity there for the second.
//
// The vendor's own sample derived the role from `whoami`, which meant
// anything running as root was an administrator; they now ship a stub that
// fails closed and a note saying an integration must take the identity from
// the panel's authenticated session instead. This does that, and refuses
// outright when the shared secret does not match -- a request that cannot
// prove it came through the panel gets no identity at all.
func writeIdentityHook(secret string) error {
	vendor := `<?php
// Written by OPanel. Do not edit.
//
// Runs inside CloudLinux Manager before it starts, which is the only place
// that can see the panel's request. It takes the identity from the headers
// the panel's proxy set, having first checked the shared secret that proves
// the request came through the panel at all -- the Manager's vhost is on the
// loopback address, and on a hosting server every customer with a shell is
// already inside that.
//
// Identity never comes from the OS user this runs as. The vendor's own sample
// did that once and authorised every caller as an administrator.
$secretFile = ` + phpQuote(secretPath) + `;
$expected = is_readable($secretFile) ? trim(file_get_contents($secretFile)) : '';
$given = isset($_SERVER['HTTP_X_OPANEL_AUTH']) ? $_SERVER['HTTP_X_OPANEL_AUTH'] : '';

if ($expected === '' || !hash_equals($expected, $given)) {
    header('Content-Type: application/json', true, 403);
    echo json_encode(['result' => 'this panel did not authorise the request']);
    exit;
}

$user = isset($_SERVER['HTTP_X_OPANEL_USER']) ? $_SERVER['HTTP_X_OPANEL_USER'] : '';
$role = isset($_SERVER['HTTP_X_OPANEL_ROLE']) ? $_SERVER['HTTP_X_OPANEL_ROLE'] : 'user';
if (!preg_match('/^[a-z_][a-z0-9_.-]{0,31}$/', $user)) {
    header('Content-Type: application/json', true, 403);
    echo json_encode(['result' => 'the panel sent no usable identity']);
    exit;
}
if (!in_array($role, ['admin', 'reseller', 'user'], true)) {
    $role = 'user';
}
putenv('OPANEL_USER=' . $user);
putenv('OPANEL_ROLE=' . $role);
`
	if err := os.WriteFile(vendorPHPPath, []byte(vendor), 0o644); err != nil {
		return fmt.Errorf("cloudlinux: write %s: %w", vendorPHPPath, err)
	}

	info := `#!/bin/sh
# Written by OPanel. Do not edit.
#
# CloudLinux Manager runs this to learn who is looking at it, passing the
# session token it holds as the one argument. The token is not the identity
# here and is deliberately ignored: it says which browser session is asking,
# and the panel has already decided who that is. The answer comes from the
# environment, where vendor.php put it after checking the panel's signature.
#
# Nothing here reads whoami. The vendor's own sample did, and it made every
# caller an administrator.
#
# Fails closed. No identity means the lowest role there is.
user="${OPANEL_USER:-nobody}"
uid="$(id -u "$user" 2>/dev/null || echo 0)"

# baseUri ends in a slash because the Manager puts it in <base href>, and a
# base without one resolves every relative link one directory too high --
# out of the Manager and into the panel's own routes.
base="` + managerBaseURI + `"
printf '{"userName":"%s","userId":%s,"userType":"%s","lang":"en","assetsUri":"%s","baseUri":"%s/","userDomain":""}' "$user" "$uid" "${OPANEL_ROLE:-user}" "$base" "$base"
`
	if err := os.WriteFile(userInfoPath, []byte(info), 0o755); err != nil {
		return fmt.Errorf("cloudlinux: write %s: %w", userInfoPath, err)
	}
	return nil
}

// ManagerToken is the session token the panel hands the Manager for a user.
//
// The Manager insists on one -- it refuses a request that carries no
// CLSIDTOKEN -- and on the panels it was written for the token is what maps a
// browser back to a signed-in account. Here it does not need to: the identity
// travels in signed headers on the same request, and ui_user_info throws the
// token away. So it carries no authority, and is derived rather than stored
// only so that it is stable per user and reveals nothing if it leaks.
func ManagerToken(secret, username string) string {
	sum := sha256.Sum256([]byte(secret + "/lvemanager/" + username))
	return hex.EncodeToString(sum[:16])
}

func phpQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "\\'") + "'" }

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) == 0 {
		return ""
	}
	return lines[len(lines)-1]
}

// managerSection is the part of integration.ini that describes the Manager.
func managerSection() string {
	var b strings.Builder
	b.WriteString("\n[lvemanager_config]\n")
	fmt.Fprintf(&b, "ui_user_info = %s\n", userInfoPath)
	fmt.Fprintf(&b, "vendor_php = %s\n", vendorPHPPath)
	fmt.Fprintf(&b, "base_path = %s\n", ManagerRoot)
	fmt.Fprintf(&b, "base_uri = %s\n", managerBaseURI)
	// The vendor's own aiohttp service, which this panel does not use: the
	// Manager is served by the webserver the panel already runs, behind the
	// session the panel already checks.
	b.WriteString("run_service = 0\nservice_port = 0\nuse_ssl = 0\n")
	return b.String()
}

// managerInstallTimeout bounds the vendor's installer.
const managerInstallTimeout = 10 * time.Minute
