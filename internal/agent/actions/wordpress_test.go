package actions

import (
	"os"
	"path/filepath"
	"testing"
)

func validWPRequest() WPInstallRequest {
	return WPInstallRequest{
		Owner:         "customer1",
		DocumentRoot:  "/home/customer1/example.com/public_html",
		PHPVersion:    "8.4",
		SiteURL:       "https://example.com",
		DBName:        "customer1_example",
		DBUser:        "customer1_example",
		DBPassword:    "a-database-password",
		Title:         "Example",
		AdminUser:     "customer1",
		AdminPassword: "an-admin-password",
		AdminEmail:    "owner@example.com",
	}
}

func TestWPInstallRequestValidate(t *testing.T) {
	ok := validWPRequest()
	if err := ok.Validate(); err != nil {
		t.Fatalf("a valid request was rejected: %v", err)
	}

	bad := []struct {
		name string
		mut  func(*WPInstallRequest)
	}{
		{"a system account", func(r *WPInstallRequest) { r.Owner = "root" }},
		// The document root is built by the panel, so one that points
		// somewhere else is a bug or an attack. Installing WordPress into
		// /var/www or another customer's home must not be reachable by
		// sending a different string.
		{"a root outside the home", func(r *WPInstallRequest) { r.DocumentRoot = "/var/www/html" }},
		{"another customer's home", func(r *WPInstallRequest) {
			r.DocumentRoot = "/home/customer2/example.com/public_html"
		}},
		{"a traversal", func(r *WPInstallRequest) {
			r.DocumentRoot = "/home/customer1/../customer2/public_html"
		}},
		{"the home itself", func(r *WPInstallRequest) { r.DocumentRoot = "/home/customer1" }},
		{"an unknown PHP version", func(r *WPInstallRequest) { r.PHPVersion = "8.4; rm -rf /" }},
		{"a scheme-less url", func(r *WPInstallRequest) { r.SiteURL = "example.com" }},
		{"a database name with a quote", func(r *WPInstallRequest) { r.DBName = "x`; DROP" }},
		{"no database password", func(r *WPInstallRequest) { r.DBPassword = "" }},
		{"a title with a newline", func(r *WPInstallRequest) { r.Title = "Example\nInjected" }},
		{"an admin name with a space", func(r *WPInstallRequest) { r.AdminUser = "the admin" }},
		{"something that is not an email", func(r *WPInstallRequest) { r.AdminEmail = "nope" }},
		{"a locale that is a path", func(r *WPInstallRequest) { r.Locale = "../../etc" }},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			r := validWPRequest()
			tc.mut(&r)
			if err := r.Validate(); err == nil {
				t.Fatalf("%s was accepted", tc.name)
			}
		})
	}
}

func TestWPStatus(t *testing.T) {
	dir := t.TempDir()

	// A site the panel just created holds only the placeholder page, and
	// that must still count as empty or the one-click install would never
	// be offered.
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("placeholder"), 0o644); err != nil {
		t.Fatal(err)
	}
	st := wpStatus(dir)
	if !st.Empty || st.Installed {
		t.Fatalf("a fresh site reported %+v", st)
	}

	// Something the customer uploaded is not empty.
	if err := os.WriteFile(filepath.Join(dir, "app.php"), []byte("<?php"), 0o644); err != nil {
		t.Fatal(err)
	}
	if st := wpStatus(dir); st.Empty {
		t.Fatal("a directory with a customer's file reported empty")
	}

	// WordPress present.
	inc := filepath.Join(dir, "wp-includes")
	if err := os.MkdirAll(inc, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inc, "version.php"),
		[]byte("<?php\n$wp_version = '7.1.2';\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	st = wpStatus(dir)
	if !st.Installed {
		t.Fatal("an installed WordPress was not detected")
	}
	if st.Version != "7.1.2" {
		t.Fatalf("version = %q, want 7.1.2", st.Version)
	}
}
