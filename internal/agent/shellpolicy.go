package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ShellCommands is what a terminal session may run.
//
// A guard rail, not a sandbox, and the difference matters enough to write
// down. Half of this list executes arbitrary programs by design -- php -r
// runs anything, so do node, find -exec, git's pager and aliases, tar
// --to-command, and every package manager here -- so somebody determined to
// run something else will. What it does do is make the environment
// predictable and keep the rest of the system out of reach of a mistake: no
// systemctl, no useradd, no mysql, no package manager, no editor that writes
// where it should not.
//
// The boundary that does hold is the one underneath: the shell runs as an
// unprivileged account with one group, never as root, and an SFTP session is
// chrooted into a home it cannot replace.
var ShellCommands = []string{
	"artisan", "cat", "cd", "chmod", "chown", "clear", "composer", "cp",
	"curl", "date", "df", "diff", "du", "echo", "find", "git", "grep",
	"head", "less", "ls", "mkdir", "mv", "node", "npm", "npx", "php",
	"phpunit", "pwd", "rm", "tail", "tar", "touch", "unzip", "wget",
	"which", "whoami", "wp", "yarn", "zip",
}

// ShellDir holds the curated bin directory and the startup file.
//
// Under /usr/local/lib rather than a customer's home: the point is that the
// occupant of the shell cannot edit it.
const ShellDir = "/usr/local/lib/opanel/shell"

// ShellBinDir is the only directory on a session's PATH.
func ShellBinDir() string { return filepath.Join(ShellDir, "bin") }

// ShellRC is the startup file a session is given.
func ShellRC() string { return filepath.Join(ShellDir, "rc.sh") }

// searchPath is where a real copy of an allowed command might live.
var searchPath = []string{"/usr/local/bin", "/usr/bin", "/bin", "/usr/local/sbin"}

// BuildShellPolicy writes the curated bin directory and the startup file.
//
// Rebuilt from scratch each time rather than patched: a stale symlink to a
// command that has since been removed from the list is exactly the thing
// this is supposed to prevent, and there are forty of them, so making the
// directory again is cheaper than reasoning about what changed.
func BuildShellPolicy() (linked []string, err error) {
	bin := ShellBinDir()
	if err := os.RemoveAll(bin); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("agent: clear shell bin: %w", err)
	}
	if err := os.MkdirAll(bin, 0o755); err != nil {
		return nil, fmt.Errorf("agent: create shell bin: %w", err)
	}

	php := findPHP()

	for _, name := range ShellCommands {
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
			// On the list but not installed on this server. Not a failure:
			// the list says what a session may run, not what a distribution
			// was obliged to ship.
			continue
		}
		if err := os.Symlink(target, filepath.Join(bin, name)); err != nil {
			return nil, fmt.Errorf("agent: link %s: %w", name, err)
		}
		linked = append(linked, name)
	}
	sort.Strings(linked)

	if err := os.WriteFile(ShellRC(), []byte(shellRC()), 0o644); err != nil {
		return nil, fmt.Errorf("agent: write shell rc: %w", err)
	}
	return linked, nil
}

// WPCLIPharPath is where the installer puts WP-CLI. Repeated here rather
// than imported: internal/agent cannot import internal/agent/actions, which
// imports it.
const WPCLIPharPath = "/usr/local/lib/opanel/wp-cli.phar"

// phpGlobs are where an interpreter lives, newest last so the last match
// wins. lsphp today; alt-php when this moves to CloudLinux.
var phpGlobs = []string{
	"/usr/local/lsws/lsphp*/bin/php",
	"/opt/alt/php*/usr/bin/php",
}

// findPHP picks the newest interpreter installed.
//
// There is no /usr/bin/php on a server that runs PHP through the webserver's
// own builds, which is every server this panel sets up -- so a terminal with
// no php on it would be a terminal that cannot run composer, wp or artisan,
// which is most of what the list is for.
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
		sort.Strings(matches)
		for _, m := range matches {
			if isFile(m) {
				best = m
			}
		}
	}
	return best
}

func isFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
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

// shellRC is the file every terminal session starts from.
//
// It replaces ~/.bashrc rather than adding to it -- the account owns its
// home and could otherwise put the PATH back -- and it is read because the
// session is started with --rcfile, not as a login shell, so /etc/profile
// and ~/.bash_profile do not get a turn at PATH either.
func shellRC() string {
	allowed := strings.Join(ShellCommands, " ")
	var b strings.Builder
	fmt.Fprintf(&b, `# Managed by OPanel. Rewritten whenever the agent starts.

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
`, ShellBinDir(), allowed)
	return b.String()
}
