package actions

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bnixvn/opanel-ent/internal/agent"
	"github.com/bnixvn/opanel-ent/internal/platform/run"
)

// Where the WAF's own files live. Under the webserver's configuration
// directory because that is what reads them.
const (
	wafDir       = "/usr/local/lsws/conf/opanel/waf"
	wafRulesFile = wafDir + "/rules.conf"
	wafAuditLog  = "/usr/local/lsws/logs/modsec_audit.log"
	modSecModule = "/usr/local/lsws/modules/mod_security.so"
)

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
}

// Validate checks the mode and the rule ids.
func (r *WAFConfigureRequest) Validate() error {
	switch r.Mode {
	case WAFOff, WAFDetectOnly, WAFBlock:
	default:
		return fmt.Errorf("mode must be %q, %q or %q", WAFOff, WAFDetectOnly, WAFBlock)
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
	if _, err := os.Stat(modSecModule); err == nil {
		st.ModuleAvailable = true
	}
	if _, err := os.Stat(wafRulesFile); err != nil {
		return st
	}
	st.RulesInstalled = true
	// Counted across the rule files rather than the one that includes them:
	// rules.conf holds a handful of directives and an Include, so counting
	// there reports zero rules on a fully installed rule set.
	matches, _ := filepath.Glob(filepath.Join(wafDir, "rules", "*.conf"))
	for _, path := range matches {
		if body, err := os.ReadFile(path); err == nil {
			st.RuleCount += strings.Count(string(body), "SecRule ")
		}
	}

	// The CRS records its own version in a setup file; showing it means an
	// operator can tell whether their rules are three years old.
	if v, err := os.ReadFile(filepath.Join(wafDir, "crs-version")); err == nil {
		st.RulesVersion = strings.TrimSpace(string(v))
	}
	return st
}

// crsURL is the OWASP Core Rule Set release the panel installs.
const crsURL = "https://github.com/coreruleset/coreruleset/releases/download/v4.7.0/coreruleset-4.7.0-minimal.tar.gz"

// installCRS downloads and unpacks the rule set.
func installCRS(ctx context.Context) error {
	if _, err := os.Stat(modSecModule); err != nil {
		return errors.New("this webserver has no ModSecurity module, so there is nothing to configure")
	}
	if err := os.MkdirAll(wafDir, 0o755); err != nil {
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
		"cp", "-a", filepath.Join(tmp, "rules"), wafDir + "/",
	}, run.Timeout(time.Minute)); err != nil {
		return fmt.Errorf("install the rules: %w", err)
	}
	setup := filepath.Join(tmp, "crs-setup.conf.example")
	if _, err := os.Stat(setup); err == nil {
		if _, err := run.Cmd(ctx, []string{"cp", setup, filepath.Join(wafDir, "crs-setup.conf")}); err != nil {
			return err
		}
	}
	if err := os.WriteFile(filepath.Join(wafDir, "crs-version"), []byte("4.7.0\n"), 0o644); err != nil {
		return err
	}
	return writeWAFConfig(WAFConfigureRequest{Mode: WAFDetectOnly})
}

// writeWAFConfig renders the file every protected vhost includes.
func writeWAFConfig(in WAFConfigureRequest) error {
	if err := os.MkdirAll(wafDir, 0o755); err != nil {
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
	fmt.Fprintf(&b, "SecAuditLog %s\n", wafAuditLog)
	b.WriteString("SecAuditLogType Serial\n")
	b.WriteString("SecTmpDir /tmp\n")
	b.WriteString("SecDataDir /tmp\n\n")

	if _, err := os.Stat(filepath.Join(wafDir, "crs-setup.conf")); err == nil {
		fmt.Fprintf(&b, "Include %s/crs-setup.conf\n", wafDir)
	}
	fmt.Fprintf(&b, "Include %s/rules/*.conf\n", wafDir)

	if len(in.ExcludedRules) > 0 {
		b.WriteString("\n# Rules an operator switched off after a false positive.\n")
		for _, id := range in.ExcludedRules {
			fmt.Fprintf(&b, "SecRuleRemoveById %s\n", id)
		}
	}
	return os.WriteFile(wafRulesFile, []byte(b.String()), 0o644)
}

// readWAFEvents pulls the recent entries out of the audit log.
//
// The serial audit format is a set of sections separated by boundary lines.
// Only a few fields matter for the panel's purpose -- who, what, which rule
// -- so this reads those rather than modelling the whole format.
func readWAFEvents(limit int) []WAFEvent {
	f, err := os.Open(wafAuditLog)
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

	var out []WAFEvent
	var current WAFEvent
	for _, line := range strings.Split(string(buf), "\n") {
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
