package cloudlinux

import (
	"crypto/rand"
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
// dropped into the panel's web root; for everybody else the vendor ships a
// "panelless" copy, an installer that puts it where the integration file
// says, and a service that serves it. This panel uses that service and
// proxies to it, which is the same arrangement phpMyAdmin already has and for
// the same reason: the Manager signs people in with system accounts, and a
// web login for root does not belong on a public port.
const (
	ManagerRoot = "/usr/share/opanel/lvemanager"
	// ManagerPort is where the vendor's own service listens. It binds every
	// interface, which is safe here only because the panel's firewall opens
	// four ports and this is not one of them -- the panel reaches it at
	// 127.0.0.1 and nothing outside the machine can.
	ManagerPort = 2223

	vendorPHPPath  = ConfigDir + "/vendor.php"
	userInfoPath   = ConfigDir + "/ui_user_info"
	secretPath     = "/etc/opanel/lvemanager.secret"
	pluginInstall  = "/usr/share/l.v.e-manager/install-lvemanager-plugin.py"
	pluginPython   = "/opt/cloudlinux/venv/bin/python3"
	managerBaseURI = "/lvemanager"
)

// ManagerPrefix is the path the panel serves the Manager under.
const ManagerPrefix = managerBaseURI

// ManagerRunning reports whether the vendor's own service is up. It is the
// one that serves the Manager here: run_service in the integration file.
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

	if err := os.MkdirAll(filepath.Dir(secretPath), 0o750); err != nil {
		return "", err
	}
	if err := os.WriteFile(secretPath, []byte(secret+"\n"), 0o640); err != nil {
		return "", fmt.Errorf("cloudlinux: write %s: %w", secretPath, err)
	}
	if u, err := user.Lookup("opanel"); err == nil {
		uid, _ := strconv.Atoi(u.Uid)
		gid, _ := strconv.Atoi(u.Gid)
		_ = os.Chown(secretPath, 0, gid)
		_ = uid
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
# CloudLinux Manager runs this to learn who is looking at it. The identity is
# in the environment because vendor.php put it there, having taken it from the
# panel session that authenticated the request. Nothing here reads whoami: the
# vendor's own sample did, and it made every caller an administrator.
#
# Fails closed. No identity means the lowest role there is.
printf '{"userName":"%s","userType":"%s","lang":"en","assetsUri":".","baseUri":"%s","defaultDomain":""}\n' \
  "${OPANEL_USER:-nobody}" "${OPANEL_ROLE:-user}" "` + managerBaseURI + `"
`
	if err := os.WriteFile(userInfoPath, []byte(info), 0o755); err != nil {
		return fmt.Errorf("cloudlinux: write %s: %w", userInfoPath, err)
	}
	return nil
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
