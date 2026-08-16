//go:build freebsd || linux

package main

// cloud_guest.go — polar-cloud guest side: compute-task skill `cloud.guest`,
// run by the polar-agent baked into VM images. Lets the control plane power a
// VM off cleanly (FreeBSD ignores the VZ ACPI power button, see
// polar-cloud/cmd/polar-vmd/README.md) without ssh keys.
//
// input: {"op":"poweroff|reboot|ping"}
// output: {"op","hostname","uptime_s"} — poweroff/reboot complete FIRST, then
// run the command ~1s later so dock records the task as done.

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

func registerCloudGuestHandler() {
	registerComputeHandler("cloud.guest", runCloudGuestTask)
}

func runCloudGuestTask(ctx context.Context, cfg AgentConfig, t computeTask) (any, error) {
	var in struct {
		Op string `json:"op"`
	}
	if err := json.Unmarshal(t.Input, &in); err != nil {
		return nil, fmt.Errorf("cloud.guest: bad input: %w", err)
	}
	op := strings.ToLower(strings.TrimSpace(in.Op))
	host, _ := os.Hostname()
	out := map[string]any{"op": op, "hostname": host, "os": runtime.GOOS}
	if up := guestUptimeSeconds(); up > 0 {
		out["uptime_s"] = up
	}
	switch op {
	case "ping":
		return out, nil
	case "poweroff", "reboot":
		var argv []string
		switch runtime.GOOS {
		case "freebsd":
			if op == "poweroff" {
				argv = []string{"/sbin/shutdown", "-p", "now"}
			} else {
				argv = []string{"/sbin/shutdown", "-r", "now"}
			}
		default: // linux
			if op == "poweroff" {
				argv = []string{"/bin/systemctl", "poweroff"}
			} else {
				argv = []string{"/bin/systemctl", "reboot"}
			}
		}
		// Deferred so completeComputeTask (called by the loop right after we
		// return) gets out first.
		go func() {
			time.Sleep(1500 * time.Millisecond)
			log.Printf("[cloud.guest] %s: exec %v", op, argv)
			cmd := exec.Command(argv[0], argv[1:]...)
			if b, err := cmd.CombinedOutput(); err != nil {
				log.Printf("[cloud.guest] %s failed: %v: %s", op, err, strings.TrimSpace(string(b)))
			}
		}()
		out["scheduled"] = true
		return out, nil
	default:
		return out, fmt.Errorf("cloud.guest: unknown op %q", op)
	}
}

func guestUptimeSeconds() int64 {
	if runtime.GOOS == "linux" {
		if b, err := os.ReadFile("/proc/uptime"); err == nil {
			var up float64
			fmt.Sscanf(string(b), "%f", &up)
			return int64(up)
		}
	}
	return 0
}
