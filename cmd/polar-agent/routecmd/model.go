// Package routecmd is the platform-neutral route model shared by the
// polar-routing control plane and the polar-agent executor: canonical
// route rows, a deterministic hash, the three-way diff (desired × managed ×
// kernel) and the per-OS command renderer / kernel-table parser.
//
// CANONICAL COPY lives in polar-routing/routecmd; polar-agent carries a
// byte-identical mirror at cmd/polar-agent/routecmd (polar-agent has no
// module dependency on polar-routing). Keep the two in sync — the JSON shape
// of Route is a cross-repo contract.
package routecmd

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"
)

const (
	KindVia       = "via"       // dst via gateway IP
	KindIface     = "iface"     // dst out of an interface (link route)
	KindBlackhole = "blackhole" // dst dropped

	DefaultV4 = "0.0.0.0/0"
	DefaultV6 = "::/0"
)

// Route is one desired or observed kernel route. Dst is always canonical
// CIDR (host routes are /32 or /128). Flags/Proto are parse-only metadata,
// never part of the hash.
type Route struct {
	Family  int    `json:"family"` // 4 | 6
	Dst     string `json:"dst"`
	Kind    string `json:"kind"`
	Gateway string `json:"gateway,omitempty"`
	Iface   string `json:"iface,omitempty"`
	Metric  int    `json:"metric,omitempty"`
	Flags   string `json:"flags,omitempty"` // observed only (netstat flags / ip proto)
}

// key identifies a route slot in the table: family + destination.
func (r Route) key() string { return fmt.Sprintf("%d|%s", r.Family, r.Dst) }

// IsDefault reports whether the route is a default route.
func (r Route) IsDefault() bool { return r.Dst == DefaultV4 || r.Dst == DefaultV6 }

// same reports whether two routes would produce the same forwarding entry.
func (r Route) same(o Route) bool {
	if r.Family != o.Family || r.Dst != o.Dst || r.Kind != o.Kind {
		return false
	}
	switch r.Kind {
	case KindVia:
		return r.Gateway == o.Gateway && (r.Iface == "" || o.Iface == "" || r.Iface == o.Iface)
	case KindIface:
		return r.Iface == o.Iface
	}
	return true
}

// Normalize validates a route and rewrites it in canonical form: CIDR
// with the host bits cleared, family derived from the address, gateway
// parsed, kind-specific field requirements enforced.
func Normalize(r Route) (Route, error) {
	dst := strings.TrimSpace(r.Dst)
	if dst == "default" {
		dst = DefaultV4
	}
	if !strings.Contains(dst, "/") {
		if ip := net.ParseIP(dst); ip != nil {
			if ip.To4() != nil {
				dst += "/32"
			} else {
				dst += "/128"
			}
		}
	}
	ip, ipnet, err := net.ParseCIDR(dst)
	if err != nil {
		return r, fmt.Errorf("dst %q: not a CIDR", r.Dst)
	}
	out := r
	out.Dst = ipnet.String()
	if ip.To4() != nil {
		out.Family = 4
	} else {
		out.Family = 6
	}
	out.Kind = strings.TrimSpace(strings.ToLower(r.Kind))
	out.Gateway = strings.TrimSpace(r.Gateway)
	out.Iface = strings.TrimSpace(r.Iface)
	out.Flags = ""
	switch out.Kind {
	case KindVia:
		gw := net.ParseIP(out.Gateway)
		if gw == nil {
			return r, fmt.Errorf("dst %s: gateway %q is not an IP", out.Dst, r.Gateway)
		}
		if (gw.To4() != nil) != (out.Family == 4) {
			return r, fmt.Errorf("dst %s: gateway %s family mismatch", out.Dst, gw)
		}
		out.Gateway = gw.String()
	case KindIface:
		if out.Iface == "" {
			return r, fmt.Errorf("dst %s: iface required for kind=iface", out.Dst)
		}
		out.Gateway = ""
	case KindBlackhole:
		out.Gateway, out.Iface = "", ""
	default:
		return r, fmt.Errorf("dst %s: kind must be %s|%s|%s", out.Dst, KindVia, KindIface, KindBlackhole)
	}
	if out.Metric < 0 {
		return r, errors.New("metric must be >= 0")
	}
	return out, nil
}

// Canonical sorts a route list deterministically (family, dst). Input is
// not mutated.
func Canonical(rs []Route) []Route {
	out := append([]Route(nil), rs...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Family != out[j].Family {
			return out[i].Family < out[j].Family
		}
		return out[i].Dst < out[j].Dst
	})
	return out
}

// Hash is the sha256 of the canonical JSON encoding (Flags stripped).
// Identical desired sets hash identically on both sides of the wire.
func Hash(rs []Route) string {
	c := Canonical(rs)
	for i := range c {
		c[i].Flags = ""
	}
	b, _ := json.Marshal(c)
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Op is one kernel mutation produced by Diff.
type Op struct {
	Action string `json:"action"` // add | change | delete
	Route  Route  `json:"route"`
}

// Diff computes the mutations that move the kernel table from its current
// state to `desired`, touching only slots polar-routing owns:
//
//   - desired route missing from kernel         → add
//   - desired route present but differs          → change
//   - managed (previously applied) route no longer desired → delete
//     (only if the kernel still has it; never deletes unmanaged routes)
//
// `managed` is the last committed desired set from the agent's state file.
func Diff(desired, managed, kernel []Route) []Op {
	kern := map[string]Route{}
	for _, k := range kernel {
		if _, dup := kern[k.key()]; !dup { // first match wins (lowest metric first on linux)
			kern[k.key()] = k
		}
	}
	want := map[string]Route{}
	var ops []Op
	for _, d := range Canonical(desired) {
		want[d.key()] = d
		k, ok := kern[d.key()]
		switch {
		case !ok:
			ops = append(ops, Op{Action: "add", Route: d})
		case !k.same(d):
			ops = append(ops, Op{Action: "change", Route: d})
		}
	}
	for _, m := range Canonical(managed) {
		if _, still := want[m.key()]; still {
			continue
		}
		if _, inKernel := kern[m.key()]; inKernel {
			ops = append(ops, Op{Action: "delete", Route: m})
		}
	}
	return ops
}

// Contains reports whether cidr `inner` lies entirely within `outer`.
func Contains(outer, inner string) bool {
	_, o, err := net.ParseCIDR(outer)
	if err != nil {
		return false
	}
	ip, in, err := net.ParseCIDR(inner)
	if err != nil {
		return false
	}
	if !o.Contains(ip) {
		return false
	}
	oo, _ := o.Mask.Size()
	ii, _ := in.Mask.Size()
	return ii >= oo
}

// Overlaps reports whether two CIDRs share any address.
func Overlaps(a, b string) bool {
	return Contains(a, b) || Contains(b, a)
}
