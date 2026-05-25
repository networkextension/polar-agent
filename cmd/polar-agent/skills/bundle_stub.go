//go:build !unix

package skills

// Build-tag stub: bundle skill uses syscall.Setsid + Kill(-pgid) for
// graceful subprocess shutdown, which is unix-only. Returning nil
// here lets main.go call NewBundleSkill unconditionally; Register
// skips nil skills.

// NewBundleSkill returns nil on non-unix platforms.
func NewBundleSkill(rootDir string) Skill { return nil }
