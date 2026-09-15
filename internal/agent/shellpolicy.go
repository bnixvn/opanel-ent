package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// ShellCommands is the default list of what a terminal session may run.
//
// A guard rail, not a sandbox, and the difference matters enough to write
// down. Half of this list executes arbitrary programs by design -- php -r
// runs anything, so do node, find -exec, git's pager and aliases, tar
// --to-command, and every package manager here -- so somebody determined to
// run something else will. What it does do is make the environment
// predictable and keep the rest of the system out of reach of a mistake: no
// systemctl, no useradd, no mysql, no package manager.
//
// The boundary that does hold is the one underneath: the shell runs as an
// unprivileged account with one group, never as root, and an SFTP session is
// chrooted into a home it cannot replace. That is why an operator is allowed
// to widen this list, or turn it off for their own sessions, without giving
// anything away -- there is nothing here for it to give.
var ShellCommands = []string{
	"artisan", "cat", "cd", "chmod", "chown", "clear", "composer", "cp",
	"curl", "date", "df", "diff", "du", "echo", "find", "git", "grep",
	"head", "less", "ls", "mkdir", "mv", "node", "npm", "npx", "php",
	"phpunit", "pwd", "rm", "tail", "tar", "touch", "unzip", "wget",
	"which", "whoami", "wp", "yarn", "zip",
}

// SessionRoot holds the per-session directories.
//
// Under /run so a reboot clears anything a crash left behind, and beside
// /run/opanel rather than inside it: that directory is 0750 owned by the
// panel's group, and the account the shell runs as is not in that group, so
// a session directory underneath it is one the session cannot traverse into.
// The failure reads as "rc.sh: Permission denied" and looks like a broken
// file rather than an unreachable one.
const SessionRoot = "/run/opanel-shell"

// WPCLIPharPath is where the installer puts WP-CLI. Repeated here rather
// than imported: internal/agent cannot import internal/agent/actions, which
// imports this package's siblings.
const WPCLIPharPath = "/usr/local/lib/opanel/wp-cli.phar"

// UnrestrictedPATH is what a session gets when the list is turned off.
const UnrestrictedPATH = "/usr/local/bin:/usr/bin:/bin:/usr/local/sbin"

// searchPath is where a real copy of a command might live.
var searchPath = []string{"/usr/local/bin", "/usr/bin", "/bin", "/usr/local/sbin"}

// phpGlobs are where an interpreter lives, in the order to prefer them.
// Remi's tree is first because it is what an Apache host has; the LiteSpeed
// and CloudLinux paths follow for hosts running those.
var phpGlobs = []string{
	"/opt/remi/php*/root/usr/bin/php",
	"/usr/local/lsws/lsphp*/bin/php",
	"/opt/alt/php*/usr/bin/php",
}

// commandName is the shape of an entry in the list. A name with a slash in
// it would be a path, and linking one under a name of somebody's choosing is
// how a list of commands quietly becomes a list of aliases for something
// else.
var commandName = regexp.MustCompile(`^[A-Za-z0-9._+-]{1,64}$`)

// SessionPolicy is the PATH and startup file one session gets.
type SessionPolicy struct {
	// Dir is the session's private directory, removed by Cleanup. Empty when
	// the session is unrestricted.
	Dir string
	// PATH for the session.
	PATH string
	// RC is the startup file, or empty when the session is unrestricted.
	RC string
	// Linked is what was actually found on this server, which is not always
	// what was asked for.
	Linked []string
}

// Cleanup removes whatever the policy made.
func (p SessionPolicy) Cleanup() {
	if p.Dir != "" {
		_ = os.RemoveAll(p.Dir)
	}
}

// BuildSessionPolicy prepares one session's command list.
//
// Per session rather than one directory shared by all of them: the list is
// configurable, so two sessions can legitimately want different ones, and a
// shared directory rebuilt underneath a running shell is a PATH that changes
// while somebody is typing into it.
func BuildSessionPolicy(commands []string, unrestricted bool) (SessionPolicy, error) {
	if unrestricted {
		return SessionPolicy{PATH: UnrestrictedPATH}, nil
	}
	commands = sanitise(commands)
	if len(commands) == 0 {
		commands = ShellCommands
	}

	if err := os.MkdirAll(SessionRoot, 0o755); err != nil {
		return SessionPolicy{}, fmt.Errorf("agent: create session root: %w", err)
	}
	// MkdirAll leaves an existing directory's mode alone, and a root created
	// by an older build may be tighter than this needs.
	if err := os.Chmod(SessionRoot, 0o755); err != nil {
		return SessionPolicy{}, fmt.Errorf("agent: open session root: %w", err)
	}
	dir, err := os.MkdirTemp(SessionRoot, "s-")
	if err != nil {
		return SessionPolicy{}, fmt.Errorf("agent: create session dir: %w", err)
	}
	ok := false
	defer func() {
		if !ok {
			_ = os.RemoveAll(dir)
		}
	}()
	// Traversable by the account the shell runs as, which is not this one.
	if err := os.Chmod(dir, 0o755); err != nil {
		return SessionPolicy{}, err
	}

	bin := filepath.Join(dir, "bin")
	linked, err := linkCommands(bin, commands)
	if err != nil {
		return SessionPolicy{}, err
	}
	rc := filepath.Join(dir, "rc.sh")
	if err := os.WriteFile(rc, []byte(renderRC(bin, commands)), 0o644); err != nil {
		return SessionPolicy{}, err
	}
	ok = true
	return SessionPolicy{Dir: dir, PATH: bin, RC: rc, Linked: linked}, nil
}

// sanitise drops anything that is not a bare command name. An operator types
// this list, so it arrives as whatever they pasted.
func sanitise(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, name := range in {
		name = strings.TrimSpace(name)
		if name == "" || seen[name] || !commandName.MatchString(name) {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// linkCommands fills a directory with links to the named commands.
func linkCommands(bin string, commands []string) ([]string, error) {
	if err := os.MkdirAll(bin, 0o755); err != nil {
		return nil, fmt.Errorf("agent: create session bin: %w", err)
	}
	php := findPHP()
	linked := make([]string, 0, len(commands))

	for _, name := range commands {
		switch name {
		case "artisan":
			// A file in a Laravel project, not a program. It is on the list
			// because people type it; what runs it is php.
			continue
		case "php":
			if php == "" {
				continue
			}
			if err := os.Symlink(php, filepath.Join(bin, "php")); err != nil {
				return nil, fmt.Errorf("agent: link php: %w", err)
			}
			linked = append(linked, "php")
			continue
		case "wp":
			// WP-CLI is a phar, so it needs a launcher rather than a link.
			if php == "" || !isFile(WPCLIPharPath) {
				continue
			}
			script := fmt.Sprintf(
				"#!/bin/sh\n# Managed by OPanel.\nexec %s %s \"$@\"\n",
				php, WPCLIPharPath)
			if err := os.WriteFile(filepath.Join(bin, "wp"), []byte(script), 0o755); err != nil {
				return nil, fmt.Errorf("agent: write wp launcher: %w", err)
			}
			linked = append(linked, "wp")
			continue
		}

		target := findCommand(name)
		if target == "" {
			// Asked for but not installed on this server. Not a failure: the
			// list says what a session may run, not what a distribution was
			// obliged to ship.
			continue
		}
		if err := os.Symlink(target, filepath.Join(bin, name)); err != nil {
			return nil, fmt.Errorf("agent: link %s: %w", name, err)
		}
		linked = append(linked, name)
	}
	return linked, nil
}

// findPHP picks the newest interpreter installed.
//
// There is no /usr/bin/php on a server that runs PHP through the webserver's
// own builds, which is every server this panel sets up -- so a terminal with
// no php on it would be one that cannot run composer, wp or artisan, which
// is most of what the list is for.
func findPHP() string {
	if p := findCommand("php"); p != "" {
		return p
	}
	best := ""
	for _, pattern := range phpGlobs {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			continue
		}
		for _, m := range matches {
			if isFile(m) && m > best {
				best = m
			}
		}
	}
	return best
}

func findCommand(name string) string {
	for _, dir := range searchPath {
		full := filepath.Join(dir, name)
		st, err := os.Stat(full)
		if err != nil || st.IsDir() || st.Mode().Perm()&0o111 == 0 {
			continue
		}
		return full
	}
	return ""
}

func isFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// renderRC is the file a restricted session starts from.
//
// It replaces ~/.bashrc rather than adding to it -- the account owns its
// home and could otherwise put the PATH back -- and it is read because the
// session is started with --rcfile, not as a login shell, so /etc/profile
// and ~/.bash_profile do not get a turn at PATH either.
func renderRC(bin string, commands []string) string {
	allowed := strings.Join(commands, " ")
	var b strings.Builder
	fmt.Fprintf(&b, `# Managed by OPanel. Written for this session only.

PATH=%s
export PATH
readonly PATH 2>/dev/null

OPANEL_ALLOWED=%q
export OPANEL_ALLOWED

PS1='[\u@\h \W]\$ '

# What somebody sees when they reach for something that is not here. The
# default -- "command not found" -- reads as a broken server rather than a
# deliberate list.
command_not_found_handle() {
    printf '%%s is not available in this terminal.\n' "$1" >&2
    printf 'Available: %%s\n' "$OPANEL_ALLOWED" >&2
    return 127
}

# The list is a PATH, so naming a program by its full path would walk around
# it. Refuse that too. Only paths: a bare word is already answered by PATH,
# and checking every command here would break assignments and loops for the
# sake of a rule this cannot enforce anyway -- php and node are on the list
# and both run whatever they are given.
__opanel_guard() {
    local word=${BASH_COMMAND%%%% *}
    case "$word" in
        *=*) return 0 ;;
        */*) ;;
        *) return 0 ;;
    esac
    local base=${word##*/}
    case " $OPANEL_ALLOWED " in
        *" $base "*) return 0 ;;
    esac
    printf '%%s is not available in this terminal.\n' "$base" >&2
    return 1
}
shopt -s extdebug
trap '__opanel_guard' DEBUG
`, bin, allowed)
	return b.String()
}
