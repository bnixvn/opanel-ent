package actions

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/bnixvn/opanel-ent/internal/agent"
	"github.com/bnixvn/opanel-ent/internal/cron"
	"github.com/bnixvn/opanel-ent/internal/platform/linuxuser"
	"github.com/bnixvn/opanel-ent/internal/platform/run"
)

// CronRequest names an account's crontab.
type CronRequest struct {
	Owner string `json:"owner"`
}

// Validate checks the account name.
func (r *CronRequest) Validate() error {
	if !linuxuser.PlausibleName(r.Owner) {
		return fmt.Errorf("%q is not an acceptable account name", r.Owner)
	}
	return nil
}

// CronWriteRequest replaces an account's crontab.
type CronWriteRequest struct {
	Owner string `json:"owner"`
	// Text is the complete file. The panel renders it from its own rows, and
	// the agent validates every job line again before installing it.
	Text string `json:"text"`
}

// Validate re-checks every job line.
//
// Checked here as well as in the panel because this is the process that
// writes the file: a line the panel got wrong would run as the customer, on
// a schedule, for ever.
func (r *CronWriteRequest) Validate() error {
	if !linuxuser.PlausibleName(r.Owner) {
		return fmt.Errorf("%q is not an acceptable account name", r.Owner)
	}
	if len(r.Text) > 256<<10 {
		return fmt.Errorf("that crontab is too large")
	}
	jobs := 0
	for _, line := range strings.Split(r.Text, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if isEnvLine(trimmed) {
			continue
		}
		fields := strings.Fields(trimmed)
		if len(fields) < 6 {
			return fmt.Errorf("%q is not a crontab line", firstLines(trimmed, 1))
		}
		if err := cron.ValidateSchedule(strings.Join(fields[:5], " ")); err != nil {
			return fmt.Errorf("%q: %w", firstLines(trimmed, 1), err)
		}
		jobs++
		if jobs > cron.MaxJobs {
			return fmt.Errorf("more than %d jobs", cron.MaxJobs)
		}
	}
	return nil
}

// isEnvLine reports whether a crontab line sets a variable.
func isEnvLine(line string) bool {
	idx := strings.Index(line, "=")
	if idx <= 0 {
		return false
	}
	name := strings.TrimSpace(line[:idx])
	if name == "" || strings.ContainsAny(name, " \t") {
		return false
	}
	for _, c := range name {
		if !(c == '_' || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') ||
			(c >= '0' && c <= '9')) {
			return false
		}
	}
	return true
}

// CronResult is an account's crontab as the daemon has it.
type CronResult struct {
	Text string `json:"text"`
	// Allowed reports whether the account may use cron at all. A name in
	// /etc/cron.deny silently gets no jobs, which is a long afternoon.
	Allowed bool `json:"allowed"`
}

func registerCron(r *agent.Registry) {
	agent.Register(r, "cron.read", 1, func(ctx context.Context, in CronRequest) (CronResult, error) {
		return readCrontab(ctx, in.Owner)
	})

	agent.Register(r, "cron.write", 1, func(ctx context.Context, in CronWriteRequest) (CronResult, error) {
		if err := writeCrontab(ctx, in.Owner, in.Text); err != nil {
			return CronResult{}, err
		}
		return readCrontab(ctx, in.Owner)
	})
}

// readCrontab returns what the daemon currently has for an account.
func readCrontab(ctx context.Context, owner string) (CronResult, error) {
	acct, err := linuxuser.Lookup(owner)
	if err != nil || acct == nil {
		return CronResult{}, fmt.Errorf("account %q does not exist", owner)
	}

	res, err := run.Cmd(ctx, []string{"crontab", "-u", owner, "-l"},
		run.Timeout(30*time.Second))
	out := CronResult{Allowed: cronAllowed(owner)}
	if err != nil {
		// "no crontab for <user>" is not a failure; it is the normal state of
		// an account nobody has scheduled anything for.
		combined := res.Stdout + res.Stderr
		if strings.Contains(combined, "no crontab") {
			return out, nil
		}
		return out, fmt.Errorf("read crontab: %s", firstLines(combined, 2))
	}
	out.Text = res.Stdout
	return out, nil
}

// writeCrontab installs a complete crontab for an account.
func writeCrontab(ctx context.Context, owner, text string) error {
	acct, err := linuxuser.Lookup(owner)
	if err != nil || acct == nil {
		return fmt.Errorf("account %q does not exist", owner)
	}
	if !cronAllowed(owner) {
		return fmt.Errorf("%s is not allowed to use cron on this server", owner)
	}

	// An empty crontab is a removal, not a file with nothing in it: crontab
	// refuses some empty inputs and leaves the old file in place, which
	// would make "delete every job" silently do nothing.
	if strings.TrimSpace(stripComments(text)) == "" {
		res, err := run.Cmd(ctx, []string{"crontab", "-u", owner, "-r"},
			run.Timeout(30*time.Second))
		if err != nil && !strings.Contains(res.Stdout+res.Stderr, "no crontab") {
			return fmt.Errorf("clear crontab: %s", firstLines(res.Stdout+res.Stderr, 2))
		}
		return nil
	}

	// Through a file rather than standard input: "crontab -" is the
	// documented form but reads differently across implementations, and a
	// file is what the daemon's own validation is written against.
	tmp, err := os.CreateTemp("", "opanel-cron-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()

	if !strings.HasSuffix(text, "\n") {
		// A crontab whose last line has no newline is ignored by some
		// implementations, which loses exactly one job -- the last one.
		text += "\n"
	}
	if _, err := tmp.WriteString(text); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(name, 0o600); err != nil {
		return err
	}

	res, err := run.Cmd(ctx, []string{"crontab", "-u", owner, name},
		run.Timeout(30*time.Second))
	if err != nil {
		return fmt.Errorf("install crontab: %s", firstLines(res.Stdout+res.Stderr, 3))
	}
	return nil
}

// stripComments removes comment and environment lines, leaving the jobs.
func stripComments(text string) string {
	var b strings.Builder
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || isEnvLine(trimmed) {
			continue
		}
		b.WriteString(trimmed)
		b.WriteString("\n")
	}
	return b.String()
}

// cronAllowed reports whether the account may schedule anything.
//
// cron.allow, when it exists, is a whitelist and everybody else is refused;
// otherwise cron.deny is a blacklist. Getting this wrong means jobs that are
// accepted by the panel and never run.
func cronAllowed(owner string) bool {
	if names, err := readLines("/etc/cron.allow"); err == nil {
		return names[owner]
	}
	if names, err := readLines("/etc/cron.deny"); err == nil {
		return !names[owner]
	}
	return true
}

func readLines(path string) (map[string]bool, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, line := range strings.Split(string(body), "\n") {
		if name := strings.TrimSpace(line); name != "" && !strings.HasPrefix(name, "#") {
			out[name] = true
		}
	}
	return out, nil
}
