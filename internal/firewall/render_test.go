package firewall

import (
	"strings"
	"testing"

	"github.com/bnixvn/opanel-ent/internal/db"
)

func rule(kind, addr string, port int) *db.FirewallRule {
	return &db.FirewallRule{
		Kind: kind, Address: addr, PortFrom: port,
		Protocol: "tcp", Enabled: true, Comment: "test",
	}
}

// The property that matters most: whatever the rules say, the ports an
// operator needs to get back in are open. Everything else in the firewall
// can be wrong and be fixed; this one cannot.
func TestRenderAlwaysKeepsTheWayIn(t *testing.T) {
	out, err := Render(Ruleset{SSHPorts: []int{22, 2201}, PanelPort: 2222})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"tcp dport 22 accept", "tcp dport 2201 accept", "tcp dport 2222 accept",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the rendered ruleset is missing %q", want)
		}
	}
	if !strings.Contains(out, "policy drop") {
		t.Error("the input chain does not default to drop")
	}
}

func TestRenderPorts(t *testing.T) {
	both := rule(db.FirewallPort, "", 8080)
	both.Protocol = "both"

	ranged := rule(db.FirewallPort, "", 6000)
	ranged.PortTo = 6010

	scoped := rule(db.FirewallPort, "203.0.113.0/24", 5432)

	disabled := rule(db.FirewallPort, "", 9999)
	disabled.Enabled = false

	out, err := Render(Ruleset{Rules: []*db.FirewallRule{both, ranged, scoped, disabled}})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"tcp dport 8080 accept",
		"udp dport 8080 accept",
		"tcp dport 6000-6010 accept",
		"ip saddr 203.0.113.0/24 tcp dport 5432 accept",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "9999") {
		t.Error("a disabled rule was rendered")
	}
}

// Overlapping ranges are the ordinary case once a blocklist is subscribed to,
// and nftables refuses a set that holds them unless auto-merge is set. Without
// this the whole ruleset fails to load, which means no firewall change at all.
func TestRenderSetsAutoMerge(t *testing.T) {
	out, err := Render(Ruleset{
		Rules:     []*db.FirewallRule{rule(db.FirewallBlock, "203.0.113.0/24", 0)},
		Blocklist: []string{"203.0.113.9", "198.51.100.1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(out, "auto-merge") != 4 {
		t.Errorf("expected auto-merge on all four sets, got %d", strings.Count(out, "auto-merge"))
	}
	if !strings.Contains(out, "203.0.113.9") || !strings.Contains(out, "198.51.100.1") {
		t.Error("blocklist addresses did not reach the set")
	}
}

func TestRenderSplitsByFamily(t *testing.T) {
	out, err := Render(Ruleset{
		Rules: []*db.FirewallRule{
			rule(db.FirewallBlock, "203.0.113.1", 0),
			rule(db.FirewallBlock, "2001:db8::1", 0),
			rule(db.FirewallAllow, "198.51.100.5", 0),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	four := out[strings.Index(out, "set blocklist4"):strings.Index(out, "set blocklist6")]
	if !strings.Contains(four, "203.0.113.1") || strings.Contains(four, "2001:db8") {
		t.Error("the v4 blocklist holds the wrong addresses")
	}
	if !strings.Contains(out, "set allowlist4") || !strings.Contains(out, "198.51.100.5") {
		t.Error("the allow rule did not reach the allowlist")
	}
	// Order matters: an address an operator trusts must be accepted before
	// a subscribed blocklist gets a chance to drop it.
	allow := strings.Index(out, "saddr @allowlist4 accept")
	block := strings.Index(out, "saddr @blocklist4 drop")
	if allow < 0 || block < 0 || allow > block {
		t.Error("the allowlist is not consulted before the blocklist")
	}
}

// A comment reaches the ruleset inside quotes, so it must not be able to
// close them.
func TestRenderSanitisesComments(t *testing.T) {
	r := rule(db.FirewallPort, "", 8080)
	r.Comment = `evil" ; drop table x; comment "`
	out, err := Render(Ruleset{Rules: []*db.FirewallRule{r}})
	if err != nil {
		t.Fatal(err)
	}
	line := ""
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "dport 8080") {
			line = l
		}
	}
	if strings.Count(line, `"`) != 2 {
		t.Errorf("the comment broke out of its quotes: %s", line)
	}
}

func TestRenderRefusesBadInput(t *testing.T) {
	if _, err := Render(Ruleset{
		Rules: []*db.FirewallRule{rule(db.FirewallBlock, "not-an-address", 0)},
	}); err == nil {
		t.Error("an unparsable address was accepted")
	}

	r := rule(db.FirewallPort, "", 80)
	r.Protocol = "sctp"
	if _, err := Render(Ruleset{Rules: []*db.FirewallRule{r}}); err == nil {
		t.Error("an unknown protocol was accepted")
	}

	// A feed with more entries than the kernel should hold is refused
	// outright rather than loaded slowly.
	huge := make([]string, MaxBlocklistEntries+1)
	for i := range huge {
		huge[i] = "198.51.100.1"
	}
	if _, err := Render(Ruleset{Blocklist: huge}); err == nil {
		t.Error("an oversized blocklist was accepted")
	}
}

// One bad line in a downloaded feed must not cost the server its firewall.
func TestRenderSkipsRubbishInAFeed(t *testing.T) {
	out, err := Render(Ruleset{
		Blocklist: []string{"198.51.100.1", "well this is not an address", "203.0.113.0/24"},
	})
	if err != nil {
		t.Fatalf("one bad line failed the whole render: %v", err)
	}
	if !strings.Contains(out, "198.51.100.1") || !strings.Contains(out, "203.0.113.0/24") {
		t.Error("the good addresses were lost with the bad one")
	}
}
