package main

import "testing"

func TestParseIrecoveryOutput(t *testing.T) {
	sample := `CPID: 0x8101
CPRV: 0x11
CPFM: 0x03
SCEP: 0x01
BDID: 0x0A
ECID: 0xABCD1234567890
IBFL: 0x39
SRTG: [iBoot-7459.40.10]
SRNM: [F2LXX12345]
IMEI: [123456789012345]
SDOM: 0x01
PWND: []
`
	d := parseIrecoveryOutput(sample)
	if d.CPID != 0x8101 {
		t.Errorf("CPID: got %#x want 0x8101", d.CPID)
	}
	if d.BDID != 0x0A {
		t.Errorf("BDID: got %#x want 0x0A", d.BDID)
	}
	if d.CPRV != 0x11 {
		t.Errorf("CPRV: got %#x want 0x11", d.CPRV)
	}
	if d.ECID != 0xABCD1234567890 {
		t.Errorf("ECID: got %#x want 0xABCD1234567890", d.ECID)
	}
	if d.SRTG != "iBoot-7459.40.10" {
		t.Errorf("SRTG: got %q want iBoot-7459.40.10", d.SRTG)
	}
	if d.SRNM != "F2LXX12345" {
		t.Errorf("SRNM: got %q want F2LXX12345", d.SRNM)
	}
	if d.IMEI != "123456789012345" {
		t.Errorf("IMEI: got %q", d.IMEI)
	}
}

func TestParseIrecoveryOutputEmpty(t *testing.T) {
	d := parseIrecoveryOutput("")
	if d.isPresent() {
		t.Errorf("empty input should not be present: %+v", d)
	}
}

func TestParseMaybeHex64(t *testing.T) {
	cases := []struct {
		in   string
		want int64
	}{
		{"0x80001234", 0x80001234},
		{"0X10", 0x10},
		{"42", 42},
		{"0", 0},
		{"", 0},
	}
	for _, tt := range cases {
		got := parseMaybeHex64(tt.in)
		if got != tt.want {
			t.Errorf("%q: got %d want %d", tt.in, got, tt.want)
		}
	}
}

func TestRecoveryDeviceInfoEquals(t *testing.T) {
	a := recoveryDeviceInfo{ECID: 1, CPID: 0x8101, BDID: 10}
	b := recoveryDeviceInfo{ECID: 1, CPID: 0x8101, BDID: 10}
	c := recoveryDeviceInfo{ECID: 2, CPID: 0x8101, BDID: 10}
	if !a.equals(b) {
		t.Error("a == b should be true")
	}
	if a.equals(c) {
		t.Error("a == c (different ECID) should be false")
	}
}

func TestRecoveryDeviceInfoIsPresent(t *testing.T) {
	if (recoveryDeviceInfo{}).isPresent() {
		t.Error("zero value should not be present")
	}
	if !(recoveryDeviceInfo{ECID: 42}).isPresent() {
		t.Error("non-zero ECID should be present")
	}
	if !(recoveryDeviceInfo{CPID: 0x8101}).isPresent() {
		t.Error("non-zero CPID should be present (ECID may be hidden in some modes)")
	}
}
