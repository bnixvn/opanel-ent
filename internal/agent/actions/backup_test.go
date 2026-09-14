package actions

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/bnixvn/opanel-ent/internal/backuparchive"
)

func TestBackupCreateRequestValidate(t *testing.T) {
	ok := BackupCreateRequest{Owner: "customer1", Name: "customer1-20260914-030000.tar.gz",
		IncludeFiles: true}
	if err := ok.Validate(); err != nil {
		t.Fatalf("a valid request was rejected: %v", err)
	}

	bad := []struct {
		name string
		mut  func(*BackupCreateRequest)
	}{
		{"a system account", func(r *BackupCreateRequest) { r.Owner = "root" }},
		{"no extension", func(r *BackupCreateRequest) { r.Name = "customer1-20260914" }},
		{"a path in the name", func(r *BackupCreateRequest) { r.Name = "../../etc/x.tar.gz" }},
		{"a leading dot", func(r *BackupCreateRequest) { r.Name = ".hidden.tar.gz" }},
		{"nothing included", func(r *BackupCreateRequest) { r.IncludeFiles = false }},
		{"an unacceptable database", func(r *BackupCreateRequest) {
			r.Databases = []string{"foo`; DROP DATABASE bar; --"}
		}},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			r := ok
			tc.mut(&r)
			if err := r.Validate(); err == nil {
				t.Fatalf("%s was accepted", tc.name)
			}
		})
	}
}

func TestBackupInstallRequestRequiresTheStagingDirectory(t *testing.T) {
	base := BackupInstallRequest{Owner: "customer1", Name: "customer1-1.tar.gz"}

	good := base
	good.StagedPath = UploadStageDir + "/backup-upload-1"
	if err := good.Validate(); err != nil {
		t.Fatalf("a properly staged archive was rejected: %v", err)
	}
	for _, p := range []string{"/etc/shadow", UploadStageDir + "/../../etc/shadow", ""} {
		bad := base
		bad.StagedPath = p
		if err := bad.Validate(); err == nil {
			t.Errorf("staged path %q was accepted", p)
		}
	}
}

// TestArchiveHomeRoundTrip walks a directory into a tar and reads it back.
// It covers the parts that do not need a real account: the layout, the
// symlink handling and the manifest.
func TestArchiveHomeRoundTrip(t *testing.T) {
	home := t.TempDir()
	write := func(rel, body string) {
		p := filepath.Join(home, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("public_html/index.php", "<?php echo 1;")
	write("public_html/wp-content/uploads/a.txt", "hello")

	if runtime.GOOS != "windows" {
		// A customer's symlink to /etc must be stored as a link, not
		// followed: following it would pull the host's configuration into
		// the customer's archive.
		if err := os.Symlink("/etc/passwd", filepath.Join(home, "escape")); err != nil {
			t.Fatal(err)
		}
	}

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	man := backuparchive.Manifest{Format: backuparchive.FormatVersion, Owner: "customer1"}
	if err := writeManifest(tw, &man); err != nil {
		t.Fatal(err)
	}
	count, total, err := archiveHome(context.Background(), tw, home)
	if err != nil {
		t.Fatalf("archiveHome: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}

	if count != 2 {
		t.Errorf("counted %d regular files, want 2", count)
	}
	if total != int64(len("<?php echo 1;")+len("hello")) {
		t.Errorf("counted %d bytes, want %d", total, len("<?php echo 1;")+len("hello"))
	}

	// Read it back.
	got := map[string]string{}
	links := map[string]string{}
	gzr, err := gzip.NewReader(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gzr)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		switch h.Typeflag {
		case tar.TypeReg:
			body, err := io.ReadAll(tr)
			if err != nil {
				t.Fatal(err)
			}
			got[h.Name] = string(body)
		case tar.TypeSymlink:
			links[h.Name] = h.Linkname
		}
	}

	if got["files/public_html/index.php"] != "<?php echo 1;" {
		t.Errorf("index.php came back as %q", got["files/public_html/index.php"])
	}
	if got["files/public_html/wp-content/uploads/a.txt"] != "hello" {
		t.Errorf("a nested file did not round-trip")
	}
	if runtime.GOOS != "windows" {
		if links["files/escape"] != "/etc/passwd" {
			t.Errorf("the symlink was not stored as a link: %q", links["files/escape"])
		}
		if strings.Contains(got["files/escape"], "root:") {
			t.Fatal("the symlink was followed; /etc/passwd is inside the archive")
		}
	}

	// And the manifest reads back through the same path the restore uses.
	m, _, err := readManifest(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("readManifest: %v", err)
	}
	if m.Owner != "customer1" {
		t.Fatalf("manifest came back as %+v", m)
	}
}

func TestReadManifestRejectsSomethingElse(t *testing.T) {
	if _, _, err := readManifest(strings.NewReader("this is a photograph")); err == nil {
		t.Fatal("a file that is not an archive was accepted")
	}

	// A gzip that is a valid tar but carries no manifest is not one of ours,
	// and restoring from it would write whatever it happened to contain.
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Name: "random.txt", Size: 2, Mode: 0o644, Typeflag: tar.TypeReg})
	_, _ = tw.Write([]byte("hi"))
	_ = tw.Close()
	_ = gz.Close()

	if _, _, err := readManifest(bytes.NewReader(buf.Bytes())); err == nil {
		t.Fatal("an archive with no manifest was accepted")
	}
}
