package pkgmgr

import "testing"

func TestValidName(t *testing.T) {
	good := []string{"nftables", "MariaDB-server", "lsphp84-pecl-redis", "lsphp*", "valkey", "openlitespeed"}
	for _, s := range good {
		if !ValidName(s) {
			t.Errorf("ValidName(%q) = false, want true", s)
		}
	}
	// A leading dash would be read by dnf as an option even though it is a
	// separate argv element, so it must never pass validation.
	bad := []string{"", "--assumeyes", "-y", "pkg;rm -rf /", "pkg name", "pkg\nname", "../etc/passwd"}
	for _, s := range bad {
		if ValidName(s) {
			t.Errorf("ValidName(%q) = true, want false", s)
		}
	}
}

func TestParseList(t *testing.T) {
	// Real shape of "dnf -q list --available" on AlmaLinux 10.
	out := `Available Packages
openlitespeed.x86_64                1.9.2-3.el10                litespeed-update
lsphp84.x86_64                      8.4.25-1.el10               litespeed
MariaDB-server.x86_64               11.8.9-1.el10               mariadb-main
`
	pkgs := parseList(out)
	if len(pkgs) != 3 {
		t.Fatalf("parsed %d packages, want 3: %+v", len(pkgs), pkgs)
	}
	if pkgs[0].Name != "openlitespeed" || pkgs[0].Version != "1.9.2-3.el10" || pkgs[0].Repo != "litespeed-update" {
		t.Fatalf("first package parsed as %+v", pkgs[0])
	}
	if pkgs[2].Name != "MariaDB-server" {
		t.Fatalf("case in package name was not preserved: %q", pkgs[2].Name)
	}
}

func TestParseListIgnoresNoise(t *testing.T) {
	out := `Last metadata expiration check: 0:12:01 ago on Sun 14 Sep 2026.
Available Packages
Obsoleting Packages
nftables.x86_64   1:1.1.5-6.el10   appstream
`
	pkgs := parseList(out)
	if len(pkgs) != 1 || pkgs[0].Name != "nftables" {
		t.Fatalf("parsed %+v, want just nftables", pkgs)
	}
}
