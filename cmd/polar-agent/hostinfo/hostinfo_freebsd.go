//go:build freebsd

package hostinfo

// FreeBSD collector. sysctl for everything; no GPU detection
// (arm64 server boards in the lab don't have one, and even on
// x86 FreeBSD desktop GPUs the detection story is messy).
//
// Note hw.model on FreeBSD means "CPU description" (e.g.
// "ARM Cortex-A72 r0p3"), unlike macOS where it's the machine
// identifier (e.g. "Mac15,8"). We just emit it raw and let
// dock interpret per-OS.

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

func collectOS(h *HostInfo) {
	h.HwModel = sysctlString("hw.model")
	h.CPUBrand = h.HwModel // FreeBSD's hw.model already IS the CPU brand on most arches
	h.CPUCores = sysctlInt("hw.ncpu")
	h.MemoryBytes = sysctlUint("hw.physmem")
	h.BootUnix = parseFreeBSDBoottime(sysctlRaw("kern.boottime"))

	osRel := sysctlString("kern.osrelease")
	h.OSName = "FreeBSD"
	h.OSVersion = osRel

	// Kernel: "FreeBSD 14.4-RELEASE arm64" — matches the format used
	// on darwin/linux.
	h.Kernel = strings.TrimSpace("FreeBSD " + osRel + " " + h.CPUArch)
}

func sysctlString(key string) string {
	return trimNL(execCapture("sysctl", 2*time.Second, "-n", key))
}

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

func execCapture(name string, timeout time.Duration, args ...string) string {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).Output()
	if err != nil {
		return ""
	}
	return string(out)
}
