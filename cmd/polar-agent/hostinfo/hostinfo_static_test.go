package hostinfo

// Unit tests for the Tier-1/2 static-fact parsers (P0). Pure, OS-neutral
// — they run on every platform's CI like the rest of hostinfo_test.go.

import "testing"

func TestParseDarwinWifiMAC(t *testing.T) {
	blob := `Hardware Port: Ethernet
Device: en4
Ethernet Address: 00:e0:4c:68:01:02

Hardware Port: Wi-Fi
Device: en0
Ethernet Address: 70:72:FE:F3:5A:62

Hardware Port: Thunderbolt Bridge
Device: bridge0
Ethernet Address: N/A
`
	if got := parseDarwinWifiMAC(blob); got != "70:72:fe:f3:5a:62" {
		t.Errorf("wifi MAC = %q, want lowercased 70:72:fe:f3:5a:62", got)
	}
}

func TestParseDarwinWifiMAC_NoWifi(t *testing.T) {
	// Ethernet-only Mac / VM: no Wi-Fi block → empty.
	blob := "Hardware Port: Ethernet\nDevice: en0\nEthernet Address: 00:e0:4c:68:01:02\n"
	if got := parseDarwinWifiMAC(blob); got != "" {
		t.Errorf("no-wifi MAC = %q, want empty", got)
	}
}

func TestParseDarwinWifiMAC_NA(t *testing.T) {
	// Wi-Fi present but address reads N/A (adapter off / VM) → empty.
	blob := "Hardware Port: Wi-Fi\nDevice: en0\nEthernet Address: N/A\n"
	if got := parseDarwinWifiMAC(blob); got != "" {
		t.Errorf("N/A wifi MAC = %q, want empty", got)
	}
}

func TestParseDarwinModelName(t *testing.T) {
	blob := `Hardware:

    Hardware Overview:

      Model Name: MacBook Pro
      Model Identifier: Mac15,10
      Chip: Apple M3 Max
`
	if got := parseDarwinModelName(blob); got != "MacBook Pro" {
		t.Errorf("model name = %q, want MacBook Pro", got)
	}
	if got := parseDarwinModelName("Hardware Overview:\n  Chip: Apple M2\n"); got != "" {
		t.Errorf("missing model name = %q, want empty", got)
	}
}

func TestParseDarwinHasBattery(t *testing.T) {
	laptop := "Now drawing from 'AC Power'\n -InternalBattery-0 (id=6226019)\t100%; charged;\n"
	if !parseDarwinHasBattery(laptop) {
		t.Error("laptop pmset should report a battery")
	}
	desktop := "Now drawing from 'AC Power'\n"
	if parseDarwinHasBattery(desktop) {
		t.Error("desktop pmset should report no battery")
	}
}

func TestIsFanlessModel(t *testing.T) {
	fanless := []string{"MacBook Air", "MacBook Air (M2, 2022)", "macbook air", "MacBook"}
	for _, m := range fanless {
		if !isFanlessModel(m) {
			t.Errorf("%q should be fanless", m)
		}
	}
	hasFan := []string{"MacBook Pro", "Mac mini", "Mac Studio", "Mac Pro", "iMac", ""}
	for _, m := range hasFan {
		if isFanlessModel(m) {
			t.Errorf("%q should NOT be fanless", m)
		}
	}
}

// --- Network-topology parsers (host-network-topology.md). Blobs below are the
// real shapes captured from a zen M3 Max (bridge0 = Thunderbolt Bridge with
// en1/en2/en3 members). ---

func TestParseDarwinHardwarePorts(t *testing.T) {
	blob := `Hardware Port: Ethernet
Device: en10
Ethernet Address: 6c:6e:07:0d:93:5a

Hardware Port: Wi-Fi
Device: en0
Ethernet Address: 48:e1:5c:c2:b3:87

Hardware Port: Thunderbolt 1
Device: en1
Ethernet Address: 36:83:9a:11:75:80

Hardware Port: Thunderbolt Bridge
Device: bridge0
Ethernet Address: 36:83:9a:11:75:80

Hardware Port: Bluetooth PAN
Device: en9
Ethernet Address: N/A
`
	got := parseDarwinHardwarePorts(blob)
	want := map[string]string{
		"en10":    "ethernet",
		"en0":     "wifi",
		"en1":     "thunderbolt",
		"bridge0": "thunderbolt",
	}
	if len(got) != len(want) {
		t.Fatalf("ports = %v, want %v", got, want)
	}
	for dev, kind := range want {
		if got[dev] != kind {
			t.Errorf("port %s = %q, want %q", dev, got[dev], kind)
		}
	}
	// Bluetooth PAN doesn't classify → must be absent (caller defaults to other).
	if _, ok := got["en9"]; ok {
		t.Errorf("Bluetooth PAN en9 should not be classified, got %q", got["en9"])
	}
}

func TestDarwinPortKind(t *testing.T) {
	cases := map[string]string{
		"Wi-Fi":              "wifi",
		"Thunderbolt Bridge": "thunderbolt",
		"Thunderbolt 1":      "thunderbolt",
		"Ethernet":           "ethernet",
		"USB 10/100/1000 LAN": "ethernet",
		"Bluetooth PAN":      "",
	}
	for port, want := range cases {
		if got := darwinPortKind(port); got != want {
			t.Errorf("darwinPortKind(%q) = %q, want %q", port, got, want)
		}
	}
}

func TestParseDarwinBridgeMembers(t *testing.T) {
	blob := `bridge0: flags=8863<UP,BROADCAST,SMART,RUNNING,SIMPLEX,MULTICAST> mtu 1500
	options=63<RXCSUM,TXCSUM,TSO4,TSO6>
	ether 36:83:9a:11:75:80
	Configuration:
		id 0:0:0:0:0:0 priority 0 hellotime 0 fwddelay 0
	member: en1 flags=3<LEARNING,DISCOVER>
	member: en2 flags=3<LEARNING,DISCOVER>
	member: en3 flags=3<LEARNING,DISCOVER>
	nd6 options=201<PERFORMNUD,DAD>
	media: <unknown type>
`
	got := parseDarwinBridgeMembers(blob)
	want := []string{"en1", "en2", "en3"}
	if len(got) != len(want) {
		t.Fatalf("members = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("member[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestParseRouteDefaultGW(t *testing.T) {
	blob := `   route to: default
destination: default
       mask: default
    gateway: 192.168.11.1
  interface: en10
      flags: <UP,GATEWAY,DONE,STATIC,PRCLONING,GLOBAL>
`
	if got := parseRouteDefaultGW(blob); got != "192.168.11.1" {
		t.Errorf("default gw = %q, want 192.168.11.1", got)
	}
	// No default route (wg-only host) → empty.
	if got := parseRouteDefaultGW("   route to: default\n no gateway here\n"); got != "" {
		t.Errorf("no-gw blob = %q, want empty", got)
	}
}

func TestIsCGNAT(t *testing.T) {
	in := []string{"100.64.0.2/10", "100.127.255.255/32", "100.100.1.1"}
	for _, c := range in {
		if !isCGNAT(c) {
			t.Errorf("%q should be CGNAT", c)
		}
	}
	out := []string{"100.63.0.1/24", "100.128.0.1/24", "192.168.11.57/24", "10.88.0.1/24", "garbage"}
	for _, c := range out {
		if isCGNAT(c) {
			t.Errorf("%q should NOT be CGNAT", c)
		}
	}
}
