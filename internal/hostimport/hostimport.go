// Package hostimport reads a cPanel or DirectAdmin account backup.
//
// Both panels export a tar.gz of one account: its home directory, its
// databases as SQL dumps, and some configuration describing the domains. The
// layouts differ and neither is documented as a format, so what this package
// does is recognise the shapes that actually occur and report what it found
// -- including what it did not understand, which matters more here than
// anywhere else in the panel. An import that silently drops a domain is
// worse than one that refuses.
//
// Nothing here touches the disk beyond reading the archive it is given, so
// the detection is testable against archives built in a test.
package hostimport

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
)

// Formats this package recognises.
const (
	FormatCPanel      = "cpanel"
	FormatDirectAdmin = "directadmin"
)

// MaxEntries bounds how many members are walked. An archive is somebody
// else's file and an inspection should not become an afternoon.
const MaxEntries = 2000000

// maxConfigBytes caps one configuration file read into memory.
const maxConfigBytes = 256 << 10

// Domain is one website found in an archive.
type Domain struct {
	Name string `json:"name"`
	// DocRootIn is the path inside the archive holding the document root.
	DocRootIn string `json:"docroot_in"`
	// PHPVersion is what the source panel recorded, when it recorded one.
	PHPVersion string `json:"php_version,omitempty"`
	// Main is set for the account's primary domain.
	Main bool `json:"main"`
	// Kind is "main" or "addon", as the source called it.
	Kind string `json:"kind,omitempty"`
}

// Database is one SQL dump found in an archive.
type Database struct {
	// Name as the source panel had it, prefix and all.
	Name string `json:"name"`
	// DumpIn is the path inside the archive.
	DumpIn string `json:"dump_in"`
	Bytes  int64  `json:"bytes"`
}

// Plan is what an archive contains and what importing it would do.
type Plan struct {
	Format string `json:"format"`
	// SourceUser is the account name the other panel used. The panel does not
	// have to reuse it, but it is what the database prefixes were built from.
	SourceUser string     `json:"source_user"`
	Domains    []Domain   `json:"domains"`
	Databases  []Database `json:"databases"`
	// HomeIn is the directory inside the archive holding the files.
	HomeIn    string `json:"home_in"`
	HomeBytes int64  `json:"home_bytes"`
	HomeFiles int    `json:"home_files"`
	// CronIn is the crontab, when the archive carries one.
	CronIn string `json:"cron_in,omitempty"`
	// NotImported names what this panel has no place for, so nobody
	// discovers it missing a week after the DNS was cut over.
	NotImported []string `json:"not_imported,omitempty"`
	// Warnings are things that may need a person.
	Warnings []string `json:"warnings,omitempty"`
}

// member is one archive entry, kept only as far as it is needed.
type member struct {
	name string
	dir  bool
	size int64
	// body is filled only for the small configuration files.
	body []byte
}

// Inspect reads an archive and works out what is in it.
//
// One pass, collecting names and the handful of small configuration files,
// and only then deciding what the layout is. Deciding while reading does not
// work: a DirectAdmin archive has "backup" and "domains" side by side at the
// top with no directory around them, while a cPanel one wraps everything in a
// directory named for the account, and the first entry does not say which.
func Inspect(r io.Reader) (*Plan, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, fmt.Errorf("that file is not a gzip archive: %w", err)
	}
	defer func() { _ = gz.Close() }()

	var members []member
	tr := tar.NewReader(gz)
	for n := 0; ; n++ {
		if n > MaxEntries {
			return nil, fmt.Errorf("that archive holds more than %d files", MaxEntries)
		}
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read the archive: %w", err)
		}
		name := path.Clean(strings.TrimPrefix(hdr.Name, "./"))
		if name == "." || name == "/" || name == "" {
			continue
		}
		m := member{name: name, dir: hdr.Typeflag == tar.TypeDir, size: hdr.Size}
		if hdr.Typeflag == tar.TypeReg && wantsBody(name) {
			if m.body, err = io.ReadAll(io.LimitReader(tr, maxConfigBytes)); err != nil {
				return nil, fmt.Errorf("read %s: %w", name, err)
			}
		}
		members = append(members, m)
	}
	return interpret(members)
}

// wantsBody reports whether a file is small configuration worth reading.
func wantsBody(name string) bool {
	base := path.Base(name)
	if strings.Contains(name, "/userdata/") || strings.HasPrefix(name, "userdata/") {
		return !strings.HasSuffix(base, ".cache")
	}
	switch base {
	case "user.conf", "domains.list", "domain.conf":
		return true
	}
	return false
}

// markers are the directories each panel's layout is recognised by.
var (
	cpanelMarkers = map[string]bool{"homedir": true, "userdata": true, "meta": true, "cp": true}
	daMarkers     = map[string]bool{"backup": true, "domains": true}
)

// interpret decides the layout and reads what it needs.
func interpret(members []member) (*Plan, error) {
	root, format, err := detect(members)
	if err != nil {
		return nil, err
	}

	p := &Plan{Format: format, SourceUser: strings.TrimPrefix(root, "cpmove-")}
	if root == "" {
		// DirectAdmin does not name the account in the path; user.conf does.
		p.SourceUser = ""
	}

	domains := map[string]*Domain{}
	var homePrefix string
	if format == FormatCPanel {
		homePrefix = join(root, "homedir")
	} else {
		homePrefix = join(root, "domains")
	}
	p.HomeIn = homePrefix

	for _, m := range members {
		rel := relTo(root, m.name)
		if rel == "" {
			continue
		}
		if under(m.name, homePrefix) && !m.dir {
			p.HomeFiles++
			p.HomeBytes += m.size
		}

		// Database dumps, wherever they are: cPanel puts them in mysql/,
		// DirectAdmin at the top level or under backup/.
		if !m.dir && strings.HasSuffix(rel, ".sql") {
			base := strings.TrimSuffix(path.Base(rel), ".sql")
			// cPanel's mysql.sql holds grants, not a database.
			if base != "mysql" && base != "grants" && base != "mysql_grants" {
				p.Databases = append(p.Databases, Database{
					Name: base, DumpIn: m.name, Bytes: m.size,
				})
			}
		}
		if !m.dir && path.Base(rel) == "crontab" {
			p.CronIn = m.name
		}

		switch format {
		case FormatCPanel:
			if strings.HasPrefix(rel, "userdata/") && len(m.body) > 0 {
				if d := parseCPanelUserdata(path.Base(rel), m.body, root); d != nil {
					domains[d.Name] = d
				}
			}
			if rel == "homedir.tar" || rel == "homedir.tar.gz" {
				p.Warnings = append(p.Warnings,
					"This archive stores the home directory as a nested tar, which "+
						"this panel does not unpack. Extract it yourself and bring "+
						"the files in with the file manager.")
			}
		default:
			noteDADomain(domains, root, rel)
			if path.Base(rel) == "user.conf" && len(m.body) > 0 {
				if u := valueFrom(m.body, "username"); u != "" {
					p.SourceUser = u
				}
			}
		}
	}

	if format == FormatDirectAdmin {
		applyDAUserConf(members, root, domains)
	}
	for _, d := range domains {
		p.Domains = append(p.Domains, *d)
	}
	sort.Slice(p.Domains, func(i, j int) bool {
		if p.Domains[i].Main != p.Domains[j].Main {
			return p.Domains[i].Main
		}
		return p.Domains[i].Name < p.Domains[j].Name
	})
	sort.Slice(p.Databases, func(i, j int) bool { return p.Databases[i].Name < p.Databases[j].Name })

	if len(p.Domains) == 0 {
		p.Warnings = append(p.Warnings,
			"No domains were found, so only the files and databases can be imported.")
	}
	// Said plainly, every time. These are the parts of a hosting account this
	// panel has no place for, and finding that out after the DNS has been cut
	// over is how a migration goes wrong.
	p.NotImported = []string{
		"Email accounts, mailboxes and forwarders",
		"DNS zones — point the domains at this server yourself",
		"SSL certificates — the panel issues new ones",
		"FTP accounts other than the main one",
		"Anything specific to the other panel, such as its own scripts or settings",
	}
	return p, nil
}

// detect works out the root directory and which panel wrote the archive.
func detect(members []member) (root, format string, err error) {
	cpanel, da := false, false
	roots := map[string]bool{}

	for _, m := range members {
		parts := strings.Split(m.name, "/")
		switch {
		case cpanelMarkers[parts[0]] && !daMarkers[parts[0]]:
			cpanel, roots[""] = true, true
		case daMarkers[parts[0]]:
			da, roots[""] = true, true
		case len(parts) > 1 && cpanelMarkers[parts[1]]:
			cpanel, roots[parts[0]] = true, true
		case len(parts) > 1 && daMarkers[parts[1]]:
			da, roots[parts[0]] = true, true
		}
	}

	switch {
	case cpanel && da:
		// "domains" belongs to DirectAdmin but cPanel has no such directory,
		// so both sets of markers is not a format; it is somebody's own tar.
		return "", "", fmt.Errorf("that archive looks like both a cPanel and a " +
			"DirectAdmin backup, which means it is neither")
	case cpanel:
		format = FormatCPanel
	case da:
		format = FormatDirectAdmin
	default:
		return "", "", fmt.Errorf("that does not look like a cPanel or " +
			"DirectAdmin account backup")
	}

	// One wrapper directory is the normal case; more than one means the
	// archive holds several accounts, which is not what this imports.
	if len(roots) > 1 {
		return "", "", fmt.Errorf("that archive holds more than one account; " +
			"import them one at a time")
	}
	for r := range roots {
		root = r
	}
	return root, format, nil
}

// noteDADomain spots a domain from the DirectAdmin directory layout.
func noteDADomain(domains map[string]*Domain, root, rel string) {
	parts := strings.Split(rel, "/")
	if len(parts) < 2 || parts[0] != "domains" || !looksLikeDomain(parts[1]) {
		return
	}
	name := parts[1]
	if _, seen := domains[name]; !seen {
		domains[name] = &Domain{Name: name, Kind: "addon"}
	}
	if len(parts) >= 3 && parts[2] == "public_html" {
		domains[name].DocRootIn = join(root, "domains", name, "public_html")
	}
}

// applyDAUserConf marks the main domain and fills in the PHP version.
func applyDAUserConf(members []member, root string, domains map[string]*Domain) {
	for _, m := range members {
		if path.Base(relTo(root, m.name)) != "user.conf" || len(m.body) == 0 {
			continue
		}
		if main := valueFrom(m.body, "domain"); main != "" {
			if d, ok := domains[main]; ok {
				d.Main, d.Kind = true, "main"
			}
		}
		if php := normalisePHP(valueFrom(m.body, "php1_select")); php != "" {
			for _, d := range domains {
				if d.PHPVersion == "" {
					d.PHPVersion = php
				}
			}
		}
		return
	}
}

// parseCPanelUserdata reads one of cPanel's per-domain files.
func parseCPanelUserdata(file string, body []byte, root string) *Domain {
	// "main" is the account summary, not a domain, and the caches are not
	// configuration at all.
	if file == "main" || strings.HasSuffix(file, ".cache") {
		return nil
	}
	name := valueFrom(body, "servername")
	if name == "" {
		name = file
	}
	if !looksLikeDomain(name) {
		return nil
	}
	d := &Domain{Name: name, Kind: "addon"}
	if root := valueFromBool(body, "main_domain"); root {
		d.Main, d.Kind = true, "main"
	}
	if docroot := valueFrom(body, "documentroot"); docroot != "" {
		// The recorded path is absolute on the other server; only the part
		// below the home directory means anything here.
		if i := strings.Index(docroot, "/public_html"); i >= 0 {
			d.DocRootIn = join(root, "homedir", strings.TrimPrefix(docroot[i:], "/"))
		}
	}
	d.PHPVersion = normalisePHP(valueFrom(body, "phpversion"))
	return d
}

// valueFrom reads "key: value" or "key=value" out of the simple formats both
// panels use.
func valueFrom(body []byte, key string) string {
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		for _, sep := range []string{": ", "=", ":"} {
			if rest, ok := strings.CutPrefix(line, key+sep); ok {
				return strings.Trim(strings.TrimSpace(rest), `"'`)
			}
		}
	}
	return ""
}

func valueFromBool(body []byte, key string) bool {
	v := valueFrom(body, key)
	return v == "1" || strings.EqualFold(v, "yes") || strings.EqualFold(v, "true")
}

// normalisePHP turns "ea-php81", "alt-php83" or "82" into "8.1", "8.3", "8.2".
func normalisePHP(s string) string {
	var digits strings.Builder
	for _, c := range s {
		if c >= '0' && c <= '9' {
			digits.WriteRune(c)
		}
	}
	d := digits.String()
	if len(d) < 2 {
		return ""
	}
	return d[:1] + "." + d[1:2]
}

// looksLikeDomain is deliberately loose: the panel validates properly before
// creating anything, and this only has to tell a domain from "public_html".
func looksLikeDomain(s string) bool {
	if s == "" || len(s) > 253 || !strings.Contains(s, ".") {
		return false
	}
	if strings.HasPrefix(s, ".") || strings.HasSuffix(s, ".") {
		return false
	}
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '.', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

// join builds a path inside the archive, skipping an empty root.
func join(root string, parts ...string) string {
	all := parts
	if root != "" {
		all = append([]string{root}, parts...)
	}
	return path.Join(all...)
}

// relTo strips the archive's root directory from a name.
func relTo(root, name string) string {
	if root == "" {
		return name
	}
	if name == root {
		return ""
	}
	return strings.TrimPrefix(strings.TrimPrefix(name, root), "/")
}

// under reports whether a name is inside a directory.
func under(name, dir string) bool {
	return name == dir || strings.HasPrefix(name, dir+"/")
}
