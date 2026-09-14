package actions

import (
	"context"
	"net"
	"os"
	"strings"

	"github.com/bnixvn/opanel-ent/internal/agent"
)

// NetworkAddress is one address on one interface.
type NetworkAddress struct {
	Interface string `json:"interface"`
	Address   string `json:"address"`
	Family    string `json:"family"` // "ipv4" or "ipv6"
	Global    bool   `json:"global"` // routable, as opposed to loopback or link-local
}

// NetworkInfo is what the panel shows on its settings page.
type NetworkInfo struct {
	Hostname  string           `json:"hostname"`
	Addresses []NetworkAddress `json:"addresses"`
	// PrimaryIPv4 and PrimaryIPv6 are the addresses an operator would give
	// out: the first global one of each family.
	PrimaryIPv4 string `json:"primary_ipv4,omitempty"`
	PrimaryIPv6 string `json:"primary_ipv6,omitempty"`
	// IPv6Available says the host has a routable v6 address, which is a
	// different question from whether the panel is configured to use it.
	IPv6Available bool `json:"ipv6_available"`
}

func registerNetwork(r *agent.Registry) {
	agent.Register(r, "net.info", 1, func(_ context.Context, _ struct{}) (NetworkInfo, error) {
		return networkInfo()
	})
}

func networkInfo() (NetworkInfo, error) {
	var out NetworkInfo
	out.Hostname, _ = osHostname()

	ifaces, err := net.Interfaces()
	if err != nil {
		return out, err
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipnet.IP
			// A global address is one an outside host could reach: not
			// loopback, not link-local, and for v6 not a unique-local
			// address either, which is the v6 equivalent of 10.0.0.0/8.
			global := !ip.IsLoopback() && !ip.IsLinkLocalUnicast() &&
				!ip.IsLinkLocalMulticast() && !ip.IsPrivate() &&
				!isUniqueLocal(ip)

			entry := NetworkAddress{
				Interface: iface.Name, Address: ip.String(), Global: global,
			}
			if ip.To4() != nil {
				entry.Family = "ipv4"
				if global && out.PrimaryIPv4 == "" {
					out.PrimaryIPv4 = entry.Address
				}
			} else {
				entry.Family = "ipv6"
				if global {
					out.IPv6Available = true
					if out.PrimaryIPv6 == "" {
						out.PrimaryIPv6 = entry.Address
					}
				}
			}
			out.Addresses = append(out.Addresses, entry)
		}
	}
	return out, nil
}

// isUniqueLocal reports whether an IPv6 address is in fc00::/7.
func isUniqueLocal(ip net.IP) bool {
	v6 := ip.To16()
	if v6 == nil || ip.To4() != nil {
		return false
	}
	return v6[0]&0xfe == 0xfc
}

// osHostname trims what the kernel reports, which on some hosts carries a
// trailing newline.
func osHostname() (string, error) {
	h, err := os.Hostname()
	return strings.TrimSpace(h), err
}
