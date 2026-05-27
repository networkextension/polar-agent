//go:build unix

package skills

// Unit tests for the wg-show-dump parser. Pure string→struct; doesn't
// need a real wg interface, kernel module, or root. The integration
// path (poll goroutine on a real iface) requires CAP_NET_ADMIN +
// wireguard-tools and is left to manual smoke testing, not CI.

import (
	"strings"
	"testing"
	"time"
)

func TestParseWGShowDump_IfaceOnlyNoPeers(t *testing.T) {
	// Single interface line, no peers. Private key column is filled
	// (we strip it). fwmark "off" → normalized to empty.
	dump := "PRIVKEY_SHOULD_BE_DROPPED\tIFACE_PUBKEY_42chars=\t51820\toff\n"
	iface, peers, err := parseWGShowDump([]byte(dump), time.Now())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if iface.PublicKey != "IFACE_PUBKEY_42chars=" {
		t.Errorf("PublicKey: got %q want IFACE_PUBKEY_42chars=", iface.PublicKey)
	}
	if iface.ListenPort != 51820 {
		t.Errorf("ListenPort: got %d want 51820", iface.ListenPort)
	}
	if iface.Fwmark != "" {
		t.Errorf("Fwmark: want empty for 'off', got %q", iface.Fwmark)
	}
	if len(peers) != 0 {
		t.Errorf("want 0 peers, got %d", len(peers))
	}
}

func TestParseWGShowDump_PrivateKeyNeverLeaks(t *testing.T) {
	// Belt-and-suspenders: encode a recognizable private key in the
	// dump and assert it doesn't show up anywhere in the parsed struct.
	const sentinel = "SUPER_SECRET_PRIVKEY_42_chars_long_test!"
	dump := sentinel + "\tPUB\t51820\toff\n"
	iface, peers, err := parseWGShowDump([]byte(dump), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	// Spot-check every public field on iface + peers; if the parser
	// ever accidentally surfaces column 0, this lights up.
	checks := []string{iface.PublicKey, iface.Fwmark}
	for _, p := range peers {
		checks = append(checks, p.PublicKey, p.Endpoint, p.AllowedIPs)
	}
	for _, v := range checks {
		if strings.Contains(v, sentinel) {
			t.Errorf("private key leaked into parsed struct: %q", v)
		}
	}
}

func TestParseWGShowDump_ThreePeersMixedStates(t *testing.T) {
	// Synthetic dump exercising:
	//   peer A: handshook 12s before "now", endpoint set, keepalive 25s,
	//           preshared key present, transfer counters non-zero
	//   peer B: never handshook (0), endpoint "(none)", keepalive "off",
	//           preshared "(none)", allowed_ips with multiple CIDRs
	//   peer C: handshook recently, no keepalive, large counters
	now := time.Unix(1_716_705_433, 0).UTC()
	dump := strings.Join([]string{
		"PRIV\tIFACE_PUB\t51820\t0xca6c",
		"PEER_A_PUB\tPSK_PRESENT\t1.2.3.4:51820\t10.0.0.2/32\t1716705421\t4096\t8192\t25",
		"PEER_B_PUB\t(none)\t(none)\t10.0.0.3/32,fd00::/64\t0\t0\t0\toff",
		"PEER_C_PUB\t(none)\t5.6.7.8:51820\t10.0.0.4/32\t1716705430\t1073741824\t2147483648\toff",
		"", // trailing newline edge — should be ignored
	}, "\n")

	iface, peers, err := parseWGShowDump([]byte(dump), now)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	if iface.Fwmark != "0xca6c" {
		t.Errorf("fwmark: got %q want 0xca6c", iface.Fwmark)
	}
	if len(peers) != 3 {
		t.Fatalf("want 3 peers, got %d", len(peers))
	}

	a, b, c := peers[0], peers[1], peers[2]

	// --- Peer A: live handshake, full counters.
	if a.PublicKey != "PEER_A_PUB" {
		t.Errorf("A.PublicKey: got %q", a.PublicKey)
	}
	if !a.HasPresharedKey {
		t.Error("A.HasPresharedKey: want true")
	}
	if a.Endpoint != "1.2.3.4:51820" {
		t.Errorf("A.Endpoint: got %q", a.Endpoint)
	}
	if a.AllowedIPs != "10.0.0.2/32" {
		t.Errorf("A.AllowedIPs: got %q", a.AllowedIPs)
	}
	if a.LatestHandshakeUnix != 1_716_705_421 {
		t.Errorf("A.LatestHandshakeUnix: got %d", a.LatestHandshakeUnix)
	}
	if a.HandshakeAgeSec != 12 {
		t.Errorf("A.HandshakeAgeSec: want 12, got %d", a.HandshakeAgeSec)
	}
	if a.BytesRx != 4096 || a.BytesTx != 8192 {
		t.Errorf("A counters: rx=%d tx=%d", a.BytesRx, a.BytesTx)
	}
	if a.KeepaliveSec != 25 {
		t.Errorf("A.KeepaliveSec: got %d want 25", a.KeepaliveSec)
	}

	// --- Peer B: never handshook, "(none)" normalizations.
	if b.HasPresharedKey {
		t.Error("B.HasPresharedKey: want false for '(none)'")
	}
	if b.Endpoint != "" {
		t.Errorf("B.Endpoint: want empty for '(none)', got %q", b.Endpoint)
	}
	if b.AllowedIPs != "10.0.0.3/32,fd00::/64" {
		t.Errorf("B.AllowedIPs: got %q", b.AllowedIPs)
	}
	if b.LatestHandshakeUnix != 0 {
		t.Errorf("B.LatestHandshakeUnix: want 0, got %d", b.LatestHandshakeUnix)
	}
	if b.HandshakeAgeSec != -1 {
		t.Errorf("B.HandshakeAgeSec: want -1 for never-handshook, got %d", b.HandshakeAgeSec)
	}
	if b.KeepaliveSec != 0 {
		t.Errorf("B.KeepaliveSec: want 0 for 'off', got %d", b.KeepaliveSec)
	}

	// --- Peer C: large counters survive int64 round-trip.
	if c.BytesRx != 1_073_741_824 || c.BytesTx != 2_147_483_648 {
		t.Errorf("C counters: rx=%d tx=%d", c.BytesRx, c.BytesTx)
	}
	if c.HandshakeAgeSec != 3 {
		t.Errorf("C.HandshakeAgeSec: want 3, got %d", c.HandshakeAgeSec)
	}
}

func TestParseWGShowDump_EmptyOutputErrors(t *testing.T) {
	if _, _, err := parseWGShowDump([]byte(""), time.Now()); err == nil {
		t.Error("empty: want error")
	}
	if _, _, err := parseWGShowDump([]byte("\n\n"), time.Now()); err == nil {
		t.Error("blank-lines-only: want error")
	}
}

func TestParseWGShowDump_MalformedIfaceLineErrors(t *testing.T) {
	// Iface line should have 4 tab-separated cols.
	dump := "only\ttwo\n"
	if _, _, err := parseWGShowDump([]byte(dump), time.Now()); err == nil {
		t.Error("malformed iface line: want error")
	}
}

func TestParseWGShowDump_MalformedPeerLineSkipped(t *testing.T) {
	// Iface line OK; one peer row missing columns. Parser should drop
	// the bad row but still return iface metadata.
	dump := strings.Join([]string{
		"PRIV\tPUB\t51820\toff",
		"PEER_A\tPSK\t1.2.3.4:51820\t10.0.0.2/32\t100\t0\t0\t0",
		"BROKEN_ROW_TOO_FEW_COLS\thuh",
		"PEER_C\t(none)\t(none)\t10.0.0.3/32\t0\t0\t0\toff",
	}, "\n")
	_, peers, err := parseWGShowDump([]byte(dump), time.Unix(200, 0))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(peers) != 2 {
		t.Fatalf("want 2 valid peers (broken row dropped), got %d", len(peers))
	}
	if peers[0].PublicKey != "PEER_A" || peers[1].PublicKey != "PEER_C" {
		t.Errorf("unexpected peers: %+v", peers)
	}
}

func TestParseWGShowDump_HandshakeAgeNotNegativeOnClockSkew(t *testing.T) {
	// If wallclock moves backwards (NTP correction) the dump's
	// latest_handshake can be > now. Clamp to 0 rather than emit
	// a negative age that breaks dock-side sorting.
	now := time.Unix(100, 0)
	dump := "PRIV\tPUB\t51820\toff\nPEER\t(none)\t(none)\t10/32\t200\t0\t0\t0\n"
	_, peers, err := parseWGShowDump([]byte(dump), now)
	if err != nil {
		t.Fatal(err)
	}
	if peers[0].HandshakeAgeSec != 0 {
		t.Errorf("want clamped age=0 on clock skew, got %d", peers[0].HandshakeAgeSec)
	}
}

func TestNormalizeFwmark(t *testing.T) {
	cases := map[string]string{
		"off":    "",
		"(none)": "",
		"0":      "",
		"":       "",
		"0xca6c": "0xca6c",
		"42":     "42",
	}
	for in, want := range cases {
		if got := normalizeFwmark(in); got != want {
			t.Errorf("normalizeFwmark(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseKeepalive(t *testing.T) {
	cases := map[string]int{
		"off":    0,
		"(none)": 0,
		"":       0,
		"25":     25,
		"bogus":  0,
	}
	for in, want := range cases {
		if got := parseKeepalive(in); got != want {
			t.Errorf("parseKeepalive(%q) = %d, want %d", in, got, want)
		}
	}
}
