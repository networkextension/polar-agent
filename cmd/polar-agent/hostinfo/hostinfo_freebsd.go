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
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Absolute path so we don't depend on PATH — same launchd-PATH
// discipline as the darwin collector (see hostinfo_darwin.go).
const binKenv = "/sbin/kenv"

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

	// Stable machine fingerprint. smbios.system.uuid requires the
	// smbios kld; bare arm64 VMs (like dpaa2) often don't have it
	// loaded and the kenv read returns empty. /etc/hostid is the
	// FreeBSD-ism fallback — when present it's a UUID-shaped string
	// written at hostid(8) init. If both fail, leave empty (dock
	// side treats empty as "skip dedup", which is correct).
	h.MachineUUID = collectFreeBSDMachineUUID()

	// Root-fs capacity (Tier-2 static fact). statfs, no exec. Zero on error.
	h.DiskTotalBytes = diskTotalBytes("/")
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

func collectFreeBSDMachineUUID() string {
	// Primary: smbios.system.uuid via kenv. -q suppresses the "no
	// such key" stderr noise on systems without the smbios kld.
	if v := trimNL(execCapture(binKenv, 2*time.Second, "-q", "smbios.system.uuid")); v != "" {
		return v
	}
	// Fallback: hash of /etc/hostid. Hashing rather than emitting the
	// raw hostid keeps the fingerprint shape (hex string) consistent
	// with the other OS branches and avoids leaking the raw uuid that
	// some software treats as somewhat sensitive (license bindings).
	if b, err := os.ReadFile("/etc/hostid"); err == nil {
		s := strings.TrimSpace(string(b))
		if s != "" {
			sum := sha256.Sum256([]byte(s))
			return hex.EncodeToString(sum[:])
		}
	}
	return ""
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
