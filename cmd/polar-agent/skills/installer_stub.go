//go:build !unix

package skills

// Build-tag stub: the real Installer rides on the bundle skill, which
// is unix-only (venv setup + process-group kill). On other platforms
// the bundle skill is never registered, so getInstaller() in
// installer_glue.go never finds it and NewInstaller is never reached
// at runtime — these exist only to keep loop.go compiling.

import "context"

// Installer stub — see installer.go for the real implementation.
type Installer struct{}

// NewInstaller returns nil on non-unix platforms (no bundle skill).
func NewInstaller(_ Skill) *Installer { return nil }

func (i *Installer) Install(_ context.Context, req InstallRequest) InstallResult {
	return InstallResult{InstallID: req.InstallID, Status: InstallStatusFSError, Error: "bundle installs not supported on this platform"}
}

func (i *Installer) Uninstall(req UninstallRequest, _ map[int64]string) UninstallResult {
	return UninstallResult{InstallID: req.InstallID, Status: InstallStatusFSError, Error: "bundle installs not supported on this platform"}
}
