package actions

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestFilePathRequestRejectsEscapes(t *testing.T) {
	cases := []struct {
		name string
		path string
		want bool // true when the request should be accepted
	}{
		{"empty means the home itself", "", true},
		{"a plain name", "public_html", true},
		{"a nested path", "public_html/wp-content/index.php", true},
		{"a dot segment is harmless", "./public_html", true},
		{"absolute", "/etc/shadow", false},
		{"parent at the start", "../../etc/shadow", false},
		{"parent in the middle", "public_html/../../etc/shadow", false},
		{"parent alone", "..", false},
		{"a null byte", "public\x00html", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := (&FilePathRequest{Owner: "customer1", Path: tc.path}).Validate()
			if tc.want && err != nil {
				t.Fatalf("path %q was rejected: %v", tc.path, err)
			}
			if !tc.want && err == nil {
				t.Fatalf("path %q was accepted", tc.path)
			}
		})
	}
}

func TestFilePathRequestRejectsBadOwner(t *testing.T) {
	// A system account is not something a request may name, whatever the
	// path: the whole containment story starts from whose home is opened.
	for _, owner := range []string{"root", "mysql", "", "../root", "Bob"} {
		if err := (&FilePathRequest{Owner: owner}).Validate(); err == nil {
			t.Errorf("owner %q was accepted", owner)
		}
	}
}

func TestFileChmodRequestRejectsSetuid(t *testing.T) {
	ok := []string{"0644", "0755", "600", "0400"}
	for _, mode := range ok {
		if err := (&FileChmodRequest{Owner: "customer1", Path: "x", Mode: mode}).Validate(); err != nil {
			t.Errorf("mode %q was rejected: %v", mode, err)
		}
	}
	// 4755 is setuid, 2755 setgid, 1777 sticky. None of them belong on a
	// customer's file, and setuid on one they own is a local root
	// escalation waiting for an administrator to run it.
	bad := []string{"4755", "2755", "1777", "0999", "not-a-mode", ""}
	for _, mode := range bad {
		if err := (&FileChmodRequest{Owner: "customer1", Path: "x", Mode: mode}).Validate(); err == nil {
			t.Errorf("mode %q was accepted", mode)
		}
	}
}

func TestFileInstallRequestRequiresTheStagingDirectory(t *testing.T) {
	base := FileInstallRequest{Owner: "customer1", Path: "up.zip"}

	good := base
	good.StagedPath = UploadStageDir + "/upload-123"
	if err := good.Validate(); err != nil {
		t.Fatalf("a properly staged upload was rejected: %v", err)
	}

	// A bug in the API must not turn into "the agent will copy any file on
	// the host into a customer's web root".
	for _, p := range []string{
		"/etc/shadow",
		"/var/lib/opanel/opanel.db",
		UploadStageDir + "/../../etc/shadow",
		UploadStageDir, // the directory itself, not a file in it
		"",
	} {
		bad := base
		bad.StagedPath = p
		if err := bad.Validate(); err == nil {
			t.Errorf("staged path %q was accepted", p)
		}
	}
}

func TestFileWriteRequestCapsSize(t *testing.T) {
	req := FileWriteRequest{Owner: "customer1", Path: "wp-config.php",
		Content: strings.Repeat("a", MaxEditBytes+1)}
	if err := req.Validate(); err == nil {
		t.Fatal("an oversized write was accepted; the editor would truncate the file")
	}
	req.Content = strings.Repeat("a", MaxEditBytes)
	if err := req.Validate(); err != nil {
		t.Fatalf("a write at the limit was rejected: %v", err)
	}
	req.Path = ""
	if err := req.Validate(); err == nil {
		t.Fatal("a write with no file name was accepted")
	}
}

// TestOsRootRefusesSymlinkEscape pins down the guarantee the whole package
// rests on: a customer who symlinks their way out of their home does not get
// out. It exercises the standard library rather than our code deliberately —
// if a Go upgrade ever weakened this, every handler above would silently
// become an arbitrary-file-read.
func TestOsRootRefusesSymlinkEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs privileges on Windows")
	}
	home := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte("not yours"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(home, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("..", filepath.Join(home, "up")); err != nil {
		t.Fatal(err)
	}

	root, err := os.OpenRoot(home)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()

	for _, p := range []string{"escape", "up/secret", "../secret"} {
		if _, err := root.Open(p); err == nil {
			t.Errorf("os.Root opened %q, which is outside the home", p)
		}
	}

	// And the ordinary case still works, so the test would notice a change
	// that simply broke everything.
	if err := os.WriteFile(filepath.Join(home, "mine.txt"), []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := root.Open("mine.txt"); err != nil {
		t.Fatalf("os.Root refused a file inside the home: %v", err)
	}
}
