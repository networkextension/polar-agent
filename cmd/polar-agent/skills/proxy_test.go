//go:build unix

package skills

// Proxy skill unit tests. The actual sing-box binary isn't required
// — tests that need a running subprocess skip if it's not on PATH.
// Tests that don't (Kind/Version/Validate) always run.

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func singboxAvailable(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("sing-box"); err != nil {
		t.Skip("sing-box not on PATH; skipping proxy spawn test")
	}
}

func TestProxySkillKindAndVersion(t *testing.T) {
	sk := NewProxySkill()
	if sk == nil {
		t.Skip("non-unix")
	}
	if sk.Kind() != KindProxy {
		t.Errorf("Kind: got %q, want %q", sk.Kind(), KindProxy)
	}
	if sk.Version() == "" {
		t.Error("Version: empty")
	}
}

func TestProxySkillCapabilities(t *testing.T) {
	sk := NewProxySkill()
	if sk == nil {
		t.Skip("non-unix")
	}
	caps := sk.Capabilities()
	if caps["backend"] != "sing-box" {
		t.Errorf("backend: got %v, want sing-box", caps["backend"])
	}
	if _, ok := caps["installed"].(bool); !ok {
		t.Error("Capabilities.installed must be a bool (true if sing-box on PATH)")
	}
}

func TestProxySkillValidate(t *testing.T) {
	sk := NewProxySkill()
	if sk == nil {
		t.Skip("non-unix")
	}
	// Empty / null = valid (skill defaults later, though Start
	// will reject — Validate is the syntax check).
	if err := sk.Validate(nil); err != nil {
		t.Errorf("nil: %v", err)
	}
	if err := sk.Validate(json.RawMessage("null")); err != nil {
		t.Errorf("null: %v", err)
	}
	// Missing config_json.
	if err := sk.Validate(json.RawMessage(`{}`)); err == nil {
		t.Error("missing config_json: want error")
	}
	// Non-JSON inside config_json (it's a json.RawMessage; we
	// validate the inner blob too).
	bad := json.RawMessage(`{"config_json":"not-json"}`)
	if err := sk.Validate(bad); err == nil {
		t.Error("bad inner json: want error")
	}
	// Valid (tiny sing-box config — just a log block).
	good := json.RawMessage(`{"config_json":{"log":{"level":"info"}}}`)
	if err := sk.Validate(good); err != nil {
		t.Errorf("valid: %v", err)
	}
}

func TestProxySkillValidateOversized(t *testing.T) {
	sk := NewProxySkill()
	if sk == nil {
		t.Skip("non-unix")
	}
	// 300KiB > 256KiB cap.
	big := strings.Repeat(`"x":"y",`, 40000)
	cfg := []byte(`{"config_json":{` + big + `"end":1}}`)
	if err := sk.Validate(json.RawMessage(cfg)); err == nil {
		t.Error("oversize: want error")
	}
}

func TestProxySkillStartWithoutSingbox(t *testing.T) {
	// If sing-box ISN'T on PATH this test verifies the friendly error.
	// If it IS, skip (we'd actually start a process).
	if _, err := exec.LookPath("sing-box"); err == nil {
		t.Skip("sing-box installed; this test verifies the no-binary path")
	}
	sk := NewProxySkill()
	if sk == nil {
		t.Skip("non-unix")
	}
	_, err := sk.Start(context.Background(), 1, json.RawMessage(`{"config_json":{"log":{"level":"info"}}}`))
	if err == nil {
		t.Fatal("expected error when sing-box not on PATH")
	}
	if !strings.Contains(err.Error(), "sing-box") {
		t.Errorf("error should mention sing-box: %v", err)
	}
}

func TestProxySkillStartStop(t *testing.T) {
	singboxAvailable(t)
	sk := NewProxySkill()
	if sk == nil {
		t.Skip("non-unix")
	}
	// Minimal valid sing-box config (no inbounds → exits cleanly).
	// We're not testing actual proxying here; just the lifecycle.
	cfg := json.RawMessage(`{"config_json":{"log":{"level":"info","disabled":false},"experimental":{"clash_api":{"external_controller":"127.0.0.1:0"}}},"listen_addr_hint":"none"}`)
	// Wrap inner JSON correctly.
	cfg = json.RawMessage(`{"config_json":{"log":{"level":"info","disabled":false}},"listen_addr_hint":"127.0.0.1:0"}`)
	run, err := sk.Start(context.Background(), 7, cfg)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer run.Stop("test_cleanup")

	// Expect at least the initial state event.
	deadline := time.After(3 * time.Second)
	sawState := false
loop:
	for {
		select {
		case ev, ok := <-run.Events():
			if !ok {
				break loop
			}
			if ev.Kind == EventState {
				sawState = true
				if status, _ := ev.Data["status"].(string); status != "running" {
					t.Errorf("state.status: got %v, want running", ev.Data["status"])
				}
			}
		case <-deadline:
			break loop
		}
	}
	if !sawState {
		t.Error("never saw state event")
	}

	// Stop should be idempotent.
	if err := run.Stop("first"); err != nil {
		t.Errorf("first Stop: %v", err)
	}
	if err := run.Stop("second"); err != nil {
		t.Errorf("second Stop: %v", err)
	}

	// Drain remaining events until close (or timeout).
	deadline2 := time.After(3 * time.Second)
	for {
		select {
		case _, ok := <-run.Events():
			if !ok {
				return
			}
		case <-deadline2:
			t.Fatal("events channel never closed after Stop")
		}
	}
}
