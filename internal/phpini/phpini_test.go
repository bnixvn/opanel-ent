package phpini

import (
	"strings"
	"testing"
)

// The whole point of the whitelist. If a value can carry a line break, or a
// name nobody vetted can be stored, these settings become a way to write
// arbitrary webserver configuration.
func TestNormalizeRejectsInjection(t *testing.T) {
	cases := []struct {
		name, directive, value string
	}{
		{"newline in a size", "memory_limit", "256M\nphp_admin_value disable_functions none"},
		{"carriage return", "memory_limit", "256M\rextprocessor evil {"},
		{"newline in a timezone", "date.timezone", "UTC\nphp_admin_flag allow_url_fopen On"},
		{"a directive nobody vetted", "auto_prepend_file", "/tmp/shell.php"},
		{"open_basedir is not editable", "open_basedir", "/"},
		{"extension loading", "extension", "/tmp/evil.so"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got, err := Normalize(tc.directive, tc.value); err == nil {
				t.Fatalf("Normalize(%q, %q) = %q, want an error",
					tc.directive, tc.value, got)
			}
		})
	}
}

func TestNormalizeCanonicalises(t *testing.T) {
	cases := []struct {
		directive, in, want string
	}{
		{"memory_limit", "256M", "256M"},
		{"memory_limit", " 512m ", "512M"},
		{"memory_limit", "268435456", "256M"},
		{"memory_limit", "1G", "1G"},
		{"memory_limit", "-1", "-1"},
		{"max_execution_time", "300", "300"},
		{"max_execution_time", "-1", "-1"},
		{"display_errors", "on", "On"},
		{"display_errors", "1", "On"},
		{"display_errors", "false", "Off"},
		{"date.timezone", "asia/ho_chi_minh", "Asia/Ho_Chi_Minh"},
		{"disable_functions", "system, exec ,system", "exec,system"},
		{"memory_limit", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.directive+"="+tc.in, func(t *testing.T) {
			got, err := Normalize(tc.directive, tc.in)
			if err != nil {
				t.Fatalf("Normalize(%q, %q): %v", tc.directive, tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("Normalize(%q, %q) = %q, want %q", tc.directive, tc.in, got, tc.want)
			}
		})
	}
}

func TestNormalizeEnforcesBounds(t *testing.T) {
	cases := []struct {
		name, directive, value string
	}{
		{"memory above the ceiling", "memory_limit", "8G"},
		{"memory below the floor", "memory_limit", "1M"},
		{"execution time above the ceiling", "max_execution_time", "99999"},
		{"a negative that is not -1", "max_execution_time", "-5"},
		{"not a number", "max_input_vars", "lots"},
		{"a size with a nonsense suffix", "post_max_size", "64X"},
		{"a timezone nobody offered", "date.timezone", "Mars/Olympus_Mons"},
		{"a function outside the set", "disable_functions", "mail"},
		{"a flag that is neither", "display_errors", "maybe"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got, err := Normalize(tc.directive, tc.value); err == nil {
				t.Fatalf("Normalize(%q, %q) = %q, want an error",
					tc.directive, tc.value, got)
			}
		})
	}
}

// The combination that silently breaks uploads: a file larger than the
// request it has to arrive in. PHP does not complain, the upload just fails,
// so the panel has to.
func TestCheckCatchesUploadsThatCannotFit(t *testing.T) {
	if _, err := Check(map[string]string{
		"upload_max_filesize": "512M",
		"post_max_size":       "64M",
		"memory_limit":        "1G",
	}); err == nil {
		t.Fatal("an upload larger than the request body was accepted")
	}

	// The same mistake made by raising only the upload size, leaving the
	// other two at their defaults.
	if _, err := Check(map[string]string{"upload_max_filesize": "512M"}); err == nil {
		t.Fatal("an upload above the default POST size was accepted")
	}

	if _, err := Check(map[string]string{
		"upload_max_filesize": "512M",
		"post_max_size":       "512M",
		"memory_limit":        "256M",
	}); err == nil {
		t.Fatal("a request larger than the memory limit was accepted")
	}

	got, err := Check(map[string]string{
		"upload_max_filesize": "512M",
		"post_max_size":       "512M",
		"memory_limit":        "1G",
	})
	if err != nil {
		t.Fatalf("a consistent set was rejected: %v", err)
	}
	if got["memory_limit"] != "1G" {
		t.Fatalf("memory_limit = %q, want 1G", got["memory_limit"])
	}
}

// An unlimited memory limit is allowed, and must not then be compared as if
// it were a small number.
func TestCheckAllowsUnlimited(t *testing.T) {
	got, err := Check(map[string]string{
		"memory_limit":        "-1",
		"post_max_size":       "512M",
		"upload_max_filesize": "512M",
	})
	if err != nil {
		t.Fatalf("unlimited memory was rejected: %v", err)
	}
	if got["memory_limit"] != "-1" {
		t.Fatalf("memory_limit = %q, want -1", got["memory_limit"])
	}
}

func TestCheckDropsClearedValues(t *testing.T) {
	got, err := Check(map[string]string{"memory_limit": "512M", "max_input_vars": ""})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if _, ok := got["max_input_vars"]; ok {
		t.Fatal("a cleared value was stored instead of being dropped")
	}
	if len(got) != 1 {
		t.Fatalf("got %d settings, want 1", len(got))
	}
}

func TestRenderIsStableAndTyped(t *testing.T) {
	lines := Render(map[string]string{
		"memory_limit":   "512M",
		"display_errors": "Off",
		"date.timezone":  "Asia/Ho_Chi_Minh",
		// Not on the whitelist: a row that somehow reached the database must
		// not reach the configuration file.
		"auto_prepend_file": "/tmp/shell.php",
	})
	want := []string{
		"php_admin_value date.timezone Asia/Ho_Chi_Minh",
		"php_admin_flag display_errors Off",
		"php_admin_value memory_limit 512M",
	}
	if len(lines) != len(want) {
		t.Fatalf("Render produced %d lines:\n%s", len(lines), strings.Join(lines, "\n"))
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Fatalf("line %d = %q, want %q", i, lines[i], want[i])
		}
	}
}

func TestFormatBytesRoundTrips(t *testing.T) {
	for _, s := range []string{"1K", "64M", "256M", "1G", "2G", "-1", "512"} {
		n, err := ParseBytes(s)
		if err != nil {
			t.Fatalf("ParseBytes(%q): %v", s, err)
		}
		if got := FormatBytes(n); got != s {
			t.Fatalf("FormatBytes(ParseBytes(%q)) = %q", s, got)
		}
	}
}

// Every catalogue default has to pass the catalogue's own rules, or the form
// offers a placeholder the panel would refuse to save.
func TestCatalogueDefaultsAreValid(t *testing.T) {
	for _, d := range Catalogue() {
		got, err := Normalize(d.Name, d.Default)
		if err != nil {
			t.Errorf("default for %s (%q) is not acceptable: %v", d.Name, d.Default, err)
			continue
		}
		if got != d.Default {
			t.Errorf("default for %s is %q but canonicalises to %q", d.Name, d.Default, got)
		}
	}
}
