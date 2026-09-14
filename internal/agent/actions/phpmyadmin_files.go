package actions

import "fmt"

// pmaConfigFile renders phpMyAdmin's configuration.
//
// auth_type is signon, not cookie: with signon there is no login form on
// phpMyAdmin at all, so it cannot be used to guess database passwords, and
// the only way in is a ticket the panel issued to somebody it had already
// authenticated.
func pmaConfigFile(secret string) string {
	return fmt.Sprintf(`<?php
// Managed by OPanel. Rewritten whenever phpMyAdmin is installed.

declare(strict_types=1);

$cfg['blowfish_secret'] = '%s';

$i = 1;
$cfg['Servers'][$i]['auth_type']       = 'signon';
$cfg['Servers'][$i]['SignonSession']   = 'opanel_pma';
// Browser-facing, so they carry the path the panel proxies phpMyAdmin at
// rather than the path inside the vhost.
$cfg['Servers'][$i]['SignonURL']       = '/phpmyadmin/opanel-sso.php';
$cfg['Servers'][$i]['LogoutURL']       = '/databases';
$cfg['Servers'][$i]['host']            = '127.0.0.1';
// Without this the page title is the loopback address and port, which tells
// the customer nothing and looks like a misconfiguration.
$cfg['Servers'][$i]['verbose']         = 'Databases';
$cfg['Servers'][$i]['compress']        = false;
$cfg['Servers'][$i]['AllowNoPassword'] = false;

// Each sign-in gets an account granted only on its owner's databases, so
// showing the whole list would just be a list of things it cannot open.
$cfg['Servers'][$i]['hide_db'] = '^(mysql|information_schema|performance_schema|sys)$';

$cfg['TempDir']            = '%s';
$cfg['ShowServerInfo']     = false;
$cfg['ShowPhpInfo']        = false;
$cfg['ShowChgPassword']    = false;
$cfg['AllowArbitraryServer'] = false;
$cfg['VersionCheck']       = false;
$cfg['SendErrorReports']   = 'never';
`, secret, pmaTmpDir)
}

// signonScript is the shim that turns a panel ticket into a phpMyAdmin
// session.
//
// It holds no credentials of its own. The ticket in the URL is worth nothing
// without the panel, which checks that it exists, has not expired and has
// not already been used, and only then hands back the throwaway account it
// was minted for.
const signonScript = `<?php
// Managed by OPanel. Rewritten whenever phpMyAdmin is installed.
//
// Turns a one-time ticket from the panel into a phpMyAdmin signon session.

declare(strict_types=1);

const PANEL_URL = 'https://127.0.0.1:2222/api/internal/sso/redeem';

function fail(string $why): never {
    http_response_code(403);
    header('Content-Type: text/html; charset=utf-8');
    echo '<!doctype html><meta charset="utf-8"><title>Sign in from the panel</title>';
    echo '<p style="font:16px system-ui;margin:3rem auto;max-width:30rem">';
    echo htmlspecialchars($why, ENT_QUOTES);
    echo '</p><p style="font:14px system-ui;margin:0 auto;max-width:30rem;color:#666">';
    echo 'Open phpMyAdmin from the control panel to get a fresh link.</p>';
    exit;
}

$token = $_GET['token'] ?? '';
if (!is_string($token) || !preg_match('/^[A-Za-z0-9_-]{20,128}$/', $token)) {
    fail('This link is not valid.');
}

// The panel serves its own certificate, and this request never leaves the
// machine, so pinning the loopback address is the check that matters rather
// than the certificate chain.
$ch = curl_init(PANEL_URL);
curl_setopt_array($ch, [
    CURLOPT_POST           => true,
    CURLOPT_POSTFIELDS     => json_encode(['token' => $token]),
    CURLOPT_HTTPHEADER     => ['Content-Type: application/json'],
    CURLOPT_RETURNTRANSFER => true,
    CURLOPT_TIMEOUT        => 10,
    CURLOPT_SSL_VERIFYPEER => false,
    CURLOPT_SSL_VERIFYHOST => 0,
]);
$body = curl_exec($ch);
$code = curl_getinfo($ch, CURLINFO_HTTP_CODE);
curl_close($ch);

if ($code !== 200 || !is_string($body)) {
    fail('This link has expired or has already been used.');
}
$data = json_decode($body, true);
if (!is_array($data) || empty($data['username']) || empty($data['password'])) {
    fail('This link has expired or has already been used.');
}

session_name('opanel_pma');
session_start();
session_regenerate_id(true);
$_SESSION['PMA_single_signon_user']     = $data['username'];
$_SESSION['PMA_single_signon_password'] = $data['password'];
$_SESSION['PMA_single_signon_host']     = '127.0.0.1';
session_write_close();

header('Location: index.php');
exit;
`
