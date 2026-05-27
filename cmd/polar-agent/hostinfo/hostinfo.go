// Package hostinfo collects static facts about the agent host —
// hardware model, CPU/memory/GPU shape, OS version, boot time — and
// returns them as a JSON-friendly struct that ships in the `hello`
// envelope. Dock uses these to differentiate hosts in the UI
// ("which Mac is the 64 GB one?", "this is the ESXi VM").
//
// Three platforms supported: darwin, freebsd, linux. Each has its
// own collectOS() in a build-tagged file. The pure parsers below
// (parseLinuxOSRelease, parseDarwinSystemProfilerGPU, etc.) take
// raw strings and live in this OS-neutral file so every platform's
// parser is unit-testable from every other platform.
//
// Collect() is sync.Once cached: agent reconnects don't re-pay
// system_profiler's ~600 ms latency on macOS.
package hostinfo

import (
	"bufio"
	"runtime"
	"strconv"
	"strings"
	"sync"
)

// HostInfo is the wire-shape payload. Field tags map to the keys
// dock side reads out of hello.host_info. omitempty everywhere so
// per-OS missing fields don't pollute the JSON.
type HostInfo struct {
	HwModel       string `json:"hw_model,omitempty"`
	HwVendor      string `json:"hw_vendor,omitempty"`
	Virt          string `json:"virt,omitempty"` // linux only: "vmware" | "kvm" | "xen" | "hyperv" | "none"

	CPUBrand     string `json:"cpu_brand,omitempty"`
	CPUArch      string `json:"cpu_arch,omitempty"`
	CPUCores     int    `json:"cpu_cores,omitempty"`
	CPUCoresPerf int    `json:"cpu_cores_perf,omitempty"` // darwin only
	CPUCoresEff  int    `json:"cpu_cores_eff,omitempty"`  // darwin only

	MemoryBytes uint64 `json:"memory_bytes,omitempty"`

	GPU *GPU `json:"gpu,omitempty"`

	OSName        string `json:"os_name,omitempty"`
	OSVersion     string `json:"os_version,omitempty"`
	OSBuild       string `json:"os_build,omitempty"`         // darwin only
	OSReleaseType string `json:"os_release_type,omitempty"`  // darwin only ("NonUI" for headless macOS)
	OSPretty      string `json:"os_pretty,omitempty"`        // linux only (PRETTY_NAME from os-release)
	Kernel        string `json:"kernel,omitempty"`           // "uname -srm"-shaped
	BootUnix      int64  `json:"boot_unix,omitempty"`
}

// GPU describes one (or the primary, if there are multiple) GPU.
// VRAM/unified memory size deliberately omitted in v1 — on Apple
// Silicon it's shared with main RAM and on ESXi VMs it's usually
// zero or undetectable.
type GPU struct {
	Vendor string `json:"vendor"`
	Model  string `json:"model,omitempty"`
	Cores  int    `json:"cores,omitempty"`
}

var (
	cacheOnce sync.Once
	cached    HostInfo
)

// Collect returns the host facts. First call does the real work;
// later calls return the cached value. Safe for concurrent use.
//
// On darwin the first call execs system_profiler (~600 ms); on
// linux/freebsd it's all sysfs/sysctl reads (~microseconds).
func Collect() HostInfo {
	cacheOnce.Do(func() {
		h := HostInfo{CPUArch: runtime.GOARCH}
		collectOS(&h)
		cached = h
	})
	return cached
}

// --- Pure parsers below — OS-neutral, unit-tested from any platform. ---

// parseLinuxOSRelease pulls os_name / version / pretty from a
// /etc/os-release blob. Quoting handled (KEY="value" or KEY=value).
func parseLinuxOSRelease(blob string) (name, version, pretty string) {
	s := bufio.NewScanner(strings.NewReader(blob))
	for s.Scan() {
		line := s.Text()
		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			continue
		}
		k := line[:eq]
		v := strings.Trim(line[eq+1:], `"'`)
		switch k {
		case "NAME":
			name = v
		case "VERSION_ID":
			version = v
		case "PRETTY_NAME":
			pretty = v
		}
	}
	return
}

// parseLinuxCPUInfo extracts the CPU brand ("model name" on x86,
// "Processor"/"Hardware" on ARM) and the logical core count. The
// brand is the value of the first matching key; the count is the
// number of "processor :" lines.
func parseLinuxCPUInfo(blob string) (brand string, cores int) {
	s := bufio.NewScanner(strings.NewReader(blob))
	for s.Scan() {
		line := s.Text()
		colon := strings.IndexByte(line, ':')
		if colon < 0 {
			continue
		}
		k := strings.TrimSpace(line[:colon])
		v := strings.TrimSpace(line[colon+1:])
		switch k {
		case "processor":
			cores++
		case "model name":
			if brand == "" {
				brand = v
			}
		case "Hardware", "Processor":
			if brand == "" {
				brand = v
			}
		}
	}
	return
}

// parseLinuxMemInfo extracts MemTotal (kB) and converts to bytes.
func parseLinuxMemInfo(blob string) uint64 {
	s := bufio.NewScanner(strings.NewReader(blob))
	for s.Scan() {
		line := s.Text()
		if !strings.HasPrefix(line, "MemTotal:") {
			continue
		}
		// "MemTotal:       16384000 kB"
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0
		}
		kb, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return 0
		}
		return kb * 1024
	}
	return 0
}

// parseLinuxStatBtime returns the value of `btime <unix>` from
// /proc/stat. Zero on missing/malformed.
func parseLinuxStatBtime(blob string) int64 {
	s := bufio.NewScanner(strings.NewReader(blob))
	for s.Scan() {
		line := s.Text()
		if !strings.HasPrefix(line, "btime ") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0
		}
		bt, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return 0
		}
		return bt
	}
	return 0
}

// detectVirt picks a virtualization label from DMI sys_vendor +
// /proc/cpuinfo flags + /sys/hypervisor/type. Empty string means
// no hypervisor detected — caller represents as "none" only if
// it specifically wants to record "we looked and found nothing".
//
// Order matters: DMI vendor is the most specific; the cpuinfo
// "hypervisor" flag is the catch-all fallback. /sys/hypervisor/type
// covers Xen, where the DMI string is sometimes blank.
func detectVirt(sysVendor, hypervisorType, cpuFlags string) string {
	v := strings.ToLower(sysVendor)
	switch {
	case strings.Contains(v, "vmware"):
		return "vmware"
	case strings.Contains(v, "microsoft corporation"):
		return "hyperv"
	case strings.Contains(v, "qemu"), strings.Contains(v, "kvm"),
		strings.Contains(v, "redhat"), strings.Contains(v, "red hat"):
		return "kvm"
	case strings.Contains(v, "xen"):
		return "xen"
	case strings.Contains(v, "innotek gmbh"), strings.Contains(v, "virtualbox"):
		return "virtualbox"
	case strings.Contains(v, "amazon ec2"):
		return "ec2-nitro"
	}
	if strings.ToLower(strings.TrimSpace(hypervisorType)) == "xen" {
		return "xen"
	}
	// Fallback: cpuinfo flags contain "hypervisor" → some hypervisor
	// is present but we couldn't identify which. Better than nothing.
	if strings.Contains(" "+cpuFlags+" ", " hypervisor ") {
		return "generic"
	}
	return ""
}

// parseDarwinSwVers reads `sw_vers` plaintext output and pulls
// ProductName / ProductVersion / BuildVersion / ReleaseType. Each
// line is "Key:<tab>Value".
func parseDarwinSwVers(blob string) (name, version, build, releaseType string) {
	s := bufio.NewScanner(strings.NewReader(blob))
	for s.Scan() {
		line := s.Text()
		colon := strings.IndexByte(line, ':')
		if colon < 0 {
			continue
		}
		k := strings.TrimSpace(line[:colon])
		v := strings.TrimSpace(line[colon+1:])
		switch k {
		case "ProductName":
			name = v
		case "ProductVersion":
			version = v
		case "BuildVersion":
			build = v
		case "ReleaseType":
			releaseType = v
		}
	}
	return
}

// parseDarwinSystemProfilerGPU pulls the primary GPU's chipset model,
// vendor (the "Apple" / "NVIDIA" prefix), and core count from the
// `system_profiler SPDisplaysDataType` output. Returns nil when no
// GPU section is found (rare on macOS, but defensive).
//
// system_profiler emits sections like:
//
//   Graphics/Displays:
//
//       Apple M3 Max:
//         Chipset Model: Apple G15X
//         Type: GPU
//         Bus: Built-In
//         Total Number of Cores: 40
//         Vendor: Apple (0x106b)
//
// We only need the first GPU's three values.
func parseDarwinSystemProfilerGPU(blob string) *GPU {
	var g GPU
	s := bufio.NewScanner(strings.NewReader(blob))
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		switch {
		case strings.HasPrefix(line, "Chipset Model:") && g.Model == "":
			g.Model = strings.TrimSpace(strings.TrimPrefix(line, "Chipset Model:"))
		case strings.HasPrefix(line, "Vendor:") && g.Vendor == "":
			raw := strings.TrimSpace(strings.TrimPrefix(line, "Vendor:"))
			// "Apple (0x106b)" → "Apple"
			if paren := strings.Index(raw, " ("); paren > 0 {
				raw = raw[:paren]
			}
			g.Vendor = raw
		case strings.HasPrefix(line, "Total Number of Cores:") && g.Cores == 0:
			raw := strings.TrimSpace(strings.TrimPrefix(line, "Total Number of Cores:"))
			if n, err := strconv.Atoi(raw); err == nil {
				g.Cores = n
			}
		}
	}
	if g.Model == "" && g.Vendor == "" && g.Cores == 0 {
		return nil
	}
	return &g
}

// parseFreeBSDBoottime extracts the unix seconds from sysctl's
// `kern.boottime: { sec = 1779008689, usec = 347411 } …` shape.
// Returns 0 on parse failure.
func parseFreeBSDBoottime(blob string) int64 {
	// Look for "sec = " literal.
	idx := strings.Index(blob, "sec = ")
	if idx < 0 {
		return 0
	}
	rest := blob[idx+len("sec = "):]
	end := strings.IndexAny(rest, ",} ")
	if end < 0 {
		return 0
	}
	bt, err := strconv.ParseInt(strings.TrimSpace(rest[:end]), 10, 64)
	if err != nil {
		return 0
	}
	return bt
}

// trimNL is a sysctl shorthand — values come back with a trailing \n
// from exec'd stdout; this strips it consistently across darwin/freebsd.
func trimNL(s string) string {
	return strings.TrimRight(s, "\r\n")
}
