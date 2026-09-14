package actions

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bnixvn/opanel-ent/internal/agent"
	"github.com/bnixvn/opanel-ent/internal/firewall"
	"github.com/bnixvn/opanel-ent/internal/platform/run"
)

// Where the ruleset lives and what restores it.
//
// The same path the installer writes and that nftables.service loads at
// boot, so a rule added from the panel survives a restart. Pointing this
// somewhere else was the first attempt, and it meant the rollback had
// nothing to roll back to.
const (
	nftRulesetPath = "/etc/nftables/opanel.nft"
	nftLastGood    = "/etc/nftables/opanel-last-good.nft"
	rollbackUnit   = "opanel-fw-rollback"
)

// RollbackWindow is how long a change stays provisional. The caller has to
// come back and confirm; if the new rules locked them out, the confirmation
// never arrives and the previous ruleset is restored.
const RollbackWindow = 120 * time.Second

// FirewallApplyRequest carries a rendered ruleset.
type FirewallApplyRequest struct {
	Ruleset string `json:"ruleset"`
	// Confirm skips the rollback timer. Used by the boot-time reload, where
	// there is no operator to confirm and the ruleset is one that already
	// worked.
	Confirm bool `json:"confirm,omitempty"`
}

// Validate refuses an empty or obviously wrong script.
func (r *FirewallApplyRequest) Validate() error {
	if !strings.Contains(r.Ruleset, "table inet "+firewall.TableName) {
		return errors.New("that does not look like an OPanel ruleset")
	}
	if len(r.Ruleset) > 32<<20 {
		return errors.New("the ruleset is too large to load")
	}
	return nil
}

// FirewallStatus reports what the kernel is doing.
type FirewallStatus struct {
	Active bool `json:"active"`
	// Pending is true while a change is waiting to be confirmed.
	Pending    bool   `json:"pending"`
	RuleCount  int    `json:"rule_count"`
	BlockCount int    `json:"block_count"`
	Error      string `json:"error,omitempty"`
}

// FirewallFetchRequest downloads a blocklist.
type FirewallFetchRequest struct {
	URL string `json:"url"`
}

// Validate insists on http or https and nothing stranger.
func (r *FirewallFetchRequest) Validate() error {
	if !strings.HasPrefix(r.URL, "http://") && !strings.HasPrefix(r.URL, "https://") {
		return errors.New("a blocklist URL must start with http:// or https://")
	}
	return nil
}

// FirewallFetchResult is what a blocklist contained.
type FirewallFetchResult struct {
	Addresses []string `json:"addresses"`
	Skipped   int      `json:"skipped"`
}

func registerFirewall(r *agent.Registry) {
	agent.Register(r, "firewall.status", 1, func(ctx context.Context, _ struct{}) (FirewallStatus, error) {
		return firewallStatus(ctx), nil
	})

	agent.Register(r, "firewall.apply", 1, func(ctx context.Context, in FirewallApplyRequest) (FirewallStatus, error) {
		if err := applyRuleset(ctx, in.Ruleset, in.Confirm); err != nil {
			return FirewallStatus{}, err
		}
		return firewallStatus(ctx), nil
	})

	// Confirming cancels the rollback. The caller reaching this at all is
	// the evidence that the new rules did not cut them off.
	agent.Register(r, "firewall.confirm", 1, func(ctx context.Context, _ struct{}) (FirewallStatus, error) {
		cancelRollback(ctx)
		if body, err := os.ReadFile(nftRulesetPath); err == nil {
			_ = os.WriteFile(nftLastGood, body, 0o600)
		}
		return firewallStatus(ctx), nil
	})

	agent.RegisterSlow(r, "firewall.fetch", 1, 3*time.Minute,
		func(ctx context.Context, in FirewallFetchRequest) (FirewallFetchResult, error) {
			return fetchBlocklist(ctx, in.URL)
		})
}

// applyRuleset writes and loads a ruleset, arming a rollback first.
func applyRuleset(ctx context.Context, ruleset string, confirmed bool) error {
	if err := os.MkdirAll(filepath.Dir(nftRulesetPath), 0o750); err != nil {
		return err
	}
	// Keep whatever is currently loaded as the thing to go back to.
	if _, err := os.Stat(nftLastGood); errors.Is(err, os.ErrNotExist) {
		if body, err := os.ReadFile(nftRulesetPath); err == nil {
			_ = os.WriteFile(nftLastGood, body, 0o600)
		}
	}

	// Written to a temporary file and checked before it is loaded: nft
	// applies a file up to the first error, so a script that is wrong
	// halfway through leaves a half-built firewall.
	tmp := nftRulesetPath + ".new"
	if err := os.WriteFile(tmp, []byte(ruleset), 0o600); err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp) }()

	if _, err := run.Cmd(ctx, []string{"nft", "--check", "--file", tmp},
		run.Timeout(2*time.Minute)); err != nil {
		return fmt.Errorf("the ruleset was refused before loading: %w", err)
	}

	if !confirmed {
		armRollback(ctx)
	}

	if err := os.Rename(tmp, nftRulesetPath); err != nil {
		return err
	}
	if _, err := run.Cmd(ctx, []string{"nft", "--file", nftRulesetPath},
		run.Timeout(2*time.Minute)); err != nil {
		// Put the previous rules back at once rather than waiting for the
		// timer: the load failed, so nothing is provisional about it.
		restoreLastGood(ctx)
		cancelRollback(ctx)
		return fmt.Errorf("load ruleset: %w", err)
	}
	if confirmed {
		_ = os.WriteFile(nftLastGood, []byte(ruleset), 0o600)
	}
	return nil
}

// armRollback schedules the previous ruleset to be restored.
//
// A firewall change made over the network can cut off the connection that
// made it, and then there is nobody to undo it. The timer is the answer: the
// change reverts by itself unless somebody comes back to say it worked.
func armRollback(ctx context.Context) {
	cancelRollback(ctx)
	_, _ = run.Cmd(ctx, []string{
		"systemd-run", "--quiet",
		fmt.Sprintf("--on-active=%d", int(RollbackWindow.Seconds())),
		"--unit=" + rollbackUnit,
		"/usr/sbin/nft", "--file", nftLastGood,
	})
}

func cancelRollback(ctx context.Context) {
	_, _ = run.Cmd(ctx, []string{"systemctl", "stop", rollbackUnit + ".timer"})
	_, _ = run.Cmd(ctx, []string{"systemctl", "reset-failed", rollbackUnit + ".timer"})
	_, _ = run.Cmd(ctx, []string{"systemctl", "reset-failed", rollbackUnit + ".service"})
}

func restoreLastGood(ctx context.Context) {
	if _, err := os.Stat(nftLastGood); err != nil {
		return
	}
	_, _ = run.Cmd(ctx, []string{"nft", "--file", nftLastGood}, run.Timeout(time.Minute))
}

// firewallStatus asks the kernel rather than reading the file, because what
// is loaded and what was last written can differ.
func firewallStatus(ctx context.Context) FirewallStatus {
	var st FirewallStatus
	res, err := run.Cmd(ctx, []string{"nft", "list", "table", "inet", firewall.TableName},
		run.Timeout(time.Minute), run.AllowExit(1))
	if err != nil || res.ExitCode != 0 {
		st.Error = "the panel's firewall table is not loaded"
		return st
	}
	st.Active = true
	for _, line := range strings.Split(res.Stdout, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.Contains(trimmed, "dport") && strings.Contains(trimmed, "accept") {
			st.RuleCount++
		}
	}
	// BlockCount is deliberately left at zero: counting set members would
	// mean parsing output meant for humans, and the database already knows
	// exactly how many addresses it asked for.

	if out, err := run.Cmd(ctx, []string{"systemctl", "is-active", rollbackUnit + ".timer"},
		run.AllowExit(1, 3)); err == nil {
		st.Pending = strings.TrimSpace(out.Stdout) == "active"
	}
	return st
}

// fetchBlocklist downloads a list of addresses.
func fetchBlocklist(ctx context.Context, url string) (FirewallFetchResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return FirewallFetchResult{}, err
	}
	client := &http.Client{Timeout: 2 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return FirewallFetchResult{}, fmt.Errorf("fetch %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return FirewallFetchResult{}, fmt.Errorf("fetch %s: the server answered %s", url, resp.Status)
	}

	// Capped while reading: a URL that streams for ever must not fill the
	// disk or the agent's memory.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return FirewallFetchResult{}, err
	}

	var out FirewallFetchResult
	for _, raw := range strings.Split(string(body), "\n") {
		line := strings.TrimSpace(raw)
		if i := strings.IndexAny(line, "#;"); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		if line == "" {
			continue
		}
		// Some feeds put the address first and a reason after a space.
		if i := strings.IndexAny(line, " \t"); i > 0 {
			line = line[:i]
		}
		if !parsableAddress(line) {
			out.Skipped++
			continue
		}
		out.Addresses = append(out.Addresses, line)
		if len(out.Addresses) >= firewall.MaxBlocklistEntries {
			out.Skipped += 1
			break
		}
	}
	return out, nil
}

func parsableAddress(s string) bool {
	if strings.Contains(s, "/") {
		_, err := netip.ParsePrefix(s)
		return err == nil
	}
	_, err := netip.ParseAddr(s)
	return err == nil
}
