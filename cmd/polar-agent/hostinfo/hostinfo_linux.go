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
	"syscall"
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

	// Stable machine fingerprint for dock-side dedup. systemd writes
	// /etc/machine-id at first boot; on older systems / minimal
	// containers /var/lib/dbus/machine-id is the fallback. Both are
	// 32-hex-char machine IDs (NOT the same shape as a UUID, but
	// stable across reboots and that's what matters).
	h.MachineUUID = readLinuxMachineID()

	// Root-fs capacity (Tier-2 static fact). statfs, no exec. Zero on error.
	h.DiskTotalBytes = diskTotalBytes("/")

	// Tier-1/2 static facts via sysfs (pure file reads, no exec).
	h.WifiMAC = linuxWifiMAC()
	h.HasBattery = linuxHasBattery() // *bool: nil when /sys/class/power_supply is unreadable
	h.HasFan = linuxHasFan()         // *bool: nil when /sys/class/hwmon is unreadable
}

// linuxWifiMAC returns the lowercase MAC of the first wireless interface, or
// "" when there's none. A net interface is wireless iff
// /sys/class/net/<iface>/wireless/ exists (the cfg80211 marker).
func linuxWifiMAC() string {
	ents, err := os.ReadDir("/sys/class/net")
	if err != nil {
		return ""
	}
	for _, e := range ents {
		n := e.Name()
		if _, err := os.Stat("/sys/class/net/" + n + "/wireless"); err != nil {
			continue
		}
		if mac := readTrim("/sys/class/net/" + n + "/address"); mac != "" {
			return strings.ToLower(mac)
		}
	}
	return ""
}

// linuxHasBattery reports whether any /sys/class/power_supply/* has type
// "Battery" (laptops yes, servers/VMs no). Returns nil (unknown) when the
// directory can't be read at all, vs &false when it's readable but empty.
func linuxHasBattery() *bool {
	ents, err := os.ReadDir("/sys/class/power_supply")
	if err != nil {
		return nil
	}
	found := false
	for _, e := range ents {
		if strings.TrimSpace(readTrim("/sys/class/power_supply/"+e.Name()+"/type")) == "Battery" {
			found = true
			break
		}
	}
	return &found
}

// linuxHasFan reports whether any hwmon device exposes a fan tachometer
// (/sys/class/hwmon/*/fan*_input). nil (unknown) when /sys/class/hwmon is
// unreadable. Note: a fan with no readable sensor reads as &false — the best
// signal sysfs offers without privileged probing.
func linuxHasFan() *bool {
	ents, err := os.ReadDir("/sys/class/hwmon")
	if err != nil {
		return nil
	}
	found := false
	for _, e := range ents {
		files, err := os.ReadDir("/sys/class/hwmon/" + e.Name())
		if err != nil {
			continue
		}
		for _, f := range files {
			n := f.Name()
			if strings.HasPrefix(n, "fan") && strings.HasSuffix(n, "_input") {
				found = true
				break
			}
		}
		if found {
			break
		}
	}
	return &found
}

// diskTotalBytes returns the total capacity (bytes) of the filesystem
// containing path via statfs. Zero on error.
func diskTotalBytes(path string) uint64 {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0
	}
	return st.Blocks * uint64(st.Bsize)
}

// readLinuxMachineID returns the systemd machine-id (or the legacy
// dbus fallback), trimmed. Empty string when both files are
// unreadable — dock-side dedup treats empty as "skip".
func readLinuxMachineID() string {
	for _, path := range []string{"/etc/machine-id", "/var/lib/dbus/machine-id"} {
		b, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		s := strings.TrimSpace(string(b))
		if s != "" {
			return s
		}
	}
	return ""
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
