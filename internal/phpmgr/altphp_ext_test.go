package phpmgr

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// Both shapes are real, taken from a host's stderr. The first is an extension
// built against symbols this PHP does not export; the second is imagick and
// gmagick, which cannot be loaded together.
func TestFailedExtensions(t *testing.T) {
	out := `PHP Warning:  PHP Startup: Unable to load dynamic library 'eio.so' (tried: ` +
		`/opt/alt/php84/usr/lib64/php/modules/eio.so (undefined symbol: socket_ce)) in Unknown on line 0
PHP Warning:  PHP Startup: Unable to load dynamic library 'http.so' (tried: ...) in Unknown on line 0
PHP Warning:  Cannot load module "imagick" because conflicting module "gmagick" is already loaded in Unknown on line 0
[PHP Modules]
curl
json`

	got := failedExtensions(out)
	want := []string{"eio", "http", "imagick"}
	if !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	if len(failedExtensions("[PHP Modules]\ncurl\njson\n")) != 0 {
		t.Error("a clean run reported failures")
	}
}

// Mirroring, not merging. A fresh php.d names the same extensions the
// catalogue does under different filenames -- 00-mysqlnd.ini against
// mysqlnd.ini -- so merging loaded mysqlnd twice and PHP said so on every
// start. The panel's own files have to survive it.
func TestMirrorExtensionDirReplacesAndKeepsPanelFiles(t *testing.T) {
	root := t.TempDir()
	std := filepath.Join(root, "std")
	active := filepath.Join(root, "active")
	for _, d := range []string{std, active} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(dir, name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(std, "mysqlnd.ini", "extension=mysqlnd.so")
	write(std, "gd.ini", "extension=gd.so")
	write(active, "00-mysqlnd.ini", "extension=mysqlnd.so") // the duplicate
	write(active, panelIniFile, "memory_limit=256M")
	write(active, altPHPDefaultsFile, "date.timezone=UTC")

	if err := mirrorExtensionDir(std, active); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(active)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	slices.Sort(names)
	want := []string{altPHPDefaultsFile, panelIniFile, "gd.ini", "mysqlnd.ini"}
	slices.Sort(want)
	if !slices.Equal(names, want) {
		t.Errorf("got %v, want %v", names, want)
	}
}
