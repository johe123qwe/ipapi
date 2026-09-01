package main

import "net/netip"

// bogonPrefixes are ranges that must never appear as a source address on the
// public Internet: RFC 1918 space, loopback, link-local, documentation ranges,
// CGNAT, multicast and reserved space.
var bogonPrefixes = func() []netip.Prefix {
	raw := []string{
		// IPv4
		"0.0.0.0/8",
		"10.0.0.0/8",
		"100.64.0.0/10",
		"127.0.0.0/8",
		"169.254.0.0/16",
		"172.16.0.0/12",
		"192.0.0.0/24",
		"192.0.2.0/24",
		"192.88.99.0/24",
		"192.168.0.0/16",
		"198.18.0.0/15",
		"198.51.100.0/24",
		"203.0.113.0/24",
		"224.0.0.0/4",
		"240.0.0.0/4",
		"255.255.255.255/32",
		// IPv6
		"::/128",
		"::1/128",
		"64:ff9b:1::/48",
		"100::/64",
		"2001:2::/48",
		"2001:10::/28",
		"2001:db8::/32",
		"fc00::/7",
		"fe80::/10",
		"ff00::/8",
	}
	out := make([]netip.Prefix, 0, len(raw))
	for _, s := range raw {
		if p, err := netip.ParsePrefix(s); err == nil {
			out = append(out, p)
		}
	}
	return out
}()

func isBogon(addr netip.Addr) bool {
	addr = addr.Unmap()
	if !addr.IsValid() {
		return true
	}
	for _, p := range bogonPrefixes {
		// A v4 prefix never contains a v6 address and vice versa, so a single
		// pass over the combined list is safe.
		if p.Addr().Is4() == addr.Is4() && p.Contains(addr) {
			return true
		}
	}
	return false
}
