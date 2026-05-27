package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestDefaultServerIsZen pins the canonical control-plane URL the
// agent ships with. If you're intentionally rebranding for a fork,
// override at build time via:
//
//	go build -ldflags "-X main.defaultServer=https://custom.example:443"
//
// — don't edit the constant. See defaults.go for the why (2026-05-27
// "host lost" incident).
func TestDefaultServerIsZen(t *testing.T) {
	const want = "https://zen.4950.store:2443"
	if defaultServer != want {
		t.Fatalf("defaultServer = %q, want %q (override via -ldflags, not by editing the constant)",
			defaultServer, want)
	}
}

// TestDefaultServerLdflagOverride proves the -X linker override path
// compiles and actually takes effect. We rebuild the binary in a temp
// dir with an injected defaultServer, then run `polar-agent help`
// and grep for the injected URL — the help text echoes the default,
// so it's a clean black-box assertion.
//
// Skipped on `-short` because it spawns a `go build`.
func TestDefaultServerLdflagOverride(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping ldflag-override build in short mode")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not on PATH")
	}

	const injected = "https://fork.example.test:9443"
	outDir := t.TempDir()
	binName := "polar-agent-ldflag-test"
	if runtime.GOOS == "windows" {
		binName += ".exe"
	}
	outPath := filepath.Join(outDir, binName)

	// Build from the package dir (".") — go test sets cwd to the
	// package, so this resolves to cmd/polar-agent.
	cmd := exec.Command("go", "build",
		"-ldflags", "-X main.defaultServer="+injected,
		"-o", outPath,
		".")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build with -ldflags override failed: %v\n%s", err, out)
	}

	// `help` prints usage to stderr; capture both streams.
	helpCmd := exec.Command(outPath, "help")
	out, _ := helpCmd.CombinedOutput()
	if strings.Contains(string(out), injected) {
		return
	}
	// Fallback: `login` with no args also echoes the default in its
	// usage error.
	loginCmd := exec.Command(outPath, "login")
	out2, _ := loginCmd.CombinedOutput()
	if strings.Contains(string(out2), injected) {
		return
	}
	t.Fatalf("ldflag-injected defaultServer %q not visible in built binary output:\nhelp:\n%s\nlogin:\n%s",
		injected, out, out2)
}
