// Package firewall renders the nftables ruleset from the panel's rules.
//
// The database is the truth and the kernel is a projection of it: the whole
// table is rebuilt on every change rather than patched. Parsing `nft list`
// back into rules, which the previous panel did, was where its firewall bugs
// came from -- output meant for humans is not a data format.
package firewall

import (
	"fmt"
	"net/netip"
	"sort"
	"strings"

	"github.com/bnixvn/opanel-ent/internal/db"
)

// TableName is the one table the panel owns. Everything else on the host is
// left alone, so an operator with their own rules keeps them.
const TableName = "opanel"

// MaxBlocklistEntries caps what a fetched source may contribute. A list with
// ten million lines would take minutes to load and megabytes of memory in
// the kernel; refusing is better than a firewall that takes the server down
// while it thinks.
const MaxBlocklistEntries = 200000

// Ruleset is everything needed to render.
type Ruleset struct {
	Rules []*db.FirewallRule
	// Blocklist holds addresses from fetched sources, separate from the
	// block rules an operator typed so the two can be counted apart.
	Blocklist []string
	// SSHPorts and PanelPort are always open. They are added here rather
	// than stored as rules so that deleting every rule cannot lock the
	// operator out of the machine.
	SSHPorts  []int
	PanelPort int
	// WebPorts are always open for the same reason, one level up: this is a
	// hosting panel, and a firewall that can close 80 and 443 is a firewall
	// that will one day take every customer's website offline -- silently,
	// because the panel answers on its own port and still looks healthy.
	// Certificate renewal goes with them: HTTP-01 validation is fetched over
	// port 80 from outside, so closing it breaks TLS weeks later.
	WebPorts []int
}

// DefaultWebPorts is what a hosting host has to answer on.
var DefaultWebPorts = []int{80, 443}

// Render produces an nft script.
func Render(rs Ruleset) (string, error) {
	var b strings.Builder

	block4, block6, allow4, allow6, err := partition(rs)
	if err != nil {
		return "", err
	}

	b.WriteString("#!/usr/sbin/nft -f\n")
	b.WriteString("# Managed by OPanel. Rendered from the panel database;\n")
	b.WriteString("# edits here are lost on the next change.\n\n")
	fmt.Fprintf(&b, "table inet %s\n", TableName)
	fmt.Fprintf(&b, "delete table inet %s\n\n", TableName)
	fmt.Fprintf(&b, "table inet %s {\n", TableName)

	writeSet(&b, "blocklist4", "ipv4_addr", block4)
	writeSet(&b, "blocklist6", "ipv6_addr", block6)
	writeSet(&b, "allowlist4", "ipv4_addr", allow4)
	writeSet(&b, "allowlist6", "ipv6_addr", allow6)

	b.WriteString("\n    chain input {\n")
	b.WriteString("        type filter hook input priority filter; policy drop;\n\n")
	b.WriteString("        ct state established,related accept\n")
	b.WriteString("        ct state invalid drop\n")
	b.WriteString("        iif lo accept\n\n")

	// The allowlist is consulted first so an address an operator trusts is
	// not caught by a blocklist they subscribed to.
	b.WriteString("        ip  saddr @allowlist4 accept\n")
	b.WriteString("        ip6 saddr @allowlist6 accept\n")
	b.WriteString("        ip  saddr @blocklist4 drop\n")
	b.WriteString("        ip6 saddr @blocklist6 drop\n\n")

	b.WriteString("        ip  protocol icmp      accept\n")
	b.WriteString("        ip6 nexthdr  ipv6-icmp accept\n\n")

	// Ports that must never close, whatever the rules say.
	b.WriteString("        # Always open: closing these would lock the operator out.\n")
	for _, p := range rs.SSHPorts {
		fmt.Fprintf(&b, "        tcp dport %d accept comment \"ssh\"\n", p)
	}
	if rs.PanelPort > 0 {
		fmt.Fprintf(&b, "        tcp dport %d accept comment \"panel\"\n", rs.PanelPort)
	}
	for _, p := range rs.WebPorts {
		fmt.Fprintf(&b, "        tcp dport %d accept comment \"web\"\n", p)
	}

	b.WriteString("\n")
	for _, r := range rs.Rules {
		if !r.Enabled || r.Kind != db.FirewallPort {
			continue
		}
		line, err := portRule(r)
		if err != nil {
			return "", err
		}
		b.WriteString(line)
	}

	b.WriteString("    }\n\n")
	b.WriteString("    chain forward { type filter hook forward priority filter; policy drop; }\n")
	b.WriteString("    chain output  { type filter hook output  priority filter; policy accept; }\n")
	b.WriteString("}\n")
	return b.String(), nil
}

// portRule renders one accept.
func portRule(r *db.FirewallRule) (string, error) {
	ports := fmt.Sprintf("%d", r.PortFrom)
	if r.PortTo > r.PortFrom {
		ports = fmt.Sprintf("%d-%d", r.PortFrom, r.PortTo)
	}
	comment := sanitiseComment(r.Comment)

	var protocols []string
	switch r.Protocol {
	case "tcp", "":
		protocols = []string{"tcp"}
	case "udp":
		protocols = []string{"udp"}
	case "both":
		protocols = []string{"tcp", "udp"}
	default:
		return "", fmt.Errorf("firewall: protocol %q is not tcp, udp or both", r.Protocol)
	}

	var b strings.Builder
	for _, proto := range protocols {
		prefix := ""
		if r.Address != "" {
			family, err := addressFamily(r.Address)
			if err != nil {
				return "", err
			}
			prefix = fmt.Sprintf("%s saddr %s ", family, r.Address)
		}
		fmt.Fprintf(&b, "        %s%s dport %s accept comment \"%s\"\n",
			prefix, proto, ports, comment)
	}
	return b.String(), nil
}

// partition splits addresses by family and by whether they block or allow.
func partition(rs Ruleset) (block4, block6, allow4, allow6 []string, err error) {
	add := func(dst4, dst6 *[]string, addr string) error {
		family, err := addressFamily(addr)
		if err != nil {
			return err
		}
		if family == "ip" {
			*dst4 = append(*dst4, addr)
		} else {
			*dst6 = append(*dst6, addr)
		}
		return nil
	}

	for _, r := range rs.Rules {
		if !r.Enabled || r.Address == "" {
			continue
		}
		switch r.Kind {
		case db.FirewallBlock:
			if err := add(&block4, &block6, r.Address); err != nil {
				return nil, nil, nil, nil, err
			}
		case db.FirewallAllow:
			if err := add(&allow4, &allow6, r.Address); err != nil {
				return nil, nil, nil, nil, err
			}
		}
	}

	if len(rs.Blocklist) > MaxBlocklistEntries {
		return nil, nil, nil, nil, fmt.Errorf(
			"firewall: %d blocked addresses is more than the %d this panel will load",
			len(rs.Blocklist), MaxBlocklistEntries)
	}
	for _, a := range rs.Blocklist {
		// A malformed line in a downloaded list is skipped rather than
		// failing the whole ruleset: one bad entry in a feed of thousands
		// must not leave the server with no firewall at all.
		if err := add(&block4, &block6, a); err != nil {
			continue
		}
	}

	return dedupe(block4), dedupe(block6), dedupe(allow4), dedupe(allow6), nil
}

func dedupe(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(in))
	out := in[:0]
	for _, v := range in {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}

// writeSet emits a named set, with its elements when it has any.
func writeSet(b *strings.Builder, name, typ string, elements []string) {
	// auto-merge is not optional here. Blocklists overlap each other and
	// overlap the ranges an operator typed -- 203.0.113.9 inside
	// 203.0.113.0/24 is the ordinary case, not a mistake -- and without it
	// nftables refuses the whole set with "conflicting intervals", which
	// means one overlapping entry anywhere leaves the server with no
	// firewall change at all.
	fmt.Fprintf(b, "    set %s {\n        type %s\n        flags interval\n        auto-merge\n",
		name, typ)
	if len(elements) > 0 {
		b.WriteString("        elements = {\n")
		// Wrapped rather than one line: a set of 200,000 entries on a single
		// line is a file no operator can read and some editors cannot open.
		for i := 0; i < len(elements); i += 8 {
			end := min(i+8, len(elements))
			fmt.Fprintf(b, "            %s,\n", strings.Join(elements[i:end], ", "))
		}
		b.WriteString("        }\n")
	}
	b.WriteString("    }\n")
}

// AddressFamily reports whether an address or CIDR is v4 or v6, as the nft
// keyword for it.
func AddressFamily(addr string) (string, error) { return addressFamily(addr) }

func addressFamily(addr string) (string, error) {
	addr = strings.TrimSpace(addr)
	if strings.Contains(addr, "/") {
		p, err := netip.ParsePrefix(addr)
		if err != nil {
			return "", fmt.Errorf("firewall: %q is not an address range", addr)
		}
		if p.Addr().Is4() {
			return "ip", nil
		}
		return "ip6", nil
	}
	a, err := netip.ParseAddr(addr)
	if err != nil {
		return "", fmt.Errorf("firewall: %q is not an address", addr)
	}
	if a.Is4() {
		return "ip", nil
	}
	return "ip6", nil
}

// sanitiseComment keeps a comment to something nft will accept inside quotes.
func sanitiseComment(c string) string {
	c = strings.Map(func(r rune) rune {
		switch {
		case r == '"', r == '\\', r == '\n', r == '\r':
			return -1
		case r < 0x20:
			return -1
		default:
			return r
		}
	}, c)
	if len(c) > 60 {
		c = c[:60]
	}
	if c == "" {
		c = "opanel"
	}
	return c
}
