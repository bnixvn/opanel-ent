package actions

import (
	"os"
	"strings"
)

// What LiteSpeed Enterprise can run a site's PHP with.
//
// LiteSpeed reads Apache's configuration but not Apache's PHP handler: a
// SetHandler pointing at a PHP-FPM socket means nothing to it. What it does
// understand is AddHandler naming one of its own, and on CloudLinux those are
// the alt-php builds -- application/x-httpd-alt-php84 runs
// /opt/alt/php84, as the site's own user, under LVE.
//
// Measured on the host rather than taken from documentation, because the
// three outcomes are not what the documentation implies and the difference
// decides whether a switch is safe:
//
//   - handler naming an installed alt-php: runs it. Confirmed by the ini path
//     the script sees, /opt/alt/php84/etc/php.ini, and by the uid.
//   - handler naming a version that is not installed: 403. Fails closed.
//   - no handler at all: LiteSpeed runs the lsphp it ships with, 7.2.34, and
//     returns 200. It does not serve the source -- but a site written for 8.4
//     running on 7.2 without a word is its own kind of damage, and it is the
//     reason a site with no handler still blocks the switch.
const altPHPRoot = "/opt/alt"

// lsphpHandler returns the handler LiteSpeed should run this version under,
// or empty when the host has no alt-php for it.
//
// The binary is what is checked, not the directory: installing part of an
// alt-php group leaves /opt/alt/phpNN behind with no interpreter in it, which
// is how this host had thirteen directories and three usable versions.
func lsphpHandler(version string) string {
	tag := altPHPTag(version)
	if tag == "" {
		return ""
	}
	if _, err := os.Stat(altPHPRoot + "/php" + tag + "/usr/bin/lsphp"); err != nil {
		return ""
	}
	return "application/x-httpd-alt-php" + tag
}

// altPHPTag turns "8.4" into "84", the form alt-php names things with.
//
// Returns empty for anything that is not two numeric parts. A version reaches
// here from the database, and this value ends up in a handler name in a
// config file, so the shape is checked rather than assumed.
func altPHPTag(version string) string {
	major, minor, ok := strings.Cut(version, ".")
	if !ok || major == "" || minor == "" {
		return ""
	}
	for _, part := range []string{major, minor} {
		for _, r := range part {
			if r < '0' || r > '9' {
				return ""
			}
		}
	}
	return major + minor
}
