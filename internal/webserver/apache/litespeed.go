package apache

import (
	"bytes"
	"fmt"
	"os"
	"regexp"
	"strconv"

	"github.com/bnixvn/opanel-ent/internal/webserver"
)

// LiteSpeed Enterprise's own configuration file. The panel touches four
// scalar settings in it and nothing else.
const (
	lswsConfig = "/usr/local/lsws/conf/httpd_config.xml"
	apacheConf = "/etc/httpd/conf/httpd.conf"
	apacheBin  = "/usr/sbin/httpd"
)

// ensureApacheMode points LiteSpeed Enterprise at Apache's configuration.
//
// This is the whole reason the two backends share a renderer. LiteSpeed reads
// Apache's configuration files directly -- the feature its Apache-replacement
// story is built on -- so the panel writes one set of vhosts and either server
// can serve them. Switching is then starting a different daemon, not
// migrating anything, and switching back is just as quick.
//
// Five settings, edited in place rather than by rewriting the file. The rest
// of that file is LiteSpeed's and an operator's, and a panel that regenerated
// it whole would silently discard tuning nobody asked it to touch.
func (b *Backend) ensureApacheMode() error {
	if b.name != webserver.BackendLSWS {
		return nil
	}
	data, err := os.ReadFile(lswsConfig)
	if err != nil {
		return fmt.Errorf("apache: read %s: %w", lswsConfig, err)
	}
	port := b.httpPort
	if port == 0 {
		port = 80
	}
	want := [][2]string{
		{"loadApacheConf", "1"},
		{"apacheConfFile", apacheConf},
		{"apacheBinPath", apacheBin},
		{"apachePort", strconv.Itoa(port)},
		// Without this LiteSpeed runs every site's PHP as its own user,
		// which is nobody. Measured on a fresh install: the vhost's
		// SuexecUserGroup was read and ignored, lsphp came up as uid 65534,
		// and a WordPress site served a 500 because it could not read its
		// own 0600 wp-config.php. Every customer on one uid is precisely
		// what per-site PHP exists to prevent, so this is not tuning -- it
		// is the setting that makes the LiteSpeed backend safe to use.
		{"phpSuExec", "1"},
	}

	out := data
	for _, kv := range want {
		out = setXMLValue(out, kv[0], kv[1])
	}
	if string(out) == string(data) {
		return nil
	}
	return writeFileAtomic(lswsConfig, out, 0o644)
}

// setXMLValue replaces one element's text, or adds the element when the file
// does not carry it yet.
//
// Deliberately line-shaped rather than a parse-and-serialise round trip:
// LiteSpeed's file has a layout and comments of its own, and re-emitting it
// from a parsed tree would reformat every line of a file the panel does not
// own to change four of them.
func setXMLValue(doc []byte, name, value string) []byte {
	re := regexp.MustCompile(`(?s)<` + name + `>.*?</` + name + `>`)
	replacement := "<" + name + ">" + value + "</" + name + ">"
	if re.Match(doc) {
		return re.ReplaceAll(doc, []byte(replacement))
	}
	// Not present: put it just inside the root element, where every other
	// server-level setting lives.
	//
	// Anchored to the root's closing tag rather than to an element expected at
	// the start of a line. LiteSpeed writes this file as very nearly one long
	// line, so the line-anchored pattern this used to look for matched
	// nothing and the add path silently did nothing -- which is how a setting
	// the panel believed it had written turned out to be absent.
	closing := []byte("</httpServerConfig>")
	if i := bytes.LastIndex(doc, closing); i >= 0 {
		out := make([]byte, 0, len(doc)+len(replacement))
		out = append(out, doc[:i]...)
		out = append(out, replacement...)
		out = append(out, doc[i:]...)
		return out
	}
	return doc
}
