// Package cron validates and renders a customer's crontab.
//
// The panel owns the whole file rather than a block inside it, the same way
// it owns the webserver's configuration: rows in the database are the truth
// and the crontab is a projection of them. Anything already there when the
// panel first looks is imported into rows, so nothing is lost by taking
// ownership.
//
// Nothing here touches the disk, so the rules are testable on their own.
package cron

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// MaxCommandLength bounds one command. Far past anything reasonable; it
// exists so a paste accident cannot produce a crontab the daemon refuses.
const MaxCommandLength = 1000

// MaxJobs caps how many jobs one account may have. A customer with two
// hundred per-minute jobs is a customer whose neighbours notice.
const MaxJobs = 100

// Job is one scheduled command.
type Job struct {
	ID       int64  `json:"id"`
	Schedule string `json:"schedule"`
	Command  string `json:"command"`
	Comment  string `json:"comment"`
	Enabled  bool   `json:"enabled"`
}

// Presets are the schedules people actually want, so the common case is a
// dropdown rather than five fields nobody remembers the order of.
var Presets = []struct {
	Label    string `json:"label"`
	Schedule string `json:"schedule"`
}{
	{"Every minute", "* * * * *"},
	{"Every 5 minutes", "*/5 * * * *"},
	{"Every 15 minutes", "*/15 * * * *"},
	{"Every 30 minutes", "*/30 * * * *"},
	{"Hourly", "0 * * * *"},
	{"Twice a day", "0 0,12 * * *"},
	{"Daily at midnight", "0 0 * * *"},
	{"Daily at 3am", "0 3 * * *"},
	{"Weekly on Sunday", "0 0 * * 0"},
	{"Monthly", "0 0 1 * *"},
}

// fieldRange is the acceptable numbers for one position.
type fieldRange struct {
	name     string
	min, max int
	// names maps the word forms cron accepts, such as "sun" or "jan".
	names map[string]int
}

var fields = []fieldRange{
	{name: "minute", min: 0, max: 59},
	{name: "hour", min: 0, max: 23},
	{name: "day of month", min: 1, max: 31},
	{name: "month", min: 1, max: 12, names: map[string]int{
		"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6,
		"jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12,
	}},
	// 7 as well as 0 for Sunday: cron accepts both and refusing one would be
	// rejecting a schedule that works.
	{name: "day of week", min: 0, max: 7, names: map[string]int{
		"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6,
	}},
}

// ValidateSchedule checks a five-field cron expression.
func ValidateSchedule(spec string) error {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return fmt.Errorf("a schedule is required")
	}
	// @reboot and friends are refused rather than translated. @reboot in
	// particular runs as the machine comes up, which is when a customer's
	// runaway script is hardest to stop.
	if strings.HasPrefix(spec, "@") {
		return fmt.Errorf("%q is not supported; use the five-field form, "+
			"for example \"0 3 * * *\"", spec)
	}
	parts := strings.Fields(spec)
	if len(parts) != 5 {
		return fmt.Errorf("a schedule has five fields (minute hour day month weekday), got %d", len(parts))
	}
	for i, part := range parts {
		if err := validField(part, fields[i]); err != nil {
			return fmt.Errorf("%s: %w", fields[i].name, err)
		}
	}
	return nil
}

// validField checks one position, including lists, ranges and steps.
func validField(part string, f fieldRange) error {
	for _, item := range strings.Split(part, ",") {
		if item == "" {
			return fmt.Errorf("empty value in %q", part)
		}
		body, step := item, ""
		if idx := strings.Index(item, "/"); idx >= 0 {
			body, step = item[:idx], item[idx+1:]
			n, err := strconv.Atoi(step)
			if err != nil || n < 1 || n > f.max {
				return fmt.Errorf("%q is not a usable step", step)
			}
		}
		if body == "*" {
			continue
		}
		lo, hi := body, ""
		if idx := strings.Index(body, "-"); idx >= 0 {
			lo, hi = body[:idx], body[idx+1:]
		}
		loN, err := fieldValue(lo, f)
		if err != nil {
			return err
		}
		if hi == "" {
			continue
		}
		hiN, err := fieldValue(hi, f)
		if err != nil {
			return err
		}
		if hiN < loN {
			return fmt.Errorf("%q runs backwards", body)
		}
	}
	return nil
}

func fieldValue(s string, f fieldRange) (int, error) {
	if f.names != nil {
		if n, ok := f.names[strings.ToLower(s)]; ok {
			return n, nil
		}
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("%q is not a number between %d and %d", s, f.min, f.max)
	}
	if n < f.min || n > f.max {
		return 0, fmt.Errorf("%d is outside %d-%d", n, f.min, f.max)
	}
	return n, nil
}

// ValidateCommand checks what will be run.
//
// The command reaches cron as a line in a file, so a newline in it would be
// a second job nobody asked for. A percent sign is the subtler one: cron
// turns an unescaped % into a newline and feeds the rest to the command on
// standard input, which is why a date format like +%Y in a crontab silently
// stops working. Escaping it here means what somebody typed is what runs.
func ValidateCommand(cmd string) error {
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return fmt.Errorf("a command is required")
	}
	if len(cmd) > MaxCommandLength {
		return fmt.Errorf("the command is longer than %d characters", MaxCommandLength)
	}
	if strings.ContainsAny(cmd, "\r\n") {
		return fmt.Errorf("a command cannot contain a line break")
	}
	if strings.ContainsRune(cmd, 0) {
		return fmt.Errorf("a command cannot contain a null byte")
	}
	return nil
}

// escapePercent makes a literal percent survive cron.
func escapePercent(cmd string) string {
	var b strings.Builder
	for i := 0; i < len(cmd); i++ {
		if cmd[i] == '%' {
			// Already escaped by whoever typed it: leave one backslash, not two.
			if i > 0 && cmd[i-1] == '\\' {
				b.WriteByte('%')
				continue
			}
			b.WriteString("\\%")
			continue
		}
		b.WriteByte(cmd[i])
	}
	return b.String()
}

// Validate checks a whole job.
func Validate(j *Job) error {
	if err := ValidateSchedule(j.Schedule); err != nil {
		return err
	}
	if err := ValidateCommand(j.Command); err != nil {
		return err
	}
	if strings.ContainsAny(j.Comment, "\r\n") {
		return fmt.Errorf("a note cannot contain a line break")
	}
	if len(j.Comment) > 200 {
		return fmt.Errorf("the note is too long")
	}
	return nil
}

// Render builds the crontab for one account.
//
// MAILTO is emptied because most hosting servers have no mail transport, and
// cron that cannot deliver output writes it to the system log on every run --
// a per-minute job then fills the disk with delivery failures. Output that
// matters should be redirected to a file the customer can read.
func Render(jobs []Job) string {
	var b strings.Builder
	b.WriteString("# Managed by OPanel. Changes made here are replaced.\n")
	b.WriteString("# Edit these under Cron in the panel.\n")
	b.WriteString("#\n")
	b.WriteString("# Output is not mailed: this server has no mail transport, and cron\n")
	b.WriteString("# that cannot deliver logs a failure on every run. Redirect what you\n")
	b.WriteString("# want to keep, for example: >> $HOME/logs/cron.log 2>&1\n")
	b.WriteString("MAILTO=\"\"\n")
	b.WriteString("PATH=/usr/local/bin:/usr/bin:/bin\n\n")

	// Sorted by id so the same rows always produce the same file, which is
	// what makes "did anything change" a byte comparison.
	ordered := make([]Job, len(jobs))
	copy(ordered, jobs)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].ID < ordered[j].ID })

	for _, j := range ordered {
		if j.Comment != "" {
			b.WriteString("# " + strings.TrimSpace(j.Comment) + "\n")
		}
		if !j.Enabled {
			// Kept as a comment rather than dropped, so the file explains
			// itself to anyone reading it over SSH.
			b.WriteString("# disabled: ")
		}
		b.WriteString(normalizeSchedule(j.Schedule))
		b.WriteString(" ")
		b.WriteString(escapePercent(strings.TrimSpace(j.Command)))
		b.WriteString("\n")
	}
	return b.String()
}

// normalizeSchedule collapses whitespace so the rendered file is tidy.
func normalizeSchedule(spec string) string {
	return strings.Join(strings.Fields(spec), " ")
}

// Parse reads an existing crontab into jobs.
//
// Used once, when the panel first takes over an account's crontab: anything
// a customer put there over SSH becomes a row rather than being thrown away
// by the first render.
func Parse(text string) []Job {
	var out []Job
	var pending string

	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			pending = ""
			continue
		}
		if strings.HasPrefix(trimmed, "#") {
			body := strings.TrimSpace(strings.TrimPrefix(trimmed, "#"))
			// The panel's own header is not a customer's note.
			if strings.HasPrefix(body, "Managed by OPanel") ||
				strings.HasPrefix(body, "Edit these under") ||
				strings.HasPrefix(body, "Output is not mailed") ||
				strings.HasPrefix(body, "that cannot deliver") ||
				strings.HasPrefix(body, "want to keep") {
				continue
			}
			if rest, ok := strings.CutPrefix(body, "disabled: "); ok {
				if j, ok := parseLine(rest); ok {
					j.Enabled = false
					j.Comment = pending
					out = append(out, j)
					pending = ""
				}
				continue
			}
			pending = body
			continue
		}
		// An environment assignment, not a job.
		if !strings.HasPrefix(trimmed, "@") && isAssignment(trimmed) {
			pending = ""
			continue
		}
		if j, ok := parseLine(trimmed); ok {
			j.Enabled = true
			j.Comment = pending
			out = append(out, j)
		}
		pending = ""
	}
	return out
}

// isAssignment reports whether a line sets a variable rather than a job.
func isAssignment(line string) bool {
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

// parseLine splits a crontab line into a schedule and a command.
func parseLine(line string) (Job, bool) {
	parts := strings.Fields(line)
	if len(parts) < 6 {
		return Job{}, false
	}
	schedule := strings.Join(parts[:5], " ")
	if ValidateSchedule(schedule) != nil {
		return Job{}, false
	}
	// Rebuilt from the original text rather than from the split fields, so
	// runs of spaces inside the command survive.
	idx := indexAfterFields(line, 5)
	if idx < 0 {
		return Job{}, false
	}
	command := strings.TrimSpace(line[idx:])
	// Undo what Render did, so a round trip through the file does not
	// accumulate backslashes.
	command = strings.ReplaceAll(command, "\\%", "%")
	if ValidateCommand(command) != nil {
		return Job{}, false
	}
	return Job{Schedule: schedule, Command: command}, true
}

// indexAfterFields returns the offset just past the n'th whitespace-separated
// field.
func indexAfterFields(s string, n int) int {
	seen, inField := 0, false
	for i := 0; i < len(s); i++ {
		isSpace := s[i] == ' ' || s[i] == '\t'
		switch {
		case !isSpace && !inField:
			inField = true
		case isSpace && inField:
			inField = false
			seen++
			if seen == n {
				return i
			}
		}
	}
	return -1
}
