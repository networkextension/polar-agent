//go:build unix

package skills

// Tests for the Shell skill. Unix-only — the skill itself is
// //go:build unix. Each test spawns a real pty + bash subprocess.
// They're skipped automatically on machines without `bash` or `sh`
// on PATH (e.g. some minimal CI containers).

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func shellAvailable(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err == nil {
		return
	}
	if _, err := exec.LookPath("sh"); err == nil {
		return
	}
	t.Skip("no bash or sh on PATH; skipping shell skill test")
}

func TestShellSkillKindAndVersion(t *testing.T) {
	sk := NewShellSkill("/")
	if sk == nil {
		t.Skip("NewShellSkill returned nil (non-unix build?)")
	}
	if sk.Kind() != KindShell {
		t.Errorf("Kind: got %q, want %q", sk.Kind(), KindShell)
	}
	if sk.Version() == "" {
		t.Error("Version: empty")
	}
	caps := sk.Capabilities()
	if caps["protocol"] != "pty-bytes" {
		t.Errorf("Capabilities.protocol: got %v, want pty-bytes", caps["protocol"])
	}
}

func TestShellSkillValidate(t *testing.T) {
	sk := NewShellSkill("/")
	if sk == nil {
		t.Skip("non-unix")
	}
	// Empty config = valid (use defaults).
	if err := sk.Validate(nil); err != nil {
		t.Errorf("empty config: %v", err)
	}
	if err := sk.Validate(json.RawMessage("null")); err != nil {
		t.Errorf("null: %v", err)
	}
	// Oversize rows.
	if err := sk.Validate(json.RawMessage(`{"rows":99999}`)); err == nil {
		t.Error("oversize rows: want error")
	}
	// Idle timeout too large.
	if err := sk.Validate(json.RawMessage(`{"idle_timeout_sec":99999}`)); err == nil {
		t.Error("oversize idle: want error")
	}
	// Banned env key.
	if err := sk.Validate(json.RawMessage(`{"env":{"LD_PRELOAD":"x"}}`)); err == nil {
		t.Error("LD_PRELOAD: want error")
	}
	// Bad env key (contains '=').
	if err := sk.Validate(json.RawMessage(`{"env":{"BAD=KEY":"x"}}`)); err == nil {
		t.Error("env key with '=': want error")
	}
}

func TestShellSkillStartEchoExit(t *testing.T) {
	shellAvailable(t)
	sk := NewShellSkill(os.TempDir())
	if sk == nil {
		t.Skip("non-unix")
	}
	cfg := json.RawMessage(`{"rows":24,"cols":80,"idle_timeout_sec":60}`)
	run, err := sk.Start(context.Background(), 1, cfg)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer run.Stop("test_cleanup")

	// Type "exit\n" — bash should terminate, we should see an exit event.
	input, ok := run.(RunInput)
	if !ok {
		t.Fatal("Run doesn't satisfy RunInput")
	}
	if err := input.WriteInput([]byte("exit\n")); err != nil {
		t.Fatalf("WriteInput: %v", err)
	}

	deadline := time.After(5 * time.Second)
	sawState := false
	sawStdout := false
	for {
		select {
		case ev, ok := <-run.Events():
			if !ok {
				if !sawState {
					t.Error("events channel closed without state event")
				}
				if !sawStdout {
					t.Error("events channel closed without any stdout (bash didn't print prompt?)")
				}
				return
			}
			switch ev.Kind {
			case EventState:
				sawState = true
				if status, _ := ev.Data["status"].(string); status != "running" {
					t.Errorf("state event status=%v, want running", ev.Data["status"])
				}
			case EventStdout:
				sawStdout = true
				// Validate base64.
				b64, _ := ev.Data["bytes_b64"].(string)
				if b64 == "" {
					t.Error("stdout event has empty bytes_b64")
				}
				if _, err := base64.StdEncoding.DecodeString(b64); err != nil {
					t.Errorf("stdout event bytes_b64 invalid base64: %v", err)
				}
			case EventExit:
				if _, ok := ev.Data["exit_code"]; !ok {
					t.Error("exit event missing exit_code")
				}
				if _, ok := ev.Data["reason"]; !ok {
					t.Error("exit event missing reason")
				}
			}
		case <-deadline:
			t.Fatalf("timed out waiting for exit event (sawState=%v sawStdout=%v)", sawState, sawStdout)
		}
	}
}

func TestShellSkillResize(t *testing.T) {
	shellAvailable(t)
	sk := NewShellSkill(os.TempDir())
	if sk == nil {
		t.Skip("non-unix")
	}
	run, err := sk.Start(context.Background(), 2, json.RawMessage(`{"rows":24,"cols":80,"idle_timeout_sec":60}`))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer run.Stop("test_cleanup")

	resizer, ok := run.(RunResizer)
	if !ok {
		t.Fatal("Run doesn't satisfy RunResizer")
	}
	if err := resizer.Resize(40, 120); err != nil {
		t.Errorf("Resize: %v", err)
	}
	// Resize to 0 should error (or at least not crash).
	if err := resizer.Resize(0, 0); err == nil {
		t.Error("Resize(0,0): want error")
	}
}

func TestShellSkillStopIdempotent(t *testing.T) {
	shellAvailable(t)
	sk := NewShellSkill(os.TempDir())
	if sk == nil {
		t.Skip("non-unix")
	}
	run, err := sk.Start(context.Background(), 3, json.RawMessage(`{"idle_timeout_sec":60}`))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := run.Stop("first"); err != nil {
		t.Errorf("first Stop: %v", err)
	}
	if err := run.Stop("second"); err != nil {
		t.Errorf("second Stop (should be idempotent): %v", err)
	}
	// Channel should be closed within a bounded window.
	deadline := time.After(3 * time.Second)
	for {
		select {
		case _, ok := <-run.Events():
			if !ok {
				return // closed, good
			}
		case <-deadline:
			t.Fatal("events channel never closed after Stop")
		}
	}
}

func TestBuildShellEnvStripsDangerKeys(t *testing.T) {
	t.Setenv("SHELL_TEST_KEEP", "yes")
	t.Setenv("LD_PRELOAD", "should_be_stripped")
	overrides := map[string]string{
		"FOO":         "bar",
		"LD_PRELOAD":  "from_override", // should ALSO be stripped
		"DYLD_INSERT_LIBRARIES": "x",   // should be stripped
	}
	env := buildShellEnv(overrides)
	for _, e := range env {
		if strings.HasPrefix(e, "LD_PRELOAD=") {
			t.Errorf("LD_PRELOAD leaked: %q", e)
		}
		if strings.HasPrefix(e, "DYLD_INSERT_LIBRARIES=") {
			t.Errorf("DYLD_INSERT_LIBRARIES leaked: %q", e)
		}
	}
	foundFoo := false
	foundKeep := false
	for _, e := range env {
		if e == "FOO=bar" {
			foundFoo = true
		}
		if e == "SHELL_TEST_KEEP=yes" {
			foundKeep = true
		}
	}
	if !foundFoo {
		t.Error("override FOO=bar missing")
	}
	if !foundKeep {
		t.Error("inherited SHELL_TEST_KEEP missing")
	}
}
