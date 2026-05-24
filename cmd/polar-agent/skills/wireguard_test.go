//go:build unix

package skills

// WireGuard skill unit tests. Validates the config schema + the
// no-binary error path. Doesn't exercise actual wg-quick (needs
// root); the live smoke happens on a properly-provisioned host.

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
)

func TestWireGuardSkillKindAndVersion(t *testing.T) {
	sk := NewWireGuardSkill()
	if sk == nil {
		t.Skip("non-unix")
	}
	if sk.Kind() != KindWireGuard {
		t.Errorf("Kind: got %q, want %q", sk.Kind(), KindWireGuard)
	}
	if sk.Version() == "" {
		t.Error("Version: empty")
	}
}

func TestWireGuardSkillCapabilities(t *testing.T) {
	sk := NewWireGuardSkill()
	if sk == nil {
		t.Skip("non-unix")
	}
	caps := sk.Capabilities()
	if caps["backend"] != "wg-quick" {
		t.Errorf("backend: got %v, want wg-quick", caps["backend"])
	}
	if _, ok := caps["installed"].(bool); !ok {
		t.Error("Capabilities.installed must be a bool")
	}
	if caps["config_dir"] != "/etc/wireguard" {
		t.Errorf("config_dir: got %v, want /etc/wireguard", caps["config_dir"])
	}
}

func TestWireGuardSkillValidate(t *testing.T) {
	sk := NewWireGuardSkill()
	if sk == nil {
		t.Skip("non-unix")
	}
	// Nil/null = valid (Start will reject; Validate is syntax-only).
	if err := sk.Validate(nil); err != nil {
		t.Errorf("nil: %v", err)
	}
	if err := sk.Validate(json.RawMessage("null")); err != nil {
		t.Errorf("null: %v", err)
	}
	// Missing config_text.
	if err := sk.Validate(json.RawMessage(`{"interface_name":"wg0"}`)); err == nil {
		t.Error("missing config_text: want error")
	}
	// Missing [Interface] section.
	bad := json.RawMessage(`{"config_text":"hello\n"}`)
	if err := sk.Validate(bad); err == nil {
		t.Error("no [Interface] section: want error")
	}
	// Missing PrivateKey.
	noKey := json.RawMessage(`{"config_text":"[Interface]\nAddress = 10.0.0.1/24\n"}`)
	if err := sk.Validate(noKey); err == nil {
		t.Error("no PrivateKey: want error")
	}
	// Valid minimal config.
	good := json.RawMessage(`{"config_text":"[Interface]\nPrivateKey = abc=\nAddress = 10.0.0.1/24\n"}`)
	if err := sk.Validate(good); err != nil {
		t.Errorf("valid: %v", err)
	}
}

func TestWireGuardSkillValidateInterfaceName(t *testing.T) {
	sk := NewWireGuardSkill()
	if sk == nil {
		t.Skip("non-unix")
	}
	good := `[Interface]
PrivateKey = abc=
Address = 10.0.0.1/24
`
	for _, badName := range []string{
		"wg0/etc",   // slash → escape attempt
		"wg 0",      // space
		"1wg",       // leading digit
		"wg-0123456789012345", // too long (>15 chars including separator)
		"../etc",
	} {
		cfg := map[string]any{"interface_name": badName, "config_text": good}
		raw, _ := json.Marshal(cfg)
		if err := sk.Validate(raw); err == nil {
			t.Errorf("interface_name=%q: want error", badName)
		}
	}
	for _, goodName := range []string{"wg0", "wg-research", "polar_wg"} {
		cfg := map[string]any{"interface_name": goodName, "config_text": good}
		raw, _ := json.Marshal(cfg)
		if err := sk.Validate(raw); err != nil {
			t.Errorf("interface_name=%q: %v", goodName, err)
		}
	}
}

func TestWireGuardSkillStartWithoutBinary(t *testing.T) {
	if _, err := exec.LookPath("wg-quick"); err == nil {
		t.Skip("wg-quick installed; this test verifies the no-binary path")
	}
	sk := NewWireGuardSkill()
	if sk == nil {
		t.Skip("non-unix")
	}
	cfg := json.RawMessage(`{"config_text":"[Interface]\nPrivateKey = abc=\nAddress = 10.0.0.1/24\n"}`)
	_, err := sk.Start(context.Background(), 1, cfg)
	if err == nil {
		t.Fatal("expected error when wg-quick not on PATH")
	}
	if !strings.Contains(err.Error(), "wg-quick") {
		t.Errorf("error should mention wg-quick: %v", err)
	}
}

func TestWireGuardSkillStartValidatesInterfaceName(t *testing.T) {
	// Even when wg-quick IS installed, an invalid interface name
	// should fail before any subprocess work — and before we touch
	// /etc/wireguard, which would require root.
	if _, err := exec.LookPath("wg-quick"); err != nil {
		t.Skip("wg-quick not installed; can't distinguish validate-error from no-binary-error")
	}
	sk := NewWireGuardSkill()
	if sk == nil {
		t.Skip("non-unix")
	}
	cfg := json.RawMessage(`{"interface_name":"bad name","config_text":"[Interface]\nPrivateKey = abc=\nAddress = 10.0.0.1/24\n"}`)
	_, err := sk.Start(context.Background(), 1, cfg)
	if err == nil {
		t.Fatal("expected validation error for bad interface_name")
	}
	if !strings.Contains(err.Error(), "interface_name") {
		t.Errorf("error should mention interface_name: %v", err)
	}
}
