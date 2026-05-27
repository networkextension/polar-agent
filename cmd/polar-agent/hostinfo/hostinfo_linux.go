//go:build linux

package hostinfo

// Linux collector. Pure file reads from /proc and /sys/class/dmi.
// No exec dependencies — works in a stripped container or a
// VMware ESXi guest with nothing but coreutils available.
//
// VM detection: DMI sys_vendor pinned via systemd's convention.
// On ESXi guests sys_vendor reads "VMware, Inc." — that's the
// signal we lean on.

import (
	"os"
	"strings"
)

func collectOS(h *HostInfo) {
	// /etc/os-release — every modern distro ships it.
	if blob, err := os.ReadFile("/etc/os-release"); err == nil {
		name, ver, pretty := parseLinuxOSRelease(string(blob))
		h.OSName = name
		h.OSVersion = ver
		h.OSPretty = pretty
	}

	// /proc/cpuinfo: brand + logical core count.
	if blob, err := os.ReadFile("/proc/cpuinfo"); err == nil {
		brand, cores := parseLinuxCPUInfo(string(blob))
		h.CPUBrand = brand
		h.CPUCores = cores
	}

	// /proc/meminfo: MemTotal.
	if blob, err := os.ReadFile("/proc/meminfo"); err == nil {
		h.MemoryBytes = parseLinuxMemInfo(string(blob))
	}

	// /proc/stat: btime.
	if blob, err := os.ReadFile("/proc/stat"); err == nil {
		h.BootUnix = parseLinuxStatBtime(string(blob))
	}

	// DMI: hw_model + hw_vendor + virt detection.
	sysVendor := readTrim("/sys/class/dmi/id/sys_vendor")
	prodName := readTrim("/sys/class/dmi/id/product_name")
	hypervisorType := readTrim("/sys/hypervisor/type")
	cpuFlags := readCPUFlags()

	h.HwModel = prodName
	h.HwVendor = sysVendor
	h.Virt = detectVirt(sysVendor, hypervisorType, cpuFlags)
	if h.Virt == "" {
		h.Virt = "none"
	}

	// Kernel via /proc/sys/kernel — avoids syscall.Uname's [65]int8
	// awkwardness.
	relName := readTrim("/proc/sys/kernel/ostype")    // "Linux"
	relVer := readTrim("/proc/sys/kernel/osrelease")  // "6.8.0-45-generic"
	if relName == "" {
		relName = "Linux"
	}
	h.Kernel = strings.TrimSpace(relName + " " + relVer + " " + h.CPUArch)

	// GPU detection on Linux is heterogeneous — NVIDIA via nvidia-smi
	// if present, AMD via /sys/class/drm. ESXi VMs almost never have
	// passthrough GPUs so this returns nil in the common path; left
	// for a follow-up rather than half-implemented in v1.
}

// readTrim reads a one-line sysfs/procfs file and returns the
// content with surrounding whitespace + the trailing newline
// stripped. Empty string on any read error — caller treats
// "" as "field unavailable".
func readTrim(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// readCPUFlags joins all "flags :" lines from /proc/cpuinfo so
// detectVirt can substring-match for "hypervisor". We don't try
// to dedupe — substring search doesn't care.
func readCPUFlags() string {
	blob, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return ""
	}
	var flags strings.Builder
	for _, line := range strings.Split(string(blob), "\n") {
		colon := strings.IndexByte(line, ':')
		if colon < 0 {
			continue
		}
		if strings.TrimSpace(line[:colon]) != "flags" {
			continue
		}
		flags.WriteString(strings.TrimSpace(line[colon+1:]))
		flags.WriteByte(' ')
	}
	return flags.String()
}
