//go:build !darwin && !freebsd && !linux

package hostinfo

// Build-tag stub for platforms without a collector (Windows, OpenBSD,
// etc.). The agent still ships there; host_info just stays sparse —
// CPUArch comes from runtime.GOARCH so dock at least knows the platform.

func collectOS(_ *HostInfo) {}
