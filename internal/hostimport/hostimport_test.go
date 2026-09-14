package hostimport

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"strings"
	"testing"
)

// entry is one member of a test archive.
type entry struct {
	name string
	body string
	dir  bool
}

func build(t *testing.T, entries []entry) *bytes.Reader {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Mode: 0o644, Size: int64(len(e.body))}
		if e.dir {
			hdr.Typeflag, hdr.Mode, hdr.Size = tar.TypeDir, 0o755, 0
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write header %s: %v", e.name, err)
		}
		if !e.dir {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatalf("write %s: %v", e.name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return bytes.NewReader(buf.Bytes())
}

// The shape a cPanel cpmove archive actually has.
func cpanelArchive() []entry {
	return []entry{
		{name: "cpmove-alice/", dir: true},
		{name: "cpmove-alice/meta/", dir: true},
		{name: "cpmove-alice/meta/homedir_paths", body: "/home/alice\n"},
		{name: "cpmove-alice/cp/", dir: true},
		{name: "cpmove-alice/cp/alice", body: "DNS=example.com\n"},
		{name: "cpmove-alice/userdata/", dir: true},
		{name: "cpmove-alice/userdata/main", body: "main_domain: example.com\n"},
		{name: "cpmove-alice/userdata/example.com", body: strings.Join([]string{
			"documentroot: /home/alice/public_html",
			"servername: example.com",
			"phpversion: ea-php81",
			"main_domain: 1",
		}, "\n")},
		{name: "cpmove-alice/userdata/shop.example.net", body: strings.Join([]string{
			"documentroot: /home/alice/public_html/shop",
			"servername: shop.example.net",
			"phpversion: ea-php74",
		}, "\n")},
		{name: "cpmove-alice/userdata/example.com.cache", body: "ignore me"},
		{name: "cpmove-alice/homedir/", dir: true},
		{name: "cpmove-alice/homedir/public_html/", dir: true},
		{name: "cpmove-alice/homedir/public_html/index.php", body: "<?php echo 1;"},
		{name: "cpmove-alice/homedir/public_html/shop/", dir: true},
		{name: "cpmove-alice/homedir/public_html/shop/index.php", body: "<?php echo 2;"},
		{name: "cpmove-alice/mysql/", dir: true},
		{name: "cpmove-alice/mysql/alice_wp.sql", body: "CREATE TABLE a (id int);"},
		{name: "cpmove-alice/mysql/alice_shop.sql", body: "CREATE TABLE b (id int);"},
		{name: "cpmove-alice/mysql.sql", body: "GRANT ALL ON ..."},
	}
}

// The shape a DirectAdmin user backup actually has.
func directadminArchive() []entry {
	return []entry{
		{name: "backup/", dir: true},
		{name: "backup/user.conf", body: strings.Join([]string{
			"username=bob",
			"domain=bobshop.com",
			"php1_select=82",
		}, "\n")},
		{name: "backup/domains.list", body: "bobshop.com\nother.example\n"},
		{name: "backup/crontab", body: "0 3 * * * /home/bob/x.sh\n"},
		{name: "domains/", dir: true},
		{name: "domains/bobshop.com/", dir: true},
		{name: "domains/bobshop.com/public_html/", dir: true},
		{name: "domains/bobshop.com/public_html/index.php", body: "<?php echo 3;"},
		{name: "domains/other.example/", dir: true},
		{name: "domains/other.example/public_html/", dir: true},
		{name: "domains/other.example/public_html/index.html", body: "<h1>hi</h1>"},
		{name: "bob_main.sql", body: "CREATE TABLE c (id int);"},
	}
}

func TestInspectCPanel(t *testing.T) {
	p, err := Inspect(build(t, cpanelArchive()))
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if p.Format != FormatCPanel {
		t.Fatalf("format = %q, want %q", p.Format, FormatCPanel)
	}
	// The account name, not the archive's directory name: "cpmove-" is
	// cPanel's prefix for the file, and the database prefixes were built
	// from "alice".
	if p.SourceUser != "alice" {
		t.Errorf("source user = %q, want alice", p.SourceUser)
	}
	if p.HomeIn != "cpmove-alice/homedir" {
		t.Errorf("home = %q", p.HomeIn)
	}
	if p.HomeFiles != 2 {
		t.Errorf("home files = %d, want 2", p.HomeFiles)
	}

	names := domainNames(p)
	if len(p.Domains) != 2 {
		t.Fatalf("domains = %v, want 2", names)
	}
	// The main domain sorts first, because it is the one somebody looks for.
	if !p.Domains[0].Main || p.Domains[0].Name != "example.com" {
		t.Errorf("first domain = %+v, want the main one", p.Domains[0])
	}
	if p.Domains[0].PHPVersion != "8.1" {
		t.Errorf("php for the main domain = %q, want 8.1", p.Domains[0].PHPVersion)
	}
	if p.Domains[1].PHPVersion != "7.4" {
		t.Errorf("php for the addon = %q, want 7.4", p.Domains[1].PHPVersion)
	}

	// mysql.sql is cPanel's grants file, not a database, and a .cache file
	// is not a domain.
	if got := dbNames(p); len(got) != 2 {
		t.Fatalf("databases = %v, want two", got)
	}
	for _, d := range p.Databases {
		if d.Name == "mysql" {
			t.Error("the grants file was taken for a database")
		}
	}
	for _, n := range names {
		if strings.HasSuffix(n, ".cache") {
			t.Errorf("a cache file was taken for a domain: %s", n)
		}
	}
}

func TestInspectDirectAdmin(t *testing.T) {
	p, err := Inspect(build(t, directadminArchive()))
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if p.Format != FormatDirectAdmin {
		t.Fatalf("format = %q, want %q", p.Format, FormatDirectAdmin)
	}
	if len(p.Domains) != 2 {
		t.Fatalf("domains = %v, want 2", domainNames(p))
	}
	if !p.Domains[0].Main || p.Domains[0].Name != "bobshop.com" {
		t.Errorf("first domain = %+v, want bobshop.com as the main one", p.Domains[0])
	}
	if p.Domains[0].PHPVersion != "8.2" {
		t.Errorf("php = %q, want 8.2", p.Domains[0].PHPVersion)
	}
	if p.Domains[0].DocRootIn == "" {
		t.Error("no document root was found for the main domain")
	}
	if p.CronIn == "" {
		t.Error("the crontab was not found")
	}
	if len(p.Databases) != 1 || p.Databases[0].Name != "bob_main" {
		t.Errorf("databases = %v", dbNames(p))
	}
}

// The list of what is left behind is not decoration. Somebody migrating a
// customer needs to know that their email is not coming with them, before
// they cut the DNS over rather than a week afterwards.
func TestPlanSaysWhatItWillNotImport(t *testing.T) {
	for _, entries := range [][]entry{cpanelArchive(), directadminArchive()} {
		p, err := Inspect(build(t, entries))
		if err != nil {
			t.Fatalf("Inspect: %v", err)
		}
		joined := strings.ToLower(strings.Join(p.NotImported, " "))
		for _, want := range []string{"email", "dns", "ssl"} {
			if !strings.Contains(joined, want) {
				t.Errorf("%s: nothing said about %s: %v", p.Format, want, p.NotImported)
			}
		}
	}
}

func TestInspectRefusesWhatItDoesNotUnderstand(t *testing.T) {
	t.Run("not a gzip", func(t *testing.T) {
		if _, err := Inspect(strings.NewReader("hello")); err == nil {
			t.Fatal("plain text was accepted")
		}
	})
	t.Run("somebody's own tar", func(t *testing.T) {
		_, err := Inspect(build(t, []entry{
			{name: "stuff/", dir: true},
			{name: "stuff/notes.txt", body: "hello"},
		}))
		if err == nil {
			t.Fatal("an unrelated archive was accepted")
		}
		if !strings.Contains(err.Error(), "cPanel or DirectAdmin") {
			t.Fatalf("the error does not say what was expected: %v", err)
		}
	})
	t.Run("both at once", func(t *testing.T) {
		mixed := append(cpanelArchive(), directadminArchive()...)
		if _, err := Inspect(build(t, mixed)); err == nil {
			t.Fatal("an archive with both layouts was accepted")
		}
	})
}

// Older cPanel nests the home directory in a second tar. Saying so is better
// than importing an account with no files in it and looking successful.
func TestNestedHomeTarIsReported(t *testing.T) {
	entries := append(cpanelArchive(), entry{
		name: "cpmove-alice/homedir.tar", body: "not really a tar",
	})
	p, err := Inspect(build(t, entries))
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	joined := strings.Join(p.Warnings, " ")
	if !strings.Contains(joined, "nested tar") {
		t.Fatalf("nothing warned about the nested tar: %v", p.Warnings)
	}
}

func TestNormalisePHP(t *testing.T) {
	cases := map[string]string{
		"ea-php81":  "8.1",
		"ea-php74":  "7.4",
		"php82":     "8.2",
		"82":        "8.2",
		"8":         "",
		"":          "",
		"alt-php83": "8.3",
	}
	for in, want := range cases {
		if got := normalisePHP(in); got != want {
			t.Errorf("normalisePHP(%q) = %q, want %q", in, got, want)
		}
	}
}

func domainNames(p *Plan) []string {
	out := make([]string, 0, len(p.Domains))
	for _, d := range p.Domains {
		out = append(out, d.Name)
	}
	return out
}

func dbNames(p *Plan) []string {
	out := make([]string, 0, len(p.Databases))
	for _, d := range p.Databases {
		out = append(out, d.Name)
	}
	return out
}
