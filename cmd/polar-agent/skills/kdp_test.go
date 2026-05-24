//go:build unix

package skills

// KDP skill unit tests. The real serial path needs a device;
// these tests validate the config + the agent-side `?` command
// dispatcher in isolation (without opening a port).

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func TestKDPSkillKindAndVersion(t *testing.T) {
	sk := NewKDPSkill()
	if sk == nil {
		t.Skip("non-unix")
	}
	if sk.Kind() != KindKDP {
		t.Errorf("Kind: got %q, want %q", sk.Kind(), KindKDP)
	}
	if sk.Version() == "" {
		t.Error("Version: empty")
	}
}

func TestKDPSkillCapabilities(t *testing.T) {
	sk := NewKDPSkill()
	if sk == nil {
		t.Skip("non-unix")
	}
	caps := sk.Capabilities()
	if caps["protocol"] != "pty-bytes" {
		t.Errorf("protocol: got %v, want pty-bytes", caps["protocol"])
	}
	if caps["default_baud"] != 115200 {
		t.Errorf("default_baud: got %v, want 115200", caps["default_baud"])
	}
	if caps["command_prefix"] != "?" {
		t.Errorf("command_prefix: got %v, want ?", caps["command_prefix"])
	}
}

func TestKDPSkillValidate(t *testing.T) {
	sk := NewKDPSkill()
	if sk == nil {
		t.Skip("non-unix")
	}
	if err := sk.Validate(nil); err != nil {
		t.Errorf("nil: %v", err)
	}
	if err := sk.Validate(json.RawMessage("null")); err != nil {
		t.Errorf("null: %v", err)
	}
	// Missing device_path.
	if err := sk.Validate(json.RawMessage(`{}`)); err == nil {
		t.Error("missing device_path: want error")
	}
	// Relative path → reject (defense-in-depth).
	if err := sk.Validate(json.RawMessage(`{"device_path":"ttyUSB0"}`)); err == nil {
		t.Error("relative path: want error")
	}
	// Insane baud.
	if err := sk.Validate(json.RawMessage(`{"device_path":"/dev/x","baud":99999999}`)); err == nil {
		t.Error("oversized baud: want error")
	}
	// Valid.
	if err := sk.Validate(json.RawMessage(`{"device_path":"/dev/ttyUSB0","baud":115200}`)); err != nil {
		t.Errorf("valid: %v", err)
	}
}

func TestKDPSkillStartWithoutDevice(t *testing.T) {
	sk := NewKDPSkill()
	if sk == nil {
		t.Skip("non-unix")
	}
	// /dev/null-but-not-serial: opening succeeds on some unixes,
	// fails on others. Use a path that's guaranteed not to exist.
	cfg := json.RawMessage(`{"device_path":"/dev/polar-kdp-nonexistent-device","baud":115200}`)
	_, err := sk.Start(context.Background(), 1, cfg)
	if err == nil {
		t.Fatal("expected error opening nonexistent serial device")
	}
	if !strings.Contains(err.Error(), "/dev/polar-kdp-nonexistent-device") {
		t.Errorf("error should mention the device path: %v", err)
	}
}

func TestKDPDispatchLineBuiltins(t *testing.T) {
	// Test the `?` command parser without a real serial device. We
	// construct a kdpRun manually with a nil port + a buffered
	// events channel, then call runBuiltin directly.
	run := &kdpRun{
		id:     42,
		cfg:    KDPConfig{DevicePath: "/dev/test", Baud: 115200},
		events: make(chan Event, 16),
		stopCh: make(chan struct{}),
	}

	drain := func(t *testing.T) string {
		t.Helper()
		var sb strings.Builder
		// Drain anything already queued; don't block.
		for {
			select {
			case ev := <-run.events:
				if ev.Kind != EventStdout {
					continue
				}
				b64, _ := ev.Data["bytes_b64"].(string)
				raw, _ := base64.StdEncoding.DecodeString(b64)
				sb.Write(raw)
			default:
				return sb.String()
			}
		}
	}

	run.runBuiltin("?help")
	out := drain(t)
	if !strings.Contains(out, "builtin commands") {
		t.Errorf("?help: missing 'builtin commands' in %q", out)
	}
	if !strings.Contains(out, "?help") || !strings.Contains(out, "?info") {
		t.Errorf("?help: command list incomplete: %q", out)
	}

	run.runBuiltin("?info")
	out = drain(t)
	if !strings.Contains(out, "device=/dev/test") || !strings.Contains(out, "baud=115200") {
		t.Errorf("?info: missing fields in %q", out)
	}

	run.runBuiltin("?nopesuch")
	out = drain(t)
	if !strings.Contains(out, "unknown command: ?nopesuch") {
		t.Errorf("?nopesuch: %q", out)
	}

	run.runBuiltin("?")
	out = drain(t)
	if !strings.Contains(out, "empty command") && !strings.Contains(out, "?help") {
		// ? alone matches the empty-cmd branch OR the help alias depending on impl
		t.Errorf("? alone: %q", out)
	}
}

func TestKDPResizeIsNoop(t *testing.T) {
	run := &kdpRun{}
	if err := run.Resize(40, 120); err != nil {
		t.Errorf("Resize: want nil err (serial has no winsize), got %v", err)
	}
}
