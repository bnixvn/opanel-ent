// Package security drives the firewall and the web application firewall.
//
// Separate from internal/firewall, which renders the nftables ruleset and
// nothing else: the agent needs the renderer, and this package needs the
// agent, so keeping them apart is what stops the two importing each other.
package security

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"strings"
	"time"

	"github.com/bnixvn/opanel-ent/internal/agent/actions"
	"github.com/bnixvn/opanel-ent/internal/agentclient"
	"github.com/bnixvn/opanel-ent/internal/db"
	"github.com/bnixvn/opanel-ent/internal/firewall"
)

// ProtectedPorts are never closed by a rule.
//
// SSH and the panel's own port are how an operator gets back in. 80 and 443
// are what the machine exists to serve, and closing them takes every
// customer's website down while the panel carries on answering on its own
// port and looking healthy -- the worst kind of outage to diagnose. Port 80
// is also how certificate renewal is validated, so closing it breaks TLS
// weeks later rather than immediately.
var ProtectedPorts = map[int]string{
	22: "SSH", 2222: "the panel", 80: "websites", 443: "websites",
}

// Firewall manages the packet filter.
type Firewall struct {
	db        *db.DB
	agent     *agentclient.Client
	log       *slog.Logger
	panelPort int
	sshPorts  []int
}

// New builds the service.
func NewFirewall(database *db.DB, ac *agentclient.Client, panelPort int, log *slog.Logger) *Firewall {
	if log == nil {
		log = slog.Default()
	}
	return &Firewall{
		db: database, agent: ac, log: log,
		panelPort: panelPort,
		// The installer reads these from sshd_config; here the well-known
		// port is enough, because the rendered ruleset always adds whatever
		// the panel is listening on too.
		sshPorts: []int{22},
	}
}

// Status reports what the kernel has loaded plus what the database holds.
func (s *Firewall) Status(ctx context.Context) (map[string]any, error) {
	st, err := agentclient.Call[actions.FirewallStatus](ctx, s.agent, "firewall.status", 1, struct{}{})
	if err != nil {
		return nil, err
	}
	blocked, err := s.db.BlocklistAddresses(ctx)
	if err != nil {
		return nil, err
	}
	// The ports the renderer always emits, reported so the interface can
	// show them as open and explain why they have no delete button, rather
	// than hiding the fact that they are open at all.
	protected := make([]map[string]any, 0, len(s.sshPorts)+1)
	for _, p := range s.sshPorts {
		protected = append(protected, map[string]any{
			"port": p, "protocol": "tcp", "reason": "SSH",
		})
	}
	if s.panelPort > 0 {
		protected = append(protected, map[string]any{
			"port": s.panelPort, "protocol": "tcp", "reason": "the panel",
		})
	}
	for _, p := range firewall.DefaultWebPorts {
		protected = append(protected, map[string]any{
			"port": p, "protocol": "tcp", "reason": "websites",
		})
	}

	return map[string]any{
		"active":          st.Active,
		"pending_confirm": st.Pending,
		"loaded_rules":    st.RuleCount,
		"error":           st.Error,
		"blocklist_size":  len(blocked),
		"protected_ports": protected,
	}, nil
}

// Apply renders the current rules and loads them, provisionally.
//
// Provisional on purpose: the caller has to Confirm afterwards. Reaching the
// confirmation is proof the new rules did not cut off the connection that
// asked for them.
func (s *Firewall) Apply(ctx context.Context, confirmed bool) error {
	rules, err := s.db.ListFirewallRules(ctx)
	if err != nil {
		return err
	}
	blocked, err := s.db.BlocklistAddresses(ctx)
	if err != nil {
		return err
	}
	script, err := firewall.Render(firewall.Ruleset{
		Rules: rules, Blocklist: blocked,
		SSHPorts: s.sshPorts, PanelPort: s.panelPort,
		WebPorts: firewall.DefaultWebPorts,
	})
	if err != nil {
		return err
	}
	_, err = agentclient.Call[actions.FirewallStatus](ctx, s.agent, "firewall.apply", 1,
		actions.FirewallApplyRequest{Ruleset: script, Confirm: confirmed})
	return err
}

// Confirm keeps a provisional change.
func (s *Firewall) Confirm(ctx context.Context) error {
	_, err := agentclient.Call[actions.FirewallStatus](ctx, s.agent, "firewall.confirm", 1, struct{}{})
	return err
}

// AddRule validates and stores a rule, then applies the set.
func (s *Firewall) AddRule(ctx context.Context, r *db.FirewallRule) (*db.FirewallRule, error) {
	if err := validate(r); err != nil {
		return nil, err
	}
	// A rule for a port the renderer always opens would sit in the list
	// looking meaningful and be impossible to distinguish from the reason
	// the port is actually open. Worse, deleting it later would look like it
	// closed the port when nothing changed.
	if r.Kind == db.FirewallPort && r.Address == "" && r.PortTo == 0 {
		if name, always := ProtectedPorts[r.PortFrom]; always {
			return nil, fmt.Errorf("port %d is already open for %s", r.PortFrom, name)
		}
	}
	created, err := s.db.CreateFirewallRule(ctx, r)
	if err != nil {
		return nil, err
	}
	if err := s.Apply(ctx, false); err != nil {
		// The row is removed again: a rule in the database that the kernel
		// refused would be applied silently at the next unrelated change.
		_ = s.db.DeleteFirewallRule(ctx, created.ID)
		return nil, err
	}
	return created, nil
}

// DeleteRule removes a rule and reapplies.
//
// There is no protected-port check here, and there must not be. A protected
// port is opened by the renderer on every apply, not by a row, so deleting a
// row that also opens it closes nothing -- while refusing the delete would
// leave a rule that can never be removed and a list nobody can tidy. What
// protects those ports is that no rule is needed to open them; AddRule
// refuses to create one for exactly that reason.
func (s *Firewall) DeleteRule(ctx context.Context, id int64) error {
	if _, err := s.db.FirewallRuleByID(ctx, id); err != nil {
		return err
	}
	if err := s.db.DeleteFirewallRule(ctx, id); err != nil {
		return err
	}
	return s.Apply(ctx, false)
}

// SetEnabled turns a rule on or off and reapplies.
func (s *Firewall) SetEnabled(ctx context.Context, id int64, enabled bool) error {
	if err := s.db.SetFirewallRuleEnabled(ctx, id, enabled); err != nil {
		return err
	}
	return s.Apply(ctx, false)
}

// RefreshSource downloads a blocklist and stores what it contained.
func (s *Firewall) RefreshSource(ctx context.Context, id int64) (int, error) {
	src, err := s.db.FirewallSourceByID(ctx, id)
	if err != nil {
		return 0, err
	}
	res, err := agentclient.Call[actions.FirewallFetchResult](ctx, s.agent, "firewall.fetch", 1,
		actions.FirewallFetchRequest{URL: src.URL})
	if err != nil {
		// Recorded rather than swallowed: a feed that has been failing for
		// a week is something an operator needs to see.
		_ = s.db.ReplaceSourceEntries(ctx, id, nil, err.Error())
		return 0, err
	}
	if err := s.db.ReplaceSourceEntries(ctx, id, res.Addresses, ""); err != nil {
		return 0, err
	}
	s.log.Info("firewall: blocklist refreshed",
		"url", src.URL, "addresses", len(res.Addresses), "skipped", res.Skipped)
	return len(res.Addresses), s.Apply(ctx, true)
}

// RefreshDue re-fetches the sources whose interval has passed.
func (s *Firewall) RefreshDue(ctx context.Context) (int, []error) {
	sources, err := s.db.ListFirewallSources(ctx)
	if err != nil {
		return 0, []error{err}
	}
	var errs []error
	done := 0
	for _, src := range sources {
		if !src.Enabled {
			continue
		}
		every := time.Duration(src.IntervalHours) * time.Hour
		if every <= 0 {
			every = 24 * time.Hour
		}
		if !src.LastFetchAt.IsZero() && time.Since(src.LastFetchAt) < every {
			continue
		}
		if _, err := s.RefreshSource(ctx, src.ID); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", src.URL, err))
			continue
		}
		done++
	}
	return done, errs
}

// validate checks a rule before it reaches the renderer.
func validate(r *db.FirewallRule) error {
	switch r.Kind {
	case db.FirewallPort:
		if r.PortFrom < 1 || r.PortFrom > 65535 {
			return errors.New("a port must be between 1 and 65535")
		}
		if r.PortTo != 0 && (r.PortTo < r.PortFrom || r.PortTo > 65535) {
			return errors.New("the end of a port range must be above its start")
		}
		switch r.Protocol {
		case "tcp", "udp", "both", "":
		default:
			return errors.New("protocol must be tcp, udp or both")
		}
		if r.Address != "" {
			if _, err := firewall.AddressFamily(r.Address); err != nil {
				return err
			}
		}
	case db.FirewallBlock, db.FirewallAllow:
		if strings.TrimSpace(r.Address) == "" {
			return errors.New("an address or range is required")
		}
		if _, err := firewall.AddressFamily(r.Address); err != nil {
			return err
		}
		// Blocking a whole /0 would drop every packet including the one
		// carrying the request that asked for it.
		if p, err := netip.ParsePrefix(r.Address); err == nil && p.Bits() == 0 {
			return errors.New("that range covers the whole internet; the panel will not add it")
		}
	default:
		return fmt.Errorf("kind must be %q, %q or %q",
			db.FirewallPort, db.FirewallBlock, db.FirewallAllow)
	}
	return nil
}
