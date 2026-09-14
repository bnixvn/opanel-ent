package actions

import (
	"io/fs"
	"strconv"
	"strings"
	"testing"
)

// Zip slip and its variants. These are the names an archive uses to write
// somewhere it was not unpacked into, and every one of them has been a real
// vulnerability in real software.
func TestSafeEntryNameRefusesEscapes(t *testing.T) {
	for _, name := range []string{
		"../etc/passwd",
		"../../../../etc/cron.d/backdoor",
		"/etc/passwd",
		"/../etc/passwd",
		`..\..\windows\system32\evil.dll`,
		"C:/Windows/evil.dll",
		`C:\Windows\evil.dll`,
		"public_html/../../.ssh/authorized_keys",
		"a/b/../../../outside",
	} {
		t.Run(name, func(t *testing.T) {
			got, err := safeEntryName(name)
			if err == nil {
				t.Fatalf("safeEntryName(%q) = %q, want an error", name, got)
			}
		})
	}
}

func TestSafeEntryNameKeepsOrdinaryNames(t *testing.T) {
	cases := map[string]string{
		"index.php":                 "index.php",
		"./index.php":               "index.php",
		"wp-content/themes/x/a.css": "wp-content/themes/x/a.css",
		"a/b/../c.txt":              "a/c.txt",
		`wp-content\uploads\a.jpg`:  "wp-content/uploads/a.jpg",
		"dir/":                      "dir",
		".":                         "",
		"./":                        "",
	}
	for in, want := range cases {
		t.Run(in, func(t *testing.T) {
			got, err := safeEntryName(in)
			if err != nil {
				t.Fatalf("safeEntryName(%q): %v", in, err)
			}
			if got != want {
				t.Fatalf("safeEntryName(%q) = %q, want %q", in, got, want)
			}
		})
	}
}

func TestArchiveFormat(t *testing.T) {
	ok := map[string]string{
		"backup.zip":          "zip",
		"BACKUP.ZIP":          "zip",
		"site.tar.gz":         "tar.gz",
		"site.tgz":            "tar.gz",
		"site.tar":            "tar",
		"a/b/c/archive.tar.gz": "tar.gz",
	}
	for name, want := range ok {
		got, err := archiveFormat(name)
		if err != nil {
			t.Errorf("archiveFormat(%q): %v", name, err)
			continue
		}
		if got != want {
			t.Errorf("archiveFormat(%q) = %q, want %q", name, got, want)
		}
	}
	// Anything else is refused before a file is opened. .rar and .7z in
	// particular would otherwise look supported and fail confusingly.
	for _, name := range []string{"backup.rar", "backup.7z", "backup", "backup.gz", "backup.zip.exe"} {
		if got, err := archiveFormat(name); err == nil {
			t.Errorf("archiveFormat(%q) = %q, want an error", name, got)
		}
	}
}

// Copying a directory into its own subtree walks forever, writing until the
// quota stops it and leaving nested copies behind.
func TestCopyRefusesRecursion(t *testing.T) {
	cases := []struct {
		name, from, to string
		wantErr        bool
	}{
		{"into itself", "public_html", "public_html", true},
		{"into a child", "public_html", "public_html/copy", true},
		{"into a sibling", "public_html", "backup_html", false},
		{"a prefix that is not a path boundary", "site", "site2", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := &FileCopyRequest{Owner: "alice", From: tc.from, To: tc.to}
			err := req.Validate()
			if tc.wantErr && err == nil {
				t.Fatalf("copy %s -> %s was accepted", tc.from, tc.to)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("copy %s -> %s was refused: %v", tc.from, tc.to, err)
			}
		})
	}
}

func TestArchiveRequestValidation(t *testing.T) {
	t.Run("nothing selected", func(t *testing.T) {
		req := &FileArchiveRequest{Owner: "alice", Dest: "out.zip"}
		if err := req.Validate() ; err == nil {
			t.Fatal("an empty selection was accepted")
		}
	})
	t.Run("the home itself", func(t *testing.T) {
		req := &FileArchiveRequest{Owner: "alice", Paths: []string{""}, Dest: "out.zip"}
		if err := req.Validate(); err == nil {
			t.Fatal("archiving the home directory itself was accepted")
		}
	})
	t.Run("an escape in a selected path", func(t *testing.T) {
		req := &FileArchiveRequest{Owner: "alice", Paths: []string{"../../etc"}, Dest: "out.zip"}
		if err := req.Validate(); err == nil {
			t.Fatal("a path outside the home was accepted")
		}
	})
	t.Run("an unusable destination", func(t *testing.T) {
		req := &FileArchiveRequest{Owner: "alice", Paths: []string{"public_html"}, Dest: "out.rar"}
		err := req.Validate()
		if err == nil {
			t.Fatal(".rar was accepted")
		}
		if !strings.Contains(err.Error(), "out.rar") {
			t.Fatalf("the error does not name the file: %v", err)
		}
	})
	t.Run("a good request", func(t *testing.T) {
		req := &FileArchiveRequest{
			Owner: "alice",
			Paths: []string{"public_html", "notes.txt"},
			Dest:  "backups/site.tar.gz",
		}
		if err := req.Validate(); err != nil {
			t.Fatalf("a reasonable request was refused: %v", err)
		}
	})
}

func TestExtractRequestValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		req  FileExtractRequest
	}{
		{"no archive", FileExtractRequest{Owner: "alice"}},
		{"escaping archive path", FileExtractRequest{Owner: "alice", Path: "../../etc/x.zip"}},
		{"escaping destination", FileExtractRequest{Owner: "alice", Path: "a.zip", Dest: "../.."}},
		{"unsupported format", FileExtractRequest{Owner: "alice", Path: "a.rar"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.req.Validate(); err == nil {
				t.Fatal("accepted")
			}
		})
	}
	good := FileExtractRequest{Owner: "alice", Path: "public_html/theme.zip", Dest: "public_html/wp-content/themes"}
	if err := good.Validate(); err != nil {
		t.Fatalf("a reasonable request was refused: %v", err)
	}
}

// A recursive chmod that took the mode literally would strip the execute bit
// from every directory, leaving a tree the customer cannot even list -- and
// cannot fix, because fixing it means listing it.
func TestRecursiveChmodKeepsDirectoriesEnterable(t *testing.T) {
	cases := map[string]struct{ file, dir uint32 }{
		"0644": {0o644, 0o755},
		"0600": {0o600, 0o700},
		"0640": {0o640, 0o750},
		"0444": {0o444, 0o555},
		"0755": {0o755, 0o755},
		"0000": {0o000, 0o000},
	}
	for mode, want := range cases {
		t.Run(mode, func(t *testing.T) {
			got := directoryModeFor(parseOctal(t, mode))
			if uint32(got) != want.dir {
				t.Fatalf("a tree set to %s gives directories %#o, want %#o", mode, got, want.dir)
			}
		})
	}
}

func parseOctal(t *testing.T, s string) fs.FileMode {
	t.Helper()
	n, err := strconv.ParseUint(s, 8, 32)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return fs.FileMode(n)
}
