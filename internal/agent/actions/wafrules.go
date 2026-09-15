package actions

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/bnixvn/opanel-ent/internal/agent"
)

// WAFRule is one OWASP rule file, as a category somebody can switch off.
//
// A file rather than an individual rule id: the rules inside one file are
// one kind of attack, they are written to work together, and "turn off SQL
// injection checks because the CMS stores SQL in a form field" is the
// decision people actually need to make.
type WAFRule struct {
	// File is the name on disk, which is also the id.
	File string `json:"file"`
	// Title is what it protects against, in words.
	Title string `json:"title"`
	// Help says when somebody might turn it off.
	Help string `json:"help,omitempty"`
	// Phase is "request" or "response", so the list can be grouped.
	Phase string `json:"phase"`
	// Rules is how many SecRule directives the file holds.
	Rules int `json:"rules"`
	// Enabled is whether it is currently included.
	Enabled bool `json:"enabled"`
	// Required marks the files the rule set cannot run without. Switching
	// off initialisation or blocking evaluation does not relax the rules, it
	// breaks them, so those are not offered as choices.
	Required bool `json:"required,omitempty"`
}

// wafRuleInfo describes the CRS files worth naming. Anything not here still
// appears, with a title derived from its filename.
var wafRuleInfo = map[string]struct{ title, help string }{
	"REQUEST-901-INITIALIZATION":                      {"Rule set initialisation", ""},
	"REQUEST-905-COMMON-EXCEPTIONS":                   {"Common exceptions", ""},
	"REQUEST-911-METHOD-ENFORCEMENT":                  {"HTTP method enforcement", "Blocks methods a website does not use. Turn off for an application that needs PUT or DELETE."},
	"REQUEST-913-SCANNER-DETECTION":                   {"Scanner detection", "Blocks known vulnerability scanners by their signatures."},
	"REQUEST-920-PROTOCOL-ENFORCEMENT":                {"HTTP protocol enforcement", "Rejects malformed requests. A common source of false positives with old API clients."},
	"REQUEST-921-PROTOCOL-ATTACK":                     {"HTTP protocol attacks", "Request smuggling and response splitting."},
	"REQUEST-922-MULTIPART-ATTACK":                    {"Multipart attacks", "Malformed file uploads."},
	"REQUEST-930-APPLICATION-ATTACK-LFI":              {"Local file inclusion", "Attempts to read files off the server through a path in a parameter."},
	"REQUEST-931-APPLICATION-ATTACK-RFI":              {"Remote file inclusion", "Attempts to make the application fetch and run code from elsewhere."},
	"REQUEST-932-APPLICATION-ATTACK-RCE":              {"Remote code execution", "Shell commands in parameters. The rule set most worth keeping on."},
	"REQUEST-933-APPLICATION-ATTACK-PHP":              {"PHP injection", "PHP code and dangerous function names in parameters."},
	"REQUEST-934-APPLICATION-ATTACK-GENERIC":          {"Generic application attacks", "Server-side template injection and similar."},
	"REQUEST-941-APPLICATION-ATTACK-XSS":              {"Cross-site scripting", "Script in parameters. Sometimes trips on a page builder or a rich text editor."},
	"REQUEST-942-APPLICATION-ATTACK-SQLI":             {"SQL injection", "The most common false positive on a site whose forms accept SQL-like text."},
	"REQUEST-943-APPLICATION-ATTACK-SESSION-FIXATION": {"Session fixation", "Attempts to set somebody else's session id."},
	"REQUEST-944-APPLICATION-ATTACK-JAVA":             {"Java attacks", "Only relevant to a site running Java. Safe to turn off on a PHP-only server."},
	"REQUEST-949-BLOCKING-EVALUATION":                 {"Blocking evaluation", ""},
	"RESPONSE-950-DATA-LEAKAGES":                      {"Data leakage", "Stops error pages and stack traces reaching visitors."},
	"RESPONSE-951-DATA-LEAKAGES-SQL":                  {"Database error leakage", "Stops SQL errors reaching visitors, which is how an injection is found."},
	"RESPONSE-952-DATA-LEAKAGES-JAVA":                 {"Java error leakage", ""},
	"RESPONSE-953-DATA-LEAKAGES-PHP":                  {"PHP error leakage", "Stops PHP warnings and paths reaching visitors."},
	"RESPONSE-954-DATA-LEAKAGES-IIS":                  {"IIS error leakage", ""},
	"RESPONSE-955-WEB-SHELLS":                         {"Web shells", "Recognises the output of a shell somebody left in the document root."},
	"RESPONSE-959-BLOCKING-EVALUATION":                {"Response blocking evaluation", ""},
	"RESPONSE-980-CORRELATION":                        {"Correlation and scoring", ""},
}

// requiredWAFRules are the files that make the rest work. Offering them as
// choices would let somebody break the rule set while believing they had
// merely relaxed it.
var requiredWAFRules = map[string]bool{
	"REQUEST-901-INITIALIZATION":       true,
	"REQUEST-905-COMMON-EXCEPTIONS":    true,
	"REQUEST-949-BLOCKING-EVALUATION":  true,
	"RESPONSE-959-BLOCKING-EVALUATION": true,
	"RESPONSE-980-CORRELATION":         true,
}

// WAFRulesResult is the rule set as a list of categories.
type WAFRulesResult struct {
	Rules []WAFRule `json:"rules"`
}

func registerWAFRules(r *agent.Registry) {
	agent.Register(r, "waf.rules", 1, func(_ context.Context, _ struct{}) (WAFRulesResult, error) {
		return WAFRulesResult{Rules: listWAFRules()}, nil
	})
}

// listWAFRules reads the installed rule files.
func listWAFRules() []WAFRule {
	dir := filepath.Join(wafPaths().Dir, "rules")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	disabled := readDisabledWAFRules()

	out := make([]WAFRule, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		// .data files are lookup tables the rules read, not rules; the
		// .example files ship as templates and are never included.
		if !strings.HasSuffix(name, ".conf") {
			continue
		}
		base := strings.TrimSuffix(name, ".conf")
		info := wafRuleInfo[base]
		title := info.title
		if title == "" {
			title = prettyRuleName(base)
		}
		phase := "request"
		if strings.HasPrefix(base, "RESPONSE") {
			phase = "response"
		}
		out = append(out, WAFRule{
			File: name, Title: title, Help: info.help, Phase: phase,
			Rules:    countSecRules(filepath.Join(dir, name)),
			Enabled:  !disabled[name],
			Required: requiredWAFRules[base],
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].File < out[j].File })
	return out
}

// prettyRuleName turns a CRS filename into something readable.
func prettyRuleName(base string) string {
	parts := strings.Split(base, "-")
	if len(parts) < 3 {
		return base
	}
	words := strings.ToLower(strings.Join(parts[2:], " "))
	if words == "" {
		return base
	}
	return strings.ToUpper(words[:1]) + words[1:] + " (" + parts[1] + ")"
}

// countSecRules counts the directives in a file, so the list can show how
// much each category actually carries.
func countSecRules(path string) int {
	body, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	n := 0
	for _, line := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "SecRule ") {
			n++
		}
	}
	return n
}

// disabledRulesFile records which categories are off, so the list and the
// rendered configuration agree after a restart.
var disabledRulesFile = filepath.Join(wafPaths().Dir, "disabled-rules")

func readDisabledWAFRules() map[string]bool {
	out := map[string]bool{}
	body, err := os.ReadFile(disabledRulesFile)
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(body), "\n") {
		if name := strings.TrimSpace(line); name != "" && !strings.HasPrefix(name, "#") {
			out[name] = true
		}
	}
	return out
}

func writeDisabledWAFRules(names []string) error {
	var b strings.Builder
	b.WriteString("# Managed by OPanel. Rule files switched off in the panel.\n")
	for _, n := range names {
		b.WriteString(n)
		b.WriteString("\n")
	}
	return os.WriteFile(disabledRulesFile, []byte(b.String()), 0o640)
}

// wafIncludeLines renders one Include per enabled file, in filename order.
//
// Explicit includes rather than a glob, because a glob cannot leave one file
// out -- and leaving one out is the whole point of being able to switch a
// category off. The order matters: CRS numbers its files so that
// initialisation runs first and blocking evaluation last, and sorted names
// give exactly that.
func wafIncludeLines() string {
	rules := listWAFRules()
	var b strings.Builder
	for _, r := range rules {
		if !r.Enabled && !r.Required {
			b.WriteString("# switched off in the panel: " + r.File + "\n")
			continue
		}
		b.WriteString("Include " + filepath.Join(wafPaths().Dir, "rules", r.File) + "\n")
	}
	if b.Len() == 0 {
		// No rule files at all: the glob is what the first install had, and
		// falling back to it beats writing a configuration that protects
		// nothing without saying so.
		return "Include " + filepath.Join(wafPaths().Dir, "rules", "*.conf") + "\n"
	}
	return b.String()
}

// validDisabledRuleName keeps a name from becoming a path.
func validDisabledRuleName(name string) error {
	if name == "" || strings.ContainsAny(name, "/\\") || strings.Contains(name, "..") ||
		!strings.HasSuffix(name, ".conf") {
		return fmt.Errorf("%q is not a rule file", name)
	}
	return nil
}
