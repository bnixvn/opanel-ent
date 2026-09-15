package actions

import (
	"os"
	"path/filepath"
	"testing"
)

// "Limits enforced" is the most consequential line on the CloudLinux page:
// limits recorded but not applied is the one state that looks fine and is
// not. It was answered by asking whether /proc/modules contained "lve ",
// which on a real CloudLinux 10 host is true only because the module is
// called "kmodlve" and that string ends with it -- the right answer for the
// wrong reason. Anchoring to the start of a line, the obvious correction,
// reports no LVE on every CloudLinux 10 host instead.
func TestLVEModuleDetection(t *testing.T) {
	for _, tc := range []struct {
		name    string
		modules string
		want    bool
	}{
		{"CloudLinux 10, measured on a converted host", `netconsole 36864 2 - Live 0xffffffffc0a00000
kmodlve 51265536 0 - Live 0xffffffffc0b15000 (O)
ext4 1187840 0 - Live 0xffffffffc0500000`, true},

		{"CloudLinux 7 and 8", `lve 233472 0 - Live 0xffffffffc0b15000 (O)
ext4 1187840 0 - Live 0xffffffffc0500000`, true},

		{"no LVE at all", `ext4 1187840 0 - Live 0xffffffffc0500000
mbcache 16384 1 ext4, Live 0xffffffffc0400000`, false},

		// The substring test said yes to all of these.
		{"a module that merely ends in lve", `solve 16384 0 - Live 0xffffffffc0400000
valve 16384 0 - Live 0xffffffffc0410000
resolve 16384 0 - Live 0xffffffffc0420000`, false},

		{"empty", "", false},
	} {
		dir := t.TempDir()
		path := filepath.Join(dir, "modules")
		if err := os.WriteFile(path, []byte(tc.modules), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := lveLoadedIn(path); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}
