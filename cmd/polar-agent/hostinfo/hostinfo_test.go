package hostinfo

// Parser unit tests. No build tags — every platform compiles and
// runs every parser test, so we exercise the Linux DMI parsing on a
// macOS dev box and the macOS system_profiler parsing on Linux CI.
// The OS-specific collectOS() (which does the actual exec / file
// read on its platform) is NOT tested here — those are integration
// concerns covered by manual smoke against the orchestrate harness.

import (
	"strings"
	"testing"
)

func TestParseLinuxOSRelease(t *testing.T) {
	blob := `NAME="Ubuntu"
VERSION="24.04.1 LTS (Noble Numbat)"
ID=ubuntu
VERSION_ID="24.04"
PRETTY_NAME="Ubuntu 24.04.1 LTS"
`
	name, ver, pretty := parseLinuxOSRelease(blob)
	if name != "Ubuntu" || ver != "24.04" || pretty != "Ubuntu 24.04.1 LTS" {
		t.Errorf("got name=%q ver=%q pretty=%q", name, ver, pretty)
	}
}

func TestParseLinuxOSRelease_UnquotedValues(t *testing.T) {
	// Some distros (Alpine, Arch) emit unquoted values.
	blob := "NAME=Arch Linux\nVERSION_ID=rolling\nPRETTY_NAME=Arch Linux\n"
	name, ver, pretty := parseLinuxOSRelease(blob)
	if name != "Arch Linux" || ver != "rolling" || pretty != "Arch Linux" {
		t.Errorf("unquoted: name=%q ver=%q pretty=%q", name, ver, pretty)
	}
}

func TestParseLinuxCPUInfo_X86(t *testing.T) {
	// Two-vCPU ESXi guest: two stanzas, brand from first "model name".
	blob := `processor	: 0
vendor_id	: GenuineIntel
model name	: Intel(R) Xeon(R) Gold 6346 CPU @ 3.10GHz
flags		: fpu hypervisor sse2

processor	: 1
vendor_id	: GenuineIntel
model name	: Intel(R) Xeon(R) Gold 6346 CPU @ 3.10GHz
flags		: fpu hypervisor sse2
`
	brand, cores := parseLinuxCPUInfo(blob)
	if brand != "Intel(R) Xeon(R) Gold 6346 CPU @ 3.10GHz" {
		t.Errorf("brand: %q", brand)
	}
	if cores != 2 {
		t.Errorf("cores: %d", cores)
	}
}

func TestParseLinuxCPUInfo_ARM(t *testing.T) {
	// ARM SBC cpuinfo has Hardware/Processor, not model name.
	blob := `processor	: 0
BogoMIPS	: 50.00
Features	: fp asimd
Hardware	: BCM2835
`
	brand, cores := parseLinuxCPUInfo(blob)
	if brand != "BCM2835" {
		t.Errorf("ARM brand: %q", brand)
	}
	if cores != 1 {
		t.Errorf("ARM cores: %d", cores)
	}
}

func TestParseLinuxMemInfo(t *testing.T) {
	blob := `MemTotal:       16384000 kB
MemFree:         8000000 kB
MemAvailable:   12000000 kB
`
	if got := parseLinuxMemInfo(blob); got != 16384000*1024 {
		t.Errorf("MemTotal: %d", got)
	}
}

func TestParseLinuxMemInfo_Missing(t *testing.T) {
	if got := parseLinuxMemInfo("Buffers: 0\n"); got != 0 {
		t.Errorf("expected 0 on missing MemTotal, got %d", got)
	}
}

func TestParseLinuxStatBtime(t *testing.T) {
	blob := "cpu  100 0 50\nbtime 1716705421\nprocesses 1234\n"
	if got := parseLinuxStatBtime(blob); got != 1716705421 {
		t.Errorf("btime: %d", got)
	}
}

func TestDetectVirt_VMware(t *testing.T) {
	if v := detectVirt("VMware, Inc.", "", "fpu hypervisor sse2"); v != "vmware" {
		t.Errorf("vmware: %q", v)
	}
}

func TestDetectVirt_KVM(t *testing.T) {
	if v := detectVirt("Red Hat", "", "fpu hypervisor"); v != "kvm" {
		t.Errorf("RH/kvm: %q", v)
	}
	if v := detectVirt("QEMU", "", ""); v != "kvm" {
		t.Errorf("qemu: %q", v)
	}
}

func TestDetectVirt_HyperV(t *testing.T) {
	if v := detectVirt("Microsoft Corporation", "", ""); v != "hyperv" {
		t.Errorf("hyperv: %q", v)
	}
}

func TestDetectVirt_XenViaHypervisorType(t *testing.T) {
	if v := detectVirt("", "xen", ""); v != "xen" {
		t.Errorf("xen: %q", v)
	}
}

func TestDetectVirt_FallbackGeneric(t *testing.T) {
	if v := detectVirt("Unknown Hypervisor Inc", "", "fpu hypervisor sse4"); v != "generic" {
		t.Errorf("generic fallback: %q", v)
	}
}

func TestDetectVirt_BareMetal(t *testing.T) {
	if v := detectVirt("Dell Inc.", "", "fpu sse4 avx"); v != "" {
		t.Errorf("bare metal should return empty, got %q", v)
	}
}

func TestParseDarwinSwVers(t *testing.T) {
	blob := "ProductName:\t\tmacOS\n" +
		"ProductVersion:\t\t14.5.1\n" +
		"BuildVersion:\t\t23F79\n"
	name, ver, build, releaseType := parseDarwinSwVers(blob)
	if name != "macOS" || ver != "14.5.1" || build != "23F79" || releaseType != "" {
		t.Errorf("got name=%q ver=%q build=%q releaseType=%q", name, ver, build, releaseType)
	}
}

func TestParseDarwinSwVers_NonUI(t *testing.T) {
	// Captured verbatim from one of the lab Macs (local@10.88.0.1).
	blob := `ProductName:		macOS
ProductVersion:		14.0
BuildVersion:		23A32391j
ReleaseType:		NonUI
`
	name, ver, build, releaseType := parseDarwinSwVers(blob)
	if releaseType != "NonUI" {
		t.Errorf("ReleaseType: want NonUI, got %q", releaseType)
	}
	if name != "macOS" || ver != "14.0" || build != "23A32391j" {
		t.Errorf("got name=%q ver=%q build=%q", name, ver, build)
	}
}

func TestParseDarwinSystemProfilerGPU(t *testing.T) {
	// Real output captured from M3 Max (40-core variant).
	blob := `Graphics/Displays:

    Apple M3 Max:

      Chipset Model: Apple G15X
      Type: GPU
      Bus: Built-In
      Total Number of Cores: 40
      Vendor: Apple (0x106b)
      Metal Support: Metal 3
`
	g := parseDarwinSystemProfilerGPU(blob)
	if g == nil {
		t.Fatal("expected GPU, got nil")
	}
	if g.Model != "Apple G15X" {
		t.Errorf("Model: %q", g.Model)
	}
	if g.Cores != 40 {
		t.Errorf("Cores: %d", g.Cores)
	}
	if g.Vendor != "Apple" {
		t.Errorf("Vendor: want Apple (without paren), got %q", g.Vendor)
	}
}

func TestParseDarwinSystemProfilerGPU_NoGPU(t *testing.T) {
	if g := parseDarwinSystemProfilerGPU(""); g != nil {
		t.Errorf("expected nil on empty, got %+v", g)
	}
	if g := parseDarwinSystemProfilerGPU("Graphics/Displays:\n\n"); g != nil {
		t.Errorf("expected nil on no GPU section, got %+v", g)
	}
}

func TestParseFreeBSDBoottime(t *testing.T) {
	blob := "kern.boottime: { sec = 1778631299, usec = 929665 } Wed May 13 02:14:59 2026"
	if got := parseFreeBSDBoottime(blob); got != 1778631299 {
		t.Errorf("boottime: %d", got)
	}
}

func TestParseFreeBSDBoottime_Malformed(t *testing.T) {
	if got := parseFreeBSDBoottime("not a boottime line"); got != 0 {
		t.Errorf("expected 0, got %d", got)
	}
	if got := parseFreeBSDBoottime("kern.boottime: { sec = NOT_A_NUMBER, usec = 0 }"); got != 0 {
		t.Errorf("expected 0 on parse fail, got %d", got)
	}
}

func TestCollectIsCached(t *testing.T) {
	// First call may be slow; second must return identical value.
	h1 := Collect()
	h2 := Collect()
	if h1 != h2 {
		t.Errorf("Collect should be cached: %+v vs %+v", h1, h2)
	}
}

func TestCollectFillsArchAlways(t *testing.T) {
	h := Collect()
	if h.CPUArch == "" {
		t.Error("CPUArch should always be set via runtime.GOARCH, even on the stub-OS path")
	}
}

// Sanity: ensure none of the canned darwin sample data leaks
// machdep-style raw bytes containing the "(0x...)" vendor suffix
// into the parsed Vendor field.
func TestParseDarwinSystemProfilerGPU_VendorStripped(t *testing.T) {
	blob := "      Chipset Model: Foo\n      Vendor: ACME Inc (0xdead)\n"
	g := parseDarwinSystemProfilerGPU(blob)
	if g == nil {
		t.Fatal("expected GPU")
	}
	if strings.Contains(g.Vendor, "(0x") {
		t.Errorf("vendor should be stripped of paren suffix: %q", g.Vendor)
	}
	if g.Vendor != "ACME Inc" {
		t.Errorf("vendor: %q", g.Vendor)
	}
}
