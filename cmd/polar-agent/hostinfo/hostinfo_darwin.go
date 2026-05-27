//go:build darwin

package hostinfo

// macOS collector: sysctl reads for the fast facts, sw_vers for the
// product name + build + ReleaseType, and system_profiler for the
// GPU (the only field that needs an exec with non-trivial latency
// ~600 ms; cache absorbs it).

import (
	"context"
	"os/exec"
	"strconv"
	"time"
)

func collectOS(h *HostInfo) {
	h.HwModel = sysctlString("hw.model")
	h.CPUBrand = sysctlString("machdep.cpu.brand_string")
	h.CPUCores = sysctlInt("hw.ncpu")
	h.CPUCoresPerf = sysctlInt("hw.perflevel0.physicalcpu")
	h.CPUCoresEff = sysctlInt("hw.perflevel1.physicalcpu")
	h.MemoryBytes = sysctlUint("hw.memsize")
	h.BootUnix = parseFreeBSDBoottime(sysctlRaw("kern.boottime"))
	h.Kernel = "Darwin " + sysctlString("kern.osrelease") + " " + h.CPUArch

	if name, ver, build, releaseType := parseDarwinSwVers(execCapture("sw_vers", 2*time.Second)); name != "" {
		h.OSName = name
		h.OSVersion = ver
		h.OSBuild = build
		h.OSReleaseType = releaseType
	}

	// system_profiler is the slow one. Cap at 5s — if it hangs (rare,
	// happens after some IOKit bad states) we'd rather ship without
	// GPU info than block the agent's hello.
	if gpu := parseDarwinSystemProfilerGPU(execCapture("system_profiler", 5*time.Second, "SPDisplaysDataType")); gpu != nil {
		h.GPU = gpu
	}
}

// sysctlString reads one string sysctl key via -n. Returns "" on error.
func sysctlString(key string) string {
	return trimNL(execCapture("sysctl", 2*time.Second, "-n", key))
}

// sysctlRaw is like sysctlString but keeps full sysctl format
// ("kern.boottime: { sec = ... }") — needed for boottime parsing.
func sysctlRaw(key string) string {
	return trimNL(execCapture("sysctl", 2*time.Second, key))
}

func sysctlInt(key string) int {
	n, _ := strconv.Atoi(sysctlString(key))
	return n
}

func sysctlUint(key string) uint64 {
	n, _ := strconv.ParseUint(sysctlString(key), 10, 64)
	return n
}

// execCapture runs `name args...` with a short timeout, returning
// stdout on success and an empty string on any failure. Used for
// every shell-out in this file; missing/slow tools degrade the
// payload rather than killing the agent.
func execCapture(name string, timeout time.Duration, args ...string) string {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).Output()
	if err != nil {
		return ""
	}
	return string(out)
}
