//go:build unix

package skills

// Tiny helpers used by installer.go's hostSatisfiesRequirement. Split
// out so they're trivial to override in tests via build tags later.

import (
	"os/exec"
	"runtime"
)

func goosIs(want string) bool {
	return runtime.GOOS == want
}

func python3Available() bool {
	_, err := exec.LookPath("python3")
	return err == nil
}
