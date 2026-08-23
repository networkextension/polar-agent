package routecmd

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func mustNorm(t *testing.T, r Route) Route {
	t.Helper()
	n, err := Normalize(r)
	if err != nil {
		t.Fatalf("normalize %+v: %v", r, err)
	}
	return n
}

func TestNormalize(t *testing.T) {
	cases := []struct {
		in      Route
		wantDst string
		wantFam int
		wantErr bool
	}{
		{Route{Dst: "10.10.10.5/24", Kind: "via", Gateway: "192.168.11.64"}, "10.10.10.0/24", 4, false},
		{Route{Dst: "default", Kind: "via", Gateway: "192.168.11.1"}, "0.0.0.0/0", 4, false},
		{Route{Dst: "192.168.50.7", Kind: "blackhole"}, "192.168.50.7/32", 4, false},
		{Route{Dst: "fd00:1::/64", Kind: "via", Gateway: "fd00::1"}, "fd00:1::/64", 6, false},
		{Route{Dst: "10.0.0.0/8", Kind: "via", Gateway: "fd00::1"}, "", 0, true}, // family mismatch
		{Route{Dst: "10.0.0.0/8", Kind: "iface"}, "", 0, true},                   // iface required
		{Route{Dst: "nope", Kind: "via", Gateway: "1.1.1.1"}, "", 0, true},
		{Route{Dst: "10.0.0.0/8", Kind: "teleport"}, "", 0, true},
	}
	for _, c := range cases {
		got, err := Normalize(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("%+v: expected error", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("%+v: %v", c.in, err)
			continue
		}
		if got.Dst != c.wantDst || got.Family != c.wantFam {
			t.Errorf("%+v → %s/%d, want %s/%d", c.in, got.Dst, got.Family, c.wantDst, c.wantFam)
		}
	}
}

func TestHashDeterministic(t *testing.T) {
	a := []Route{mustNorm(t, Route{Dst: "10.1.0.0/16", Kind: "via", Gateway: "10.0.0.1"}),
		mustNorm(t, Route{Dst: "10.2.0.0/16", Kind: "blackhole"})}
	b := []Route{a[1], a[0]}
	b[0].Flags = "observed-noise"
	if Hash(a) != Hash(b) {
		t.Fatal("hash must ignore order and flags")
	}
	if !strings.HasPrefix(Hash(a), "sha256:") {
		t.Fatal("hash prefix")
	}
}

func TestDiffThreeWay(t *testing.T) {
	via := func(dst, gw string) Route { return mustNorm(t, Route{Dst: dst, Kind: "via", Gateway: gw}) }
	desired := []Route{via("10.10.10.0/24", "192.168.11.64"), via("10.20.0.0/16", "192.168.11.9")}
	managed := []Route{via("10.10.10.0/24", "192.168.11.64"), via("10.30.0.0/16", "192.168.11.9")}
	kernel := []Route{
		via("10.10.10.0/24", "192.168.11.64"),                                            // unchanged
		via("10.30.0.0/16", "192.168.11.9"),                                              // managed, no longer desired → delete
		via("10.20.0.0/16", "192.168.11.77"),                                             // desired but wrong gw → change
		{Family: 4, Dst: "192.168.11.0/24", Kind: KindIface, Iface: "en0", Flags: "UCS"}, // unmanaged: untouched
	}
	ops := Diff(desired, managed, kernel)
	got := map[string]string{}
	for _, op := range ops {
		got[op.Route.Dst] = op.Action
	}
	want := map[string]string{"10.20.0.0/16": "change", "10.30.0.0/16": "delete"}
	if len(got) != len(want) {
		t.Fatalf("ops = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: %s, want %s", k, got[k], v)
		}
	}
	// managed route already gone from kernel → no delete op
	ops = Diff(nil, managed, nil)
	if len(ops) != 0 {
		t.Fatalf("delete of absent route must be a no-op, got %v", ops)
	}
}

func TestRenderGolden(t *testing.T) {
	def := mustNorm(t, Route{Dst: "default", Kind: "via", Gateway: "192.168.11.1"})
	via := mustNorm(t, Route{Dst: "10.10.10.0/24", Kind: "via", Gateway: "192.168.11.64"})
	ifr := mustNorm(t, Route{Dst: "10.50.0.0/16", Kind: "iface", Iface: "utun3"})
	bh := mustNorm(t, Route{Dst: "10.9.0.0/16", Kind: "blackhole"})
	v6 := mustNorm(t, Route{Dst: "fd00:1::/64", Kind: "via", Gateway: "fd00::1"})
	m := via
	m.Metric = 50
	cases := []struct {
		goos string
		op   Op
		want string
	}{
		{"darwin", Op{"add", via}, "route -n add -inet 10.10.10.0/24 192.168.11.64"},
		{"darwin", Op{"change", def}, "route -n change -inet default 192.168.11.1"},
		{"darwin", Op{"delete", via}, "route -n delete -inet 10.10.10.0/24"},
		{"darwin", Op{"add", ifr}, "route -n add -inet 10.50.0.0/16 -interface utun3"},
		{"darwin", Op{"add", bh}, "route -n add -inet 10.9.0.0/16 127.0.0.1 -blackhole"},
		{"freebsd", Op{"add", v6}, "route -n add -inet6 fd00:1::/64 fd00::1"},
		{"openbsd", Op{"add", bh}, "route -n add -inet 10.9.0.0/16 127.0.0.1 -reject"},
		{"linux", Op{"add", via}, "ip -4 route replace 10.10.10.0/24 via 192.168.11.64"},
		{"linux", Op{"change", m}, "ip -4 route replace 10.10.10.0/24 via 192.168.11.64 metric 50"},
		{"linux", Op{"add", def}, "ip -4 route replace 0.0.0.0/0 via 192.168.11.1"},
		{"linux", Op{"delete", via}, "ip -4 route del 10.10.10.0/24"},
		{"linux", Op{"add", ifr}, "ip -4 route replace 10.50.0.0/16 dev utun3"},
		{"linux", Op{"add", bh}, "ip -4 route replace blackhole 10.9.0.0/16"},
		{"linux", Op{"add", v6}, "ip -6 route replace fd00:1::/64 via fd00::1"},
	}
	for _, c := range cases {
		argv, err := Render(c.goos, c.op)
		if err != nil {
			t.Errorf("%s %+v: %v", c.goos, c.op, err)
			continue
		}
		if got := strings.Join(argv, " "); got != c.want {
			t.Errorf("%s %s %s:\n got %q\nwant %q", c.goos, c.op.Action, c.op.Route.Dst, got, c.want)
		}
	}
	if _, err := Render("plan9", Op{"add", via}); err == nil {
		t.Error("unsupported OS must error")
	}
}

func readTestdata(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func find(rs []Route, dst string) *Route {
	for i := range rs {
		if rs[i].Dst == dst {
			return &rs[i]
		}
	}
	return nil
}

func TestParseNetstatDarwin(t *testing.T) {
	rs := ParseTable("darwin", 4, readTestdata(t, "netstat_darwin_inet.txt"))
	if len(rs) == 0 {
		t.Fatal("no routes parsed")
	}
	def := find(rs, "0.0.0.0/0")
	if def == nil || def.Kind != KindVia || def.Gateway != "192.168.11.1" || def.Iface != "en0" {
		t.Fatalf("default route parsed as %+v", def)
	}
	if r := find(rs, "10.88.0.0/24"); r == nil || r.Kind != KindIface || r.Iface != "utun1" {
		t.Fatalf("10.88/24 parsed as %+v", r)
	}
	if r := find(rs, "127.0.0.0/8"); r == nil {
		t.Fatal("classful shorthand '127' not normalized")
	}
	for _, r := range rs {
		if strings.ContainsAny(r.Flags, "Wl") {
			t.Errorf("cloned/link entry leaked: %+v", r)
		}
	}
}

func TestParseNetstatFreeBSD(t *testing.T) {
	rs := ParseTable("freebsd", 4, readTestdata(t, "netstat_freebsd_inet.txt"))
	def := find(rs, "0.0.0.0/0")
	if def == nil || def.Gateway != "192.168.11.1" || def.Iface != "dpni1" {
		t.Fatalf("default: %+v", def)
	}
	if r := find(rs, "100.64.0.0/24"); r == nil || r.Kind != KindVia || r.Gateway != "100.64.0.1" || r.Iface != "wgc1" {
		t.Fatalf("via: %+v", r)
	}
	if r := find(rs, "10.9.0.0/16"); r == nil || r.Kind != KindBlackhole {
		t.Fatalf("blackhole (R flag): %+v", r)
	}
	if r := find(rs, "10.10.10.0/24"); r == nil || r.Kind != KindIface || r.Iface != "dpni2" {
		t.Fatalf("link route: %+v", r)
	}
}

func TestParseIPRouteLinux(t *testing.T) {
	rs := ParseTable("linux", 4, readTestdata(t, "ip_route_linux_v4.txt"))
	if len(rs) != 7 {
		t.Fatalf("want 7 routes, got %d: %+v", len(rs), rs)
	}
	def := find(rs, "0.0.0.0/0")
	if def == nil || def.Kind != KindVia || def.Gateway != "192.168.11.1" || def.Iface != "eth0" || def.Metric != 100 {
		t.Fatalf("default: %+v", def)
	}
	if r := find(rs, "10.10.10.0/24"); r == nil || r.Gateway != "192.168.11.64" {
		t.Fatalf("via: %+v", r)
	}
	if r := find(rs, "10.9.0.0/16"); r == nil || r.Kind != KindBlackhole {
		t.Fatalf("blackhole: %+v", r)
	}
	if r := find(rs, "172.17.0.0/16"); r == nil || r.Kind != KindIface || r.Iface != "docker0" {
		t.Fatalf("linkdown flag handling: %+v", r)
	}
}

func TestParseRoundTripDiff(t *testing.T) {
	// A desired route that is already in the kernel (as parsed) must diff to nothing.
	kernel := ParseTable("linux", 4, readTestdata(t, "ip_route_linux_v4.txt"))
	desired := []Route{mustNorm(t, Route{Dst: "10.10.10.0/24", Kind: "via", Gateway: "192.168.11.64"})}
	if ops := Diff(desired, desired, kernel); len(ops) != 0 {
		t.Fatalf("expected no-op, got %+v", ops)
	}
}

func TestContainsOverlaps(t *testing.T) {
	if !Contains("10.88.0.0/24", "10.88.0.0/25") || Contains("10.88.0.0/25", "10.88.0.0/24") {
		t.Fatal("Contains")
	}
	if !Overlaps("10.88.0.0/24", "10.88.0.0/16") || Overlaps("10.88.0.0/24", "10.89.0.0/24") {
		t.Fatal("Overlaps")
	}
}

func TestResolveBin(t *testing.T) {
	exists := func(p string) bool { return p == "/usr/sbin/netstat" }
	got := ResolveBin([]string{"definitely-not-on-path-netstat"}, exists)
	if got[0] != "definitely-not-on-path-netstat" {
		t.Fatal("unknown tool must pass through")
	}
	// Force the PATH miss by clearing PATH for the call.
	t.Setenv("PATH", "/nonexistent")
	got = ResolveBin([]string{"netstat", "-rn"}, exists)
	if got[0] != "/usr/sbin/netstat" || got[1] != "-rn" {
		t.Fatalf("got %v", got)
	}
	if got := ResolveBin([]string{"/sbin/route", "-n"}, exists); got[0] != "/sbin/route" {
		t.Fatal("absolute argv must be untouched")
	}
}

func TestCanonicalNeverNil(t *testing.T) {
	if Canonical(nil) == nil {
		t.Fatal("Canonical(nil) must be an empty, non-nil slice (state files serialise as [])")
	}
}

func TestLookupLongestPrefix(t *testing.T) {
	tbl := ParseTable("linux", 4, readTestdata(t, "ip_route_linux_v4.txt"))
	if r := Lookup(tbl, net.ParseIP("10.10.10.5")); r == nil || r.Dst != "10.10.10.0/24" || r.Gateway != "192.168.11.64" {
		t.Fatalf("LPM: %+v", r)
	}
	if r := Lookup(tbl, net.ParseIP("8.8.8.8")); r == nil || !r.IsDefault() {
		t.Fatalf("default fallback: %+v", r)
	}
	if r := Lookup(tbl, net.ParseIP("10.9.1.1")); r == nil || r.Kind != KindBlackhole {
		t.Fatalf("blackhole: %+v", r)
	}
	if Lookup(nil, net.ParseIP("1.1.1.1")) != nil || Lookup(tbl, nil) != nil {
		t.Fatal("nil cases")
	}
	if r := Lookup(tbl, net.ParseIP("fd00::1")); r != nil {
		t.Fatalf("v6 ip must not match v4 table: %+v", r)
	}
}

func TestFilter(t *testing.T) {
	tbl := ParseTable("linux", 4, readTestdata(t, "ip_route_linux_v4.txt"))
	if n := len(Filter(tbl, "")); n != len(tbl) {
		t.Fatal("empty query = all")
	}
	if got := Filter(tbl, "10.10.10.5"); len(got) != 2 { // 10.10.10.0/24 + default
		t.Fatalf("ip filter: %+v", got)
	}
	if got := Filter(tbl, "10.0.0.0/8"); len(got) != 4 { // default + 10.10.10/24 + 10.88.0/24 + 10.9/16
		t.Fatalf("cidr filter: %+v", got)
	}
	if got := Filter(tbl, "docker0"); len(got) != 1 || got[0].Iface != "docker0" {
		t.Fatalf("iface text filter: %+v", got)
	}
	if got := Filter(tbl, "BLACKHOLE"); len(got) != 2 { // blackhole + unreachable both map to kind=blackhole
		t.Fatalf("kind text filter case-insensitive: %+v", got)
	}
}
