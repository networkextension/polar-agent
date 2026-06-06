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
