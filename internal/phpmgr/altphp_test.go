package phpmgr

import (
	"strings"
	"testing"
)

// Every path here was read off a CloudLinux 10 host rather than inferred from
// Remi's layout, which is similar enough to be misleading: alt-php has no
// "root" level under the prefix and keeps its pool directory and its ini
// inside the tree rather than under /etc/opt.
func TestAltPHPPaths(t *testing.T) {
	p := NewAltPHP()
	for _, tc := range []struct{ got, want string }{
		{p.CLIBinary("8.4"), "/opt/alt/php84/usr/bin/php"},
		{p.LSPHPBinary("8.4"), "/opt/alt/php84/usr/bin/lsphp"},
		{p.FPMBinary("8.4"), "/opt/alt/php84/usr/sbin/php-fpm"},
		{p.PoolDir("8.4"), "/opt/alt/php84/etc/php-fpm.d"},
		{p.PoolFile("8.4", "example.com"), "/opt/alt/php84/etc/php-fpm.d/opanel-example.com.conf"},
		{p.IniDropIn("8.4"), "/opt/alt/php84/etc/php.d/99-opanel.ini"},
		{p.IonCubeLoader("8.4"), "/opt/alt/php84/usr/lib64/php/modules/ioncube_loader.so"},
		{p.ServiceUnit("8.4"), "alt-php84-fpm"},
		{p.SocketPath("8.4", "example.com"), "/run/opanel-fpm/example.com-alt84.sock"},
		{p.CLIBinary("5.3"), "/opt/alt/php53/usr/bin/php"},
	} {
		if tc.got != tc.want {
			t.Errorf("got %q, want %q", tc.got, tc.want)
		}
	}
}

// The ini drop-in must not go through link/conf. That is a symlink the PHP
// Selector owns and rebuilds on every --setup-cl-selector; a file the panel
// wrote through it is a file the panel can lose without touching anything.
func TestAltPHPIniAvoidsTheSelectorSymlink(t *testing.T) {
	if got := NewAltPHP().IniDropIn("8.4"); strings.Contains(got, "/link/") {
		t.Errorf("ini drop-in goes through the selector's symlink farm: %s", got)
	}
}

// A socket name shared with Remi's would mean that during a migration the old
// pool and the new one want the same path, and whichever started last wins
// silently. The "alt" marker is what keeps the two providers' sockets apart
// while both exist.
func TestAltPHPSocketsDoNotCollideWithRemi(t *testing.T) {
	alt, remi := NewAltPHP().SocketPath("8.4", "x.test"), NewRemi().SocketPath("8.4", "x.test")
	if alt == remi {
		t.Fatalf("both providers claim %s", alt)
	}
}

// The provider a host runs is a recorded decision, not a guess, because
// changing it moves every pool and rewrites every vhost. An unset, empty or
// corrupt state file has to answer with the one a fresh install runs.
func TestActiveProviderDefaults(t *testing.T) {
	if DefaultProvider != ProviderRemi {
		t.Errorf("a fresh install defaults to %q; it is installed before CloudLinux exists", DefaultProvider)
	}
	for _, name := range []string{"", "nonsense", "REMI"} {
		if _, err := New(name); err == nil {
			t.Errorf("New(%q) built a provider", name)
		}
	}
	for _, name := range Providers {
		if _, err := New(name); err != nil {
			t.Errorf("New(%q) failed: %v", name, err)
		}
	}
}

// Every version alt-php publishes has to pass the panel's own version check,
// or a site could be created on one the renderer then refuses.
func TestAltPHPVersionsAreAllValid(t *testing.T) {
	for _, v := range NewAltPHP().Supported() {
		if !ValidVersion(v) {
			t.Errorf("alt-php offers %q, which ValidVersion rejects", v)
		}
	}
}
