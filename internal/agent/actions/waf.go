package actions

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bnixvn/opanel-ent/internal/agent"
	"github.com/bnixvn/opanel-ent/internal/platform/pkgmgr"
	"github.com/bnixvn/opanel-ent/internal/platform/run"
	"github.com/bnixvn/opanel-ent/internal/webserver"
	"github.com/bnixvn/opanel-ent/internal/webserver/backends"
)

// wafLayout is where the engine and the panel's rules live. It follows the
// server that reads them: Apache loads ModSecurity from the distribution's
// module directory, LiteSpeed carries its own inside /usr/local/lsws.
//
// Resolved per call rather than fixed at startup, for the same reason the
// backend is: an operator can switch server without restarting the agent, and
// rules written into the other server's directory are rules nothing reads.
type wafLayout struct {
	Dir       string
	RulesFile string
	AuditLog  string
	Module    string
	// Package is what installs the engine on this server.
	Package string
}

func wafPaths() wafLayout { return wafPathsFor(backends.Active()) }

// wafPathsFor is wafPaths with the backend as an argument, so the pairing can
// be tested without a state file.
func wafPathsFor(backend string) wafLayout {
	l := wafLayout{
		Dir:       "/etc/httpd/opanel/waf",
		RulesFile: "/etc/httpd/opanel/waf/rules.conf",
		AuditLog:  "/var/log/httpd/modsec_audit.log",
		// EPEL's build for httpd 2.4. The distribution's own package loads
		// it; the panel only adds rules.
		Module:  "/usr/lib64/httpd/modules/mod_security2.so",
		Package: "mod_security",
	}
	if backend == webserver.BackendLSWS {
		// One rules directory, Apache's, for both servers -- because
		// LiteSpeed Enterprise reads Apache's configuration, which is the
		// whole reason the two share a renderer. Rules written into a
		// LiteSpeed-specific directory would be rules nothing includes.
		//
		// The engine is built into LiteSpeed Enterprise rather than being a
		// module on disk: it logs "[MODSEC] created offloader" at startup and
		// there is nothing to install. This used to look for
		// /usr/local/lsws/modules/mod_security.so and offer to install
		// "ols-modsecurity" -- OpenLiteSpeed's package, for a server this
		// panel no longer supports, from a repository that no longer carries
		// it. So switching to LiteSpeed reported a host with 658 rules
		// loaded as having no firewall at all, and offered to fix it by
		// installing a package that does not exist.
		l.Module = lswsBinary
		l.Package = ""
		// LiteSpeed's own error log, not the audit log the rules name.
		// /var/log/httpd is 0700 root, and LiteSpeed's workers run as nobody,
		// so SecAuditLog there is a file they cannot open -- it stays empty
		// and the events page stays blank while the firewall is in fact
		// blocking. LiteSpeed writes every block into its error log anyway,
		// in ModSecurity's own one-line form.
		l.AuditLog = "/usr/local/lsws/logs/error.log"
	}
	return l
}

// WAF modes.
//
// A new site starts in DetectOnly, always. The OWASP rules block real
// WordPress traffic often enough that turning them straight to blocking is
// how a host generates support tickets on the day they enable it.
const (
	WAFOff        = "off"
	WAFDetectOnly = "detect"
	WAFBlock      = "block"
)

// WAFStatus reports what is installed and configured.
type WAFStatus struct {
	// ModuleAvailable is whether the webserver has the engine at all.
	ModuleAvailable bool   `json:"module_available"`
	RulesInstalled  bool   `json:"rules_installed"`
	RuleCount       int    `json:"rule_count"`
	RulesVersion    string `json:"rules_version,omitempty"`
}

// WAFConfigureRequest writes the shared rule configuration.
type WAFConfigureRequest struct {
	// Mode is the default for sites that have the WAF on.
	Mode string `json:"mode"`
	// ExcludedRules are OWASP rule ids to switch off server-wide, for the
	// ones that break a common application.
	ExcludedRules []string `json:"excluded_rules,omitempty"`
	// DisabledFiles are whole rule categories to leave out, by filename.
	// Sent every time: the list is the complete set, so a category left out
	// of it is switched back on.
	DisabledFiles []string `json:"disabled_files,omitempty"`
}

// Validate checks the mode and the rule ids.
func (r *WAFConfigureRequest) Validate() error {
	switch r.Mode {
	case WAFOff, WAFDetectOnly, WAFBlock:
	default:
		return fmt.Errorf("mode must be %q, %q or %q", WAFOff, WAFDetectOnly, WAFBlock)
	}
	for _, name := range r.DisabledFiles {
		if err := validDisabledRuleName(name); err != nil {
			return err
		}
	}
	for _, id := range r.ExcludedRules {
		// Rule ids reach a configuration file the webserver parses, so only
		// digits are accepted rather than escaped.
		if id == "" || strings.IndexFunc(id, func(c rune) bool { return c < '0' || c > '9' }) >= 0 {
			return fmt.Errorf("%q is not a rule id", id)
		}
	}
	return nil
}

// WAFEvent is one request the engine acted on.
type WAFEvent struct {
	At       string `json:"at"`
	ClientIP string `json:"client_ip"`
	Host     string `json:"host"`
	URI      string `json:"uri"`
	RuleID   string `json:"rule_id"`
	Message  string `json:"message"`
	Blocked  bool   `json:"blocked"`
}

// WAFEventsResult is the recent history.
type WAFEventsResult struct {
	Events []WAFEvent `json:"events"`
}

func registerWAF(r *agent.Registry) {
	agent.Register(r, "waf.status", 1, func(_ context.Context, _ struct{}) (WAFStatus, error) {
		return wafStatus(), nil
	})

	agent.RegisterSlow(r, "waf.install", 1, 10*time.Minute,
		func(ctx context.Context, _ struct{}) (WAFStatus, error) {
			if err := installCRS(ctx); err != nil {
				return WAFStatus{}, err
			}
			return wafStatus(), nil
		})

	agent.Register(r, "waf.configure", 1, func(_ context.Context, in WAFConfigureRequest) (WAFStatus, error) {
		if err := writeWAFConfig(in); err != nil {
			return WAFStatus{}, err
		}
		return wafStatus(), nil
	})

	agent.Register(r, "waf.events", 1, func(_ context.Context, _ struct{}) (WAFEventsResult, error) {
		return WAFEventsResult{Events: readWAFEvents(200)}, nil
	})
}

func wafStatus() WAFStatus {
	st := WAFStatus{}
	if _, err := os.Stat(wafPaths().Module); err == nil {
		st.ModuleAvailable = true
	}
	if _, err := os.Stat(wafPaths().RulesFile); err != nil {
		return st
	}
	st.RulesInstalled = true
	// Counted across the rule files rather than the one that includes them:
	// rules.conf holds a handful of directives and an Include, so counting
	// there reports zero rules on a fully installed rule set.
	matches, _ := filepath.Glob(filepath.Join(wafPaths().Dir, "rules", "*.conf"))
	for _, path := range matches {
		if body, err := os.ReadFile(path); err == nil {
			st.RuleCount += strings.Count(string(body), "SecRule ")
		}
	}

	// The CRS records its own version in a setup file; showing it means an
	// operator can tell whether their rules are three years old.
	if v, err := os.ReadFile(filepath.Join(wafPaths().Dir, "crs-version")); err == nil {
		st.RulesVersion = strings.TrimSpace(string(v))
	}
	return st
}

// crsURL is the OWASP Core Rule Set release the panel installs.
const crsURL = "https://github.com/coreruleset/coreruleset/releases/download/v4.7.0/coreruleset-4.7.0-minimal.tar.gz"

// installCRS downloads and unpacks the rule set.
func installCRS(ctx context.Context) error {
	paths := wafPaths()
	if _, err := os.Stat(paths.Module); err != nil {
		if paths.Package == "" {
			return fmt.Errorf("this server carries its own ModSecurity and %s is not there, "+
				"so it is not running", paths.Module)
		}
		// Install the engine rather than refusing. On Apache it is a package,
		// and "install the web application firewall" plainly means the engine
		// as well as the rules -- being told to go and run dnf first is a step
		// that exists only because nobody wrote it down.
		if err := pkgmgr.Install(ctx, paths.Package); err != nil {
			return fmt.Errorf("install %s, the ModSecurity engine: %w", paths.Package, err)
		}
		if _, err := os.Stat(paths.Module); err != nil {
			return fmt.Errorf("%s installed but %s is not there", paths.Package, paths.Module)
		}
	}
	if err := os.MkdirAll(paths.Dir, 0o755); err != nil {
		return err
	}

	tmp, err := os.MkdirTemp("", "crs-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(tmp) }()

	archive := filepath.Join(tmp, "crs.tar.gz")
	if _, err := run.Cmd(ctx, []string{
		"curl", "-fsSL", "--max-time", "300", "-o", archive, crsURL,
	}, run.Timeout(6*time.Minute)); err != nil {
		return fmt.Errorf("download the rule set: %w", err)
	}
	if _, err := run.Cmd(ctx, []string{
		"tar", "xzf", archive, "-C", tmp, "--strip-components=1",
	}, run.Timeout(2*time.Minute)); err != nil {
		return fmt.Errorf("unpack the rule set: %w", err)
	}

	// Only the rules directory and the setup example are wanted; the rest of
	// the release is documentation and tests.
	if _, err := run.Cmd(ctx, []string{
		"cp", "-a", filepath.Join(tmp, "rules"), wafPaths().Dir + "/",
	}, run.Timeout(time.Minute)); err != nil {
		return fmt.Errorf("install the rules: %w", err)
	}
	setup := filepath.Join(tmp, "crs-setup.conf.example")
	if _, err := os.Stat(setup); err == nil {
		if _, err := run.Cmd(ctx, []string{"cp", setup, filepath.Join(wafPaths().Dir, "crs-setup.conf")}); err != nil {
			return err
		}
	}
	if err := os.WriteFile(filepath.Join(wafPaths().Dir, "crs-version"), []byte("4.7.0\n"), 0o644); err != nil {
		return err
	}
	return writeWAFConfig(WAFConfigureRequest{Mode: WAFDetectOnly})
}

// writeWAFConfig renders the file every protected vhost includes.
func writeWAFConfig(in WAFConfigureRequest) error {
	if err := os.MkdirAll(wafPaths().Dir, 0o755); err != nil {
		return err
	}
	// Written before the rules are rendered: wafIncludeLines reads this to
	// decide which files to include.
	if err := writeDisabledWAFRules(in.DisabledFiles); err != nil {
		return err
	}
	engine := "DetectionOnly"
	switch in.Mode {
	case WAFBlock:
		engine = "On"
	case WAFOff:
		engine = "Off"
	}

	var b strings.Builder
	b.WriteString("# Managed by OPanel. Rendered on every WAF change.\n")
	fmt.Fprintf(&b, "SecRuleEngine %s\n", engine)
	b.WriteString("SecRequestBodyAccess On\n")
	// 10 MB of request body is enough for a WordPress media upload to be
	// inspected; beyond that the engine passes it through rather than
	// buffering a video in memory.
	b.WriteString("SecRequestBodyLimit 10485760\n")
	b.WriteString("SecRequestBodyNoFilesLimit 131072\n")
	b.WriteString("SecRequestBodyLimitAction ProcessPartial\n")
	b.WriteString("SecResponseBodyAccess Off\n")
	b.WriteString("SecAuditEngine RelevantOnly\n")
	b.WriteString("SecAuditLogParts ABIJDEFHZ\n")
	fmt.Fprintf(&b, "SecAuditLog %s\n", wafPaths().AuditLog)
	b.WriteString("SecAuditLogType Serial\n")
	b.WriteString("SecTmpDir /tmp\n")
	b.WriteString("SecDataDir /tmp\n\n")

	if _, err := os.Stat(filepath.Join(wafPaths().Dir, "crs-setup.conf")); err == nil {
		fmt.Fprintf(&b, "Include %s/crs-setup.conf\n", wafPaths().Dir)
	}
	b.WriteString(wafIncludeLines())

	if len(in.ExcludedRules) > 0 {
		b.WriteString("\n# Rules an operator switched off after a false positive.\n")
		for _, id := range in.ExcludedRules {
			fmt.Fprintf(&b, "SecRuleRemoveById %s\n", id)
		}
	}
	return os.WriteFile(wafPaths().RulesFile, []byte(b.String()), 0o644)
}

// readWAFEvents pulls the recent entries out of the audit log.
//
// The serial audit format is a set of sections separated by boundary lines.
// Only a few fields matter for the panel's purpose -- who, what, which rule
// -- so this reads those rather than modelling the whole format.
func readWAFEvents(limit int) []WAFEvent {
	f, err := os.Open(wafPaths().AuditLog)
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return nil
	}
	// Only the tail is read: this file grows without bound on a busy server.
	const window = 1 << 20
	start := info.Size() - window
	if start < 0 {
		start = 0
	}
	buf := make([]byte, info.Size()-start)
	if _, err := f.ReadAt(buf, start); err != nil && len(buf) == 0 {
		return nil
	}

	return parseWAFEvents(string(buf), limit)
}

// parseWAFEvents reads both shapes ModSecurity records a block in: the
// multi-line audit record, and the single error-log line each server also
// writes.
func parseWAFEvents(text string, limit int) []WAFEvent {
	var out []WAFEvent
	var current WAFEvent
	for _, line := range strings.Split(text, "\n") {
		switch {
		case strings.HasPrefix(line, "--") && strings.HasSuffix(line, "-A--"):
			if current.RuleID != "" {
				out = append(out, current)
			}
			current = WAFEvent{}
		case strings.HasPrefix(line, "GET "), strings.HasPrefix(line, "POST "),
			strings.HasPrefix(line, "HEAD "), strings.HasPrefix(line, "PUT "):
			parts := strings.Fields(line)
			if len(parts) >= 2 {
				current.URI = parts[1]
			}
		case strings.HasPrefix(line, "Host: "):
			current.Host = strings.TrimSpace(strings.TrimPrefix(line, "Host: "))
		case strings.Contains(line, "[id \""):
			current.RuleID = between(line, "[id \"", "\"]")
			current.Message = between(line, "[msg \"", "\"]")
			current.Blocked = strings.Contains(line, "Access denied")
			// An error-log line carries the whole event on its own, rather
			// than being one part of a multi-line audit record. Both servers
			// write this form -- it is where LiteSpeed's blocks appear, since
			// it cannot open the audit log at all -- so it is finished here
			// and the next line starts fresh.
			if h := between(line, "[hostname \"", "\"]"); h != "" {
				current.Host = h
				current.URI = between(line, "[uri \"", "\"]")
				if c := between(line, "[client ", "]"); c != "" {
					current.ClientIP = strings.Fields(c)[0]
				}
				if t := between(line, "[", "]"); strings.Contains(t, ":") {
					current.At = t
				}
				out = append(out, current)
				current = WAFEvent{}
			}
		case strings.HasPrefix(line, "["):
			// The -A-- header line carries the timestamp and client address.
			fields := strings.Fields(line)
			if len(fields) >= 4 {
				current.At = strings.Trim(fields[0]+" "+fields[1], "[]")
				current.ClientIP = fields[3]
			}
		}
	}
	if current.RuleID != "" {
		out = append(out, current)
	}
	if len(out) > limit {
		out = out[len(out)-limit:]
	}
	// Newest first: the reason somebody opened this page is the thing that
	// just happened.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

func between(s, open, close string) string {
	i := strings.Index(s, open)
	if i < 0 {
		return ""
	}
	rest := s[i+len(open):]
	j := strings.Index(rest, close)
	if j < 0 {
		return ""
	}
	return rest[:j]
}
