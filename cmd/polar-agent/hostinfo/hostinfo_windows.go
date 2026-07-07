//go:build windows

package hostinfo

// Windows collector: one PowerShell exec pulls everything through CIM
// (Win32_ComputerSystem / *Product / _OperatingSystem / _Processor /
// _VideoController / _LogicalDisk / _Battery) and prints compact JSON;
// parseWindowsCIMJSON in hostinfo.go maps it onto HostInfo. ~1-2 s on
// first call, absorbed by Collect()'s sync.Once cache.
//
// MachineUUID prefers the SMBIOS system UUID (survives OS reinstall,
// consistent with the freebsd collector). Boards that ship the bogus
// all-zero / all-FF SMBIOS value fall back to the per-install
// HKLM\SOFTWARE\Microsoft\Cryptography MachineGuid.

import (
	"context"
	"os/exec"
	"strings"
	"time"
)

// winCIMScript emits one compact JSON object with every static fact we
// want. Each CIM read is isolated with -ErrorAction SilentlyContinue so
// a broken provider degrades one field, not the whole payload.
const winCIMScript = `$ErrorActionPreference='SilentlyContinue'
$cs=Get-CimInstance Win32_ComputerSystem
$csp=Get-CimInstance Win32_ComputerSystemProduct
$os=Get-CimInstance Win32_OperatingSystem
$cpu=Get-CimInstance Win32_Processor | Select-Object -First 1
$gpu=Get-CimInstance Win32_VideoController | Select-Object -First 1
$disk=Get-CimInstance Win32_LogicalDisk -Filter "DeviceID='C:'"
$boot=0
if ($os.LastBootUpTime) { $boot=[System.DateTimeOffset]::new($os.LastBootUpTime.ToUniversalTime(),[System.TimeSpan]::Zero).ToUnixTimeSeconds() }
[PSCustomObject]@{
  uuid=$csp.UUID
  vendor=$cs.Manufacturer
  model=$cs.Model
  model_name=$csp.Version
  cpu=$cpu.Name
  cores=[int]$cs.NumberOfLogicalProcessors
  mem=[uint64]$cs.TotalPhysicalMemory
  os=$os.Caption
  ver=$os.Version
  build=$os.BuildNumber
  boot=[int64]$boot
  disk=[uint64]$disk.Size
  gpu=$gpu.Name
  gpu_vendor=$gpu.AdapterCompatibility
  battery=[bool](Get-CimInstance Win32_Battery)
} | ConvertTo-Json -Compress`

func collectOS(h *HostInfo) {
	parseWindowsCIMJSON(h, execCapture("powershell", 20*time.Second,
		"-NoProfile", "-NonInteractive", "-Command", winCIMScript))

	if !validSMBIOSUUID(h.MachineUUID) {
		h.MachineUUID = parseWindowsMachineGuid(execCapture("reg", 5*time.Second,
			"query", `HKLM\SOFTWARE\Microsoft\Cryptography`, "/v", "MachineGuid"))
	}
}

// execCapture runs `name args...` with a short timeout, returning
// stdout on success and an empty string on any failure — same contract
// as the darwin/freebsd copies: missing/slow tools degrade the payload
// rather than killing the agent.
func execCapture(name string, timeout time.Duration, args ...string) string {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).Output()
	if err != nil {
		return ""
	}
	return string(out)
}

// validSMBIOSUUID rejects the empty / all-zero / all-FF placeholder
// values some boards report — dock-side dedup must never see a UUID
// shared across machines (see the MachineUUID doc in hostinfo.go).
func validSMBIOSUUID(u string) bool {
	u = strings.TrimSpace(u)
	if len(u) < 8 {
		return false
	}
	stripped := strings.ToUpper(strings.NewReplacer("-", "", ":", "").Replace(u))
	return strings.Trim(stripped, "0") != "" && strings.Trim(stripped, "F") != ""
}
