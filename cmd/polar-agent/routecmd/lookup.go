package routecmd

import (
	"net"
	"strings"
)

// Lookup returns the route the kernel would pick for `ip`: longest prefix
// match, ties broken by lower metric. nil when nothing matches (no default
// route either).
func Lookup(table []Route, ip net.IP) *Route {
	if ip == nil {
		return nil
	}
	fam := 6
	if ip.To4() != nil {
		fam = 4
	}
	var best *Route
	bestLen := -1
	for i := range table {
		r := &table[i]
		if r.Family != fam {
			continue
		}
		_, n, err := net.ParseCIDR(r.Dst)
		if err != nil || !n.Contains(ip) {
			continue
		}
		ones, _ := n.Mask.Size()
		if ones > bestLen || (ones == bestLen && best != nil && r.Metric < best.Metric) {
			best, bestLen = r, ones
		}
	}
	return best
}

// Filter narrows a table by a free-text query: an IP → routes whose dst
// contains it (most specific first is the caller's job via Lookup); a CIDR
// → routes overlapping it; anything else → substring match on dst /
// gateway / iface / kind / flags. Empty q → the whole table.
func Filter(table []Route, q string) []Route {
	q = strings.TrimSpace(q)
	if q == "" {
		return table
	}
	out := []Route{}
	if ip := net.ParseIP(q); ip != nil {
		for _, r := range table {
			if _, n, err := net.ParseCIDR(r.Dst); err == nil && n.Contains(ip) {
				out = append(out, r)
			}
		}
		return out
	}
	if _, _, err := net.ParseCIDR(q); err == nil {
		for _, r := range table {
			if Overlaps(q, r.Dst) {
				out = append(out, r)
			}
		}
		return out
	}
	lq := strings.ToLower(q)
	for _, r := range table {
		if strings.Contains(strings.ToLower(r.Dst+" "+r.Gateway+" "+r.Iface+" "+r.Kind+" "+r.Flags), lq) {
			out = append(out, r)
		}
	}
	return out
}
