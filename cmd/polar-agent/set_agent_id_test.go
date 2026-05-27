package main

import (
	"path/filepath"
	"testing"
)

func TestValidateAgentID(t *testing.T) {
	good := []string{
		"ag_0123456789abcdef0123456789abcdef",
		"ag_a1b2c3d4e5f60718293a4b5c6d7e8f90",
		"ag_FFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFF",
	}
	for _, id := range good {
		if err := validateAgentID(id); err != nil {
			t.Errorf("validateAgentID(%q): unexpected error %v", id, err)
		}
	}
	bad := []string{
		"",
		"bot_xxx",
		"ag_short",
		"ag_0123456789abcdef0123456789abcdezz", // non-hex (z)
		"AG_0123456789abcdef0123456789abcdef",  // wrong-case prefix
		"ag_0123456789abcdef0123456789abcdef0", // 33 chars
	}
	for _, id := range bad {
		if err := validateAgentID(id); err == nil {
			t.Errorf("validateAgentID(%q): expected error, got nil", id)
		}
	}
}

func TestValidateBotUserID(t *testing.T) {
	if err := validateBotUserID("bot_some_bot"); err != nil {
		t.Errorf("good bot id rejected: %v", err)
	}
	for _, bad := range []string{"", "ag_xxx", "bot_", "BOT_xxx"} {
		if err := validateBotUserID(bad); err == nil {
			t.Errorf("validateBotUserID(%q): expected error", bad)
		}
	}
}

// TestSetAgentID_WritesNewFields seeds a legacy 2-field agent.toml,
// runs `set-agent-id`, and confirms the file gains agent_id +
// bot_user_id (and reload sees them).
func TestSetAgentID_WritesNewFields(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "agent.toml")
	t.Setenv("POLAR_AGENT_CONFIG", cfgPath)

	// Seed legacy config
	legacy := AgentConfig{
		Server: "https://zen.4950.store:2443",
		Token:  "polar_agent_legacy",
	}
	if err := legacy.Save(); err != nil {
		t.Fatalf("seed: %v", err)
	}

	code := runSetAgentID([]string{
		"ag_0123456789abcdef0123456789abcdef",
		"bot_some_bot",
	})
	if code != exitOK {
		t.Fatalf("runSetAgentID exit=%d", code)
	}

	got, err := LoadAgentConfig()
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got.AgentID != "ag_0123456789abcdef0123456789abcdef" {
		t.Errorf("agent_id = %q", got.AgentID)
	}
	if got.BotUserID != "bot_some_bot" {
		t.Errorf("bot_user_id = %q", got.BotUserID)
	}
	// Server + token preserved
	if got.Server != "https://zen.4950.store:2443" || got.Token != "polar_agent_legacy" {
		t.Errorf("base fields clobbered: %+v", got)
	}
}

// TestSetAgentID_OnlyAgentID — operator might know agent_id but not
// the bot yet (will fetch via dashboard later). Should still save and
// leave BotUserID empty.
func TestSetAgentID_OnlyAgentID(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("POLAR_AGENT_CONFIG", filepath.Join(dir, "agent.toml"))

	legacy := AgentConfig{Server: "https://x.example", Token: "tok"}
	if err := legacy.Save(); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if code := runSetAgentID([]string{"ag_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}); code != exitOK {
		t.Fatalf("exit=%d", code)
	}
	got, _ := LoadAgentConfig()
	if got.AgentID != "ag_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Errorf("agent_id = %q", got.AgentID)
	}
	if got.BotUserID != "" {
		t.Errorf("BotUserID should be empty, got %q", got.BotUserID)
	}
}

// TestSetAgentID_NoChangeIsIdempotent — running the command twice
// with the same id is a no-op (returns OK, doesn't error).
func TestSetAgentID_NoChangeIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("POLAR_AGENT_CONFIG", filepath.Join(dir, "agent.toml"))

	cfg := AgentConfig{
		Server:    "https://x.example",
		Token:     "tok",
		AgentID:   "ag_0123456789abcdef0123456789abcdef",
		BotUserID: "bot_x",
	}
	if err := cfg.Save(); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if code := runSetAgentID([]string{"ag_0123456789abcdef0123456789abcdef", "bot_x"}); code != exitOK {
		t.Fatalf("exit=%d", code)
	}
}

// TestSetAgentID_RejectsBadInput exercises the usage/validation gates.
func TestSetAgentID_RejectsBadInput(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("POLAR_AGENT_CONFIG", filepath.Join(dir, "agent.toml"))
	legacy := AgentConfig{Server: "https://x.example", Token: "tok"}
	if err := legacy.Save(); err != nil {
		t.Fatalf("seed: %v", err)
	}
	cases := [][]string{
		{},                                    // missing arg
		{"bot_wrong_prefix"},                  // wrong prefix
		{"ag_short"},                          // too short
		{"ag_0123456789abcdef0123456789abcdef", "ag_wrong_2nd_prefix"}, // bad bot id
		{"ag_a", "bot_x", "extra"},            // too many args
	}
	for _, args := range cases {
		if code := runSetAgentID(args); code != exitUsage {
			t.Errorf("runSetAgentID(%v) = %d, want exitUsage", args, code)
		}
	}
}
