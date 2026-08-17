//go:build openbsd || netbsd

package hostinfo

// OpenBSD / NetBSD collector (polar-cloud guests on Apple VZ, but written
// generically). sysctl for everything, like FreeBSD; no GPU/battery/fan.
//
// Machine UUID (dock v4 register REQUIRES one — dedup + host identity):
//   OpenBSD: hw.uuid (SMBIOS system UUID; Apple VZ exposes SMBIOS → set)
//   NetBSD:  machdep.dmi.system-uuid
// Fallback: sha256 of a per-install nonce we persist under /etc/polar-machine-id
// (generated once) — stable across reboots, unique per clone (removed at bake).

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const machineIDFile = "/etc/polar-machine-id"

func collectOS(h *HostInfo) {
	h.HwModel = sysctlString("hw.model")
	h.HwVendor = sysctlString("hw.vendor")
	h.CPUCores = sysctlInt("hw.ncpu")
	if h.CPUCores == 0 {
		h.CPUCores = sysctlInt("hw.ncpuonline")
	}
	h.MemoryBytes = sysctlUint("hw.physmem")
	if h.MemoryBytes == 0 {
		h.MemoryBytes = sysctlUint("hw.physmem64")
	}
	h.BootUnix = parseBSDBoottime(sysctlRaw("kern.boottime"))
	osRel := sysctlString("kern.osrelease")
	switch runtime.GOOS {
	case "openbsd":
		h.OSName = "OpenBSD"
	case "netbsd":
		h.OSName = "NetBSD"
	}
	h.OSVersion = osRel
	h.Kernel = strings.TrimSpace(h.OSName + " " + osRel + " " + h.CPUArch)
	h.MachineUUID = collectBSDMachineUUID()
	h.DiskTotalBytes = diskTotalBytes("/")
}

func collectBSDMachineUUID() string {
	for _, k := range []string{"hw.uuid", "machdep.dmi.system-uuid"} {
		if v := sysctlString(k); v != "" && !strings.EqualFold(v, "00000000-0000-0000-0000-000000000000") {
			return v
		}
	}
	if b, err := os.ReadFile(machineIDFile); err == nil {
		if s := strings.TrimSpace(string(b)); s != "" {
			sum := sha256.Sum256([]byte(s))
			return hex.EncodeToString(sum[:])
		}
	}
	// generate + persist (best effort; root)
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return ""
	}
	nonce := hex.EncodeToString(buf)
	_ = os.WriteFile(machineIDFile, []byte(nonce+"\n"), 0o644)
	sum := sha256.Sum256([]byte(nonce))
	return hex.EncodeToString(sum[:])
}

// diskTotalBytes: `df -Pk` (Go's syscall has no Statfs on NetBSD). Zero on error.
func diskTotalBytes(path string) uint64 {
	out := execCapture("/bin/df", 2*time.Second, "-Pk", path)
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) < 2 {
		return 0
	}
	f := strings.Fields(lines[len(lines)-1])
	if len(f) < 2 {
		return 0
	}
	kb, _ := strconv.ParseUint(f[1], 10, 64)
	return kb * 1024
}

// kern.boottime prints as "kern.boottime=Mon Aug 17 16:52:00 2026" (OpenBSD, sysctl -n gives
// the epoch on newer releases) or "kern.boottime = Mon ..." — try epoch first, then date.
func parseBSDBoottime(raw string) int64 {
	raw = strings.TrimSpace(raw)
	if i := strings.Index(raw, "="); i >= 0 {
		raw = strings.TrimSpace(raw[i+1:])
	}
	if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
		return n
	}
	for _, layout := range []string{"Mon Jan 2 15:04:05 2006", "Mon Jan _2 15:04:05 2006", "Mon Jan  2 15:04:05 2006"} {
		if t, err := time.ParseInLocation(layout, raw, time.Local); err == nil {
			return t.Unix()
		}
	}
	return 0
}

func sysctlString(key string) string {
	return trimNL(execCapture("/sbin/sysctl", 2*time.Second, "-n", key))
}

func sysctlRaw(key string) string {
	return trimNL(execCapture("/sbin/sysctl", 2*time.Second, key))
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
