package routecmd

import (
	"net"
	"strconv"
	"strings"
)

// ParseTable parses the output of ListArgv(goos, family) into routes.
// Best-effort: unparseable lines are skipped. ARP/NDP-cloned host entries,
// link-layer (MAC gateway) entries and link-local v6 are dropped — they
// are not forwarding policy and would only add noise to drift views.
func ParseTable(goos string, family int, out string) []Route {
	switch goos {
	case "darwin", "freebsd", "openbsd":
		return parseNetstat(family, out)
	case "linux":
		return parseIPRoute(family, out)
	}
	return nil
}

// parseNetstat handles `netstat -rn -f inet|inet6` (BSD family).
// Columns: Destination Gateway Flags [Refs Use Mtu] Netif [Expire] — the
// middle columns vary by OS, so Flags is located by shape (all-letter
// token after Gateway) and Netif is the last token that looks like an
// interface name.
func parseNetstat(family int, out string) []Route {
	var rs []Route
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 4 || f[0] == "Destination" || strings.HasPrefix(f[0], "Routing") || strings.HasPrefix(f[0], "Internet") {
			continue
		}
		dst := normalizeDst(f[0], family)
		if dst == "" {
			continue
		}
		gw, flags := f[1], f[2]
		if !strings.Contains(flags, "U") {
			continue
		}
		// ARP/NDP clones (W), link-layer (L/l) entries are not policy.
		if strings.ContainsAny(flags, "Wl") || strings.Contains(flags, "L") && !strings.Contains(flags, "G") {
			continue
		}
		iface := f[len(f)-1]
		if isNumeric(iface) && len(f) >= 5 { // trailing Expire column
			iface = f[len(f)-2]
		}
		r := Route{Family: family, Dst: dst, Iface: iface, Flags: flags}
		switch {
		case strings.Contains(flags, "B") || strings.Contains(flags, "R"):
			r.Kind = KindBlackhole
			r.Iface = ""
		case strings.Contains(flags, "G") && net.ParseIP(stripScope(gw)) != nil:
			r.Kind = KindVia
			r.Gateway = stripScope(gw)
		default:
			r.Kind = KindIface
		}
		if family == 6 && strings.HasPrefix(strings.ToLower(dst), "fe80:") {
			continue
		}
		rs = append(rs, r)
	}
	return rs
}

// parseIPRoute handles `ip -4|-6 route show`.
//
//	default via 192.168.11.1 dev eth0 proto dhcp metric 100
//	10.0.0.0/24 dev eth0 proto kernel scope link src 10.0.0.5
//	blackhole 10.9.0.0/16
//	unreachable 10.9.0.0/16
func parseIPRoute(family int, out string) []Route {
	var rs []Route
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		r := Route{Family: family, Kind: KindIface}
		i := 0
		switch f[0] {
		case "blackhole", "unreachable", "prohibit":
			r.Kind = KindBlackhole
			r.Flags = f[0]
			i = 1
		case "local", "broadcast", "multicast", "anycast":
			continue
		}
		if i >= len(f) {
			continue
		}
		r.Dst = normalizeDst(f[i], family)
		if r.Dst == "" {
			continue
		}
		for j := i + 1; j+1 < len(f); j += 2 {
			switch f[j] {
			case "via":
				if r.Kind != KindBlackhole {
					r.Kind = KindVia
					r.Gateway = f[j+1]
				}
			case "dev":
				r.Iface = f[j+1]
			case "metric":
				r.Metric, _ = strconv.Atoi(f[j+1])
			case "proto":
				if r.Flags == "" {
					r.Flags = f[j+1]
				}
			case "scope", "src", "table", "pref", "expires", "mtu", "advmss", "hoplimit":
				// value consumed
			default:
				j-- // flag without value (e.g. "linkdown", "onlink")
			}
		}
		if r.Kind == KindBlackhole {
			r.Iface = ""
		}
		if family == 6 && strings.HasPrefix(strings.ToLower(r.Dst), "fe80:") {
			continue
		}
		rs = append(rs, r)
	}
	return rs
}

// normalizeDst turns netstat/ip shorthand ("10.88.0/24", "10/8", "default",
// "192.168.11.1", "fe80::%en0/64") into canonical CIDR. Returns "" for
// anything that is not a destination prefix.
func normalizeDst(s string, family int) string {
	s = strings.TrimSpace(stripScope(s))
	if s == "" {
		return ""
	}
	if s == "default" {
		if family == 6 {
			return DefaultV6
		}
		return DefaultV4
	}
	if i := strings.Index(s, "/"); i > 0 {
		host, mask := s[:i], s[i:]
		if family == 4 && !strings.Contains(host, ":") {
			octets := strings.Split(host, ".")
			for len(octets) < 4 {
				octets = append(octets, "0")
			}
			host = strings.Join(octets, ".")
		}
		if _, n, err := net.ParseCIDR(host + mask); err == nil {
			return n.String()
		}
		return ""
	}
	if ip := net.ParseIP(s); ip != nil {
		if ip.To4() != nil {
			return ip.String() + "/32"
		}
		return ip.String() + "/128"
	}
	// netstat short form without mask ("10.88.0" = 10.88.0.0/24 by classful
	// shorthand) — pad and infer the mask from the octet count.
	if family == 4 {
		octets := strings.Split(s, ".")
		if n := len(octets); n > 0 && n < 4 && isNumeric(octets[0]) {
			for len(octets) < 4 {
				octets = append(octets, "0")
			}
			if _, ipn, err := net.ParseCIDR(strings.Join(octets, ".") + "/" + strconv.Itoa(n*8)); err == nil {
				return ipn.String()
			}
		}
	}
	return ""
}

func stripScope(s string) string {
	if i := strings.Index(s, "%"); i >= 0 {
		rest := ""
		if j := strings.Index(s[i:], "/"); j >= 0 {
			rest = s[i+j:]
		}
		return s[:i] + rest
	}
	return s
}

func isNumeric(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
