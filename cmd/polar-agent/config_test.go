package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAgentConfigRoundTripLegacy proves a 2-field (v3) agent.toml
// round-trips clean: Save writes only server+token, Load reads them
// back, AgentID + BotUserID stay empty.
func TestAgentConfigRoundTripLegacy(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "agent.toml")
	t.Setenv("POLAR_AGENT_CONFIG", cfgPath)

	want := AgentConfig{
		Server: "https://zen.4950.store:2443",
		Token:  "polar_agent_legacy",
	}
	if err := want.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	body := string(raw)
	if !strings.Contains(body, `server = "https://zen.4950.store:2443"`) {
		t.Errorf("server line missing: %s", body)
	}
	if !strings.Contains(body, `token = "polar_agent_legacy"`) {
		t.Errorf("token line missing: %s", body)
	}
	if strings.Contains(body, "agent_id") {
		t.Errorf("empty AgentID must be omitted; got: %s", body)
	}
	if strings.Contains(body, "bot_user_id") {
		t.Errorf("empty BotUserID must be omitted; got: %s", body)
	}

	got, err := LoadAgentConfig()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got != want {
		t.Errorf("round-trip mismatch:\n got: %+v\nwant: %+v", got, want)
	}
}

// TestAgentConfigRoundTripV4 proves the full 4-field v4 agent.toml
// round-trips, including persistence on disk and parse-back.
func TestAgentConfigRoundTripV4(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "agent.toml")
	t.Setenv("POLAR_AGENT_CONFIG", cfgPath)

	want := AgentConfig{
		Server:    "https://zen.4950.store:2443",
		Token:     "polar_agent_v4",
		AgentID:   "ag_a1b2c3d4e5f6789012345678901234ab",
		BotUserID: "bot_some_bot",
	}
	if err := want.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	body := string(raw)
	for _, expect := range []string{
		`server = "https://zen.4950.store:2443"`,
		`token = "polar_agent_v4"`,
		`agent_id = "ag_a1b2c3d4e5f6789012345678901234ab"`,
		`bot_user_id = "bot_some_bot"`,
	} {
		if !strings.Contains(body, expect) {
			t.Errorf("missing %q in:\n%s", expect, body)
		}
	}

	got, err := LoadAgentConfig()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got != want {
		t.Errorf("round-trip mismatch:\n got: %+v\nwant: %+v", got, want)
	}
}

// TestAgentConfigLoadToleratesUnknownKeys — older or newer files might
// carry keys this build doesn't understand; loader must skip them and
// still parse the fields it knows.
func TestAgentConfigLoadToleratesUnknownKeys(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "agent.toml")
	t.Setenv("POLAR_AGENT_CONFIG", cfgPath)

	body := `# legacy install
server = "https://zen.4950.store:2443"
token = "polar_agent_x"
some_future_field = "yes"
# bot_user_id = "bot_disabled_via_comment"
agent_id = "ag_0123456789abcdef0123456789abcdef"
`
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	got, err := LoadAgentConfig()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.Server != "https://zen.4950.store:2443" || got.Token != "polar_agent_x" {
		t.Errorf("base fields wrong: %+v", got)
	}
	if got.AgentID != "ag_0123456789abcdef0123456789abcdef" {
		t.Errorf("AgentID wrong: %q", got.AgentID)
	}
	if got.BotUserID != "" {
		t.Errorf("commented bot_user_id leaked through: %q", got.BotUserID)
	}
}
