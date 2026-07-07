//go:build !darwin && !freebsd && !linux && !windows

package hostinfo

// Build-tag stub for platforms without a collector (OpenBSD, etc.).
// The agent still ships there; host_info just stays sparse —
// CPUArch comes from runtime.GOARCH so dock at least knows the platform.

func collectOS(_ *HostInfo) {}
