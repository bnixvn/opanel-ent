// Package phpini is the whitelist of php.ini directives a site may set.
//
// A whitelist rather than a free-text ini file for two reasons. The obvious
// one is injection: these values end up inside the webserver's own
// configuration, and a value carrying a newline could write directives
// nobody asked for. The less obvious one is support load -- a customer who
// can set any directive can set one that stops their site booting, and the
// panel then has to explain why.
//
// Nothing here touches the disk or the network, so the rules are testable on
// their own and the same catalogue drives the form, the validation and the
// rendering.
package phpini

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Kind decides how a value is parsed, validated and rendered.
type Kind string

const (
	// KindBytes is a php.ini size: "256M", "1G", or a plain byte count.
	KindBytes Kind = "bytes"
	// KindInt is a plain number, normally seconds or a count.
	KindInt Kind = "int"
	// KindBool renders as php_admin_flag.
	KindBool Kind = "bool"
	// KindEnum is one of a fixed set of strings.
	KindEnum Kind = "enum"
	// KindList is a comma-separated list drawn from a fixed set.
	KindList Kind = "list"
)

// Directive is one editable php.ini setting.
type Directive struct {
	Name  string `json:"name"`
	Kind  Kind   `json:"kind"`
	Label string `json:"label"`
	Help  string `json:"help,omitempty"`

	// Default is what PHP does when the panel writes nothing. Shown in the
	// form as the placeholder, and used when a value is cleared.
	Default string `json:"default"`

	// Min and Max bound KindBytes and KindInt. Zero means unbounded.
	Min int64 `json:"min,omitempty"`
	Max int64 `json:"max,omitempty"`

	// Unlimited allows -1 in addition to the range, for directives where
	// php.ini reads that as "no limit".
	Unlimited bool `json:"unlimited,omitempty"`

	// Options lists the accepted values for KindEnum and KindList.
	Options []string `json:"options,omitempty"`

	// StaffOnly keeps a directive out of a customer's reach. These are the
	// ones where a change is a change to how safe the host is, not to how
	// the customer's own application behaves.
	StaffOnly bool `json:"staff_only,omitempty"`
}

// The ceilings are deliberately generous rather than tuned: they exist to
// stop one site claiming the whole machine, not to express a hosting plan.
// A plan that wants tighter limits enforces them where plans are enforced.
const (
	mib = 1 << 20
	gib = 1 << 30
)

// dangerousFunctions is the default disable_functions set: the calls that
// turn a PHP vulnerability into shell access on the host.
//
// Kept sorted, because Normalize sorts what it is given and the default has
// to come back from it unchanged.
var dangerousFunctions = []string{
	"dl", "exec", "link", "passthru", "pcntl_exec", "popen",
	"proc_open", "shell_exec", "symlink", "system",
}

var catalogue = []Directive{
	{
		Name: "memory_limit", Kind: KindBytes, Label: "Memory limit",
		Default: "256M", Min: 32 * mib, Max: 2 * gib, Unlimited: true,
		Help: "How much memory one request may use before PHP stops it.",
	},
	{
		Name: "max_execution_time", Kind: KindInt, Label: "Max execution time",
		Default: "30", Min: 1, Max: 3600, Unlimited: true,
		Help: "Seconds a script may run. Long imports and backups need more.",
	},
	{
		Name: "max_input_time", Kind: KindInt, Label: "Max input time",
		Default: "60", Min: 1, Max: 3600, Unlimited: true,
		Help: "Seconds PHP may spend reading the request, including an upload.",
	},
	{
		Name: "max_input_vars", Kind: KindInt, Label: "Max input variables",
		Default: "1000", Min: 100, Max: 100000,
		Help: "Fields one request may carry. Large menus and forms need more.",
	},
	{
		Name: "post_max_size", Kind: KindBytes, Label: "Max POST size",
		Default: "64M", Min: mib, Max: gib,
		Help: "Largest request body, which has to fit the whole upload.",
	},
	{
		Name: "upload_max_filesize", Kind: KindBytes, Label: "Max upload size",
		Default: "64M", Min: mib, Max: gib,
		Help: "Largest single uploaded file.",
	},
	{
		Name: "max_file_uploads", Kind: KindInt, Label: "Max files per upload",
		Default: "20", Min: 1, Max: 1000,
	},
	{
		Name: "display_errors", Kind: KindBool, Label: "Display errors",
		Default: "Off",
		Help: "Shows PHP errors in the page. Useful while building, a " +
			"disclosure risk once the site is live.",
	},
	{
		Name: "log_errors", Kind: KindBool, Label: "Log errors",
		Default: "On",
		Help:    "Writes errors to the site's error log, readable under Logs.",
	},
	{
		Name: "allow_url_fopen", Kind: KindBool, Label: "Allow URL fopen",
		Default: "On",
		Help: "Lets file functions open http:// addresses. Some applications " +
			"need it; leaving it off blocks a class of attack.",
	},
	{
		Name: "session.gc_maxlifetime", Kind: KindInt, Label: "Session lifetime",
		Default: "1440", Min: 60, Max: 86400,
		Help: "Seconds an idle session survives.",
	},
	{
		Name: "date.timezone", Kind: KindEnum, Label: "Timezone",
		Default: "UTC", Options: timezones,
	},
	{
		Name: "opcache.enable", Kind: KindBool, Label: "OPcache",
		Default: "On",
		Help:    "Caches compiled PHP. Switch off only while debugging.",
	},
	{
		Name: "zlib.output_compression", Kind: KindBool, Label: "Compress output in PHP",
		Default: "Off",
		Help:    "The webserver already compresses responses, so this is rarely wanted.",
	},
	{
		Name: "disable_functions", Kind: KindList, Label: "Disabled functions",
		Default: strings.Join(dangerousFunctions, ","), Options: dangerousFunctions,
		StaffOnly: true,
		Help: "Functions PHP refuses to run. These are the ones that reach " +
			"the shell, so removing one widens what a compromised site can do.",
	},
}

// timezones is a short list rather than every zone PHP knows: a select with
// four hundred entries is not a better answer than one holding the zones
// customers actually pick, and anything outside it can be set in the
// application itself.
var timezones = []string{
	"UTC",
	"Asia/Ho_Chi_Minh", "Asia/Bangkok", "Asia/Singapore", "Asia/Tokyo",
	"Asia/Seoul", "Asia/Shanghai", "Asia/Hong_Kong", "Asia/Kolkata", "Asia/Dubai",
	"Australia/Sydney",
	"Europe/London", "Europe/Paris", "Europe/Berlin", "Europe/Moscow",
	"America/New_York", "America/Chicago", "America/Denver", "America/Los_Angeles",
	"America/Sao_Paulo",
}

// Catalogue returns the editable directives, in the order they should appear.
func Catalogue() []Directive {
	out := make([]Directive, len(catalogue))
	copy(out, catalogue)
	return out
}

// Lookup finds a directive by name.
func Lookup(name string) (Directive, bool) {
	for _, d := range catalogue {
		if d.Name == name {
			return d, true
		}
	}
	return Directive{}, false
}

// ErrUnknown is returned for a directive that is not on the whitelist.
type ErrUnknown struct{ Name string }

func (e *ErrUnknown) Error() string {
	return fmt.Sprintf("%q is not a setting this panel manages", e.Name)
}

// Normalize validates one value and returns it in canonical form.
//
// An empty value means "leave it at the default" and comes back empty, which
// the caller stores as a deletion rather than as a setting.
func Normalize(name, value string) (string, error) {
	d, ok := Lookup(name)
	if !ok {
		return "", &ErrUnknown{Name: name}
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	// Nothing below can legitimately contain these, and either would let a
	// value escape the line it is written on.
	if strings.ContainsAny(value, "\r\n") {
		return "", fmt.Errorf("%s cannot contain a line break", d.Label)
	}

	switch d.Kind {
	case KindBytes:
		return normalizeBytes(d, value)
	case KindInt:
		return normalizeInt(d, value)
	case KindBool:
		return normalizeBool(d, value)
	case KindEnum:
		for _, o := range d.Options {
			if strings.EqualFold(o, value) {
				return o, nil
			}
		}
		return "", fmt.Errorf("%s cannot be %q", d.Label, value)
	case KindList:
		return normalizeList(d, value)
	}
	return "", fmt.Errorf("%s has no validation rule", d.Label)
}

func normalizeBytes(d Directive, value string) (string, error) {
	n, err := ParseBytes(value)
	if err != nil {
		return "", fmt.Errorf("%s must be a size such as 256M", d.Label)
	}
	if n < 0 {
		if !d.Unlimited || n != -1 {
			return "", fmt.Errorf("%s cannot be negative", d.Label)
		}
		return "-1", nil
	}
	if d.Min > 0 && n < d.Min {
		return "", fmt.Errorf("%s cannot be below %s", d.Label, FormatBytes(d.Min))
	}
	if d.Max > 0 && n > d.Max {
		return "", fmt.Errorf("%s cannot be above %s", d.Label, FormatBytes(d.Max))
	}
	return FormatBytes(n), nil
}

func normalizeInt(d Directive, value string) (string, error) {
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return "", fmt.Errorf("%s must be a whole number", d.Label)
	}
	if n < 0 {
		if !d.Unlimited || n != -1 {
			return "", fmt.Errorf("%s cannot be negative", d.Label)
		}
		return "-1", nil
	}
	if d.Min > 0 && n < d.Min {
		return "", fmt.Errorf("%s cannot be below %d", d.Label, d.Min)
	}
	if d.Max > 0 && n > d.Max {
		return "", fmt.Errorf("%s cannot be above %d", d.Label, d.Max)
	}
	return strconv.FormatInt(n, 10), nil
}

func normalizeBool(d Directive, value string) (string, error) {
	switch strings.ToLower(value) {
	case "on", "1", "true", "yes":
		return "On", nil
	case "off", "0", "false", "no":
		return "Off", nil
	}
	return "", fmt.Errorf("%s must be On or Off", d.Label)
}

func normalizeList(d Directive, value string) (string, error) {
	seen := make(map[string]bool, len(d.Options))
	var out []string
	for _, part := range strings.Split(value, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		var match string
		for _, o := range d.Options {
			if strings.EqualFold(o, part) {
				match = o
				break
			}
		}
		if match == "" {
			return "", fmt.Errorf("%s cannot include %q", d.Label, part)
		}
		if !seen[match] {
			seen[match] = true
			out = append(out, match)
		}
	}
	sort.Strings(out)
	return strings.Join(out, ","), nil
}

// Check validates a whole set together.
//
// Some of these only make sense against each other: an upload larger than
// the request body it arrives in silently fails, and the customer sees a
// blank page rather than an error. Catching it here is the difference
// between a form that explains itself and a support ticket.
func Check(settings map[string]string) (map[string]string, error) {
	out := make(map[string]string, len(settings))
	for name, raw := range settings {
		v, err := Normalize(name, raw)
		if err != nil {
			return nil, err
		}
		if v != "" {
			out[name] = v
		}
	}

	// Effective value: what is being saved, or PHP's default when the
	// customer is not setting it. Comparing against the default matters --
	// raising only the upload size is exactly how this goes wrong.
	effective := func(name string) int64 {
		v, ok := out[name]
		if !ok {
			d, found := Lookup(name)
			if !found {
				return -1
			}
			v = d.Default
		}
		n, err := ParseBytes(v)
		if err != nil {
			return -1
		}
		return n
	}

	upload, post, mem := effective("upload_max_filesize"), effective("post_max_size"), effective("memory_limit")
	if post >= 0 && upload > post {
		return nil, fmt.Errorf(
			"an upload of %s cannot fit in a %s request: raise the max POST size to at least the max upload size",
			FormatBytes(upload), FormatBytes(post))
	}
	if mem >= 0 && post >= 0 && post > mem {
		return nil, fmt.Errorf(
			"a %s request cannot be handled with a %s memory limit: raise the memory limit to at least the max POST size",
			FormatBytes(post), FormatBytes(mem))
	}
	return out, nil
}

// StaffOnlyNames returns the directives an ordinary customer may not set.
func StaffOnlyNames() []string {
	var out []string
	for _, d := range catalogue {
		if d.StaffOnly {
			out = append(out, d.Name)
		}
	}
	return out
}

// ParseBytes reads a php.ini size such as "256M" into a byte count.
func ParseBytes(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty size")
	}
	mult := int64(1)
	switch s[len(s)-1] {
	case 'k', 'K':
		mult, s = 1<<10, s[:len(s)-1]
	case 'm', 'M':
		mult, s = mib, s[:len(s)-1]
	case 'g', 'G':
		mult, s = gib, s[:len(s)-1]
	}
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0, err
	}
	if n < 0 {
		// -1 is php.ini's "no limit"; it carries no suffix.
		return n, nil
	}
	return n * mult, nil
}

// FormatBytes writes a byte count the way php.ini does.
func FormatBytes(n int64) string {
	if n < 0 {
		return "-1"
	}
	switch {
	case n >= gib && n%gib == 0:
		return strconv.FormatInt(n/gib, 10) + "G"
	case n >= mib && n%mib == 0:
		return strconv.FormatInt(n/mib, 10) + "M"
	case n >= 1<<10 && n%(1<<10) == 0:
		return strconv.FormatInt(n/(1<<10), 10) + "K"
	}
	return strconv.FormatInt(n, 10)
}

// Render turns a validated set into the lines a vhost carries, sorted so the
// same settings always produce the same configuration file.
//
// php_admin_value rather than php_value: these are the limits the host is
// imposing, and a directive the application can raise with ini_set() is not
// a limit.
func Render(settings map[string]string) []string {
	names := make([]string, 0, len(settings))
	for name := range settings {
		if _, ok := Lookup(name); ok {
			names = append(names, name)
		}
	}
	sort.Strings(names)

	out := make([]string, 0, len(names))
	for _, name := range names {
		d, _ := Lookup(name)
		keyword := "php_admin_value"
		if d.Kind == KindBool {
			keyword = "php_admin_flag"
		}
		out = append(out, keyword+" "+name+" "+settings[name])
	}
	return out
}
