package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRegister_PersistsV4Fields verifies that a v4 register response
// (agent_id + host_id + bot_user_id + agent_token_raw + server) lands
// in agent.toml end-to-end. We stub the polar-hosts endpoint with an
// httptest server and call runRegister directly.
func TestRegister_PersistsV4Fields(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "agent.toml")
	t.Setenv("POLAR_AGENT_CONFIG", cfgPath)

	var bodyRaw []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/hosts/register" {
			http.NotFound(w, r)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer enroll_token_abc" {
			t.Errorf("auth header: got %q", got)
		}
		bodyRaw, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"agent_id":        "ag_1111111111111111111111111111aaaa",
			"host_id":         "h_test_host",
			"bot_user_id":     "bot_auto_created",
			"agent_token_raw": "polar_agent_RAW",
			"server":          "https://zen.4950.store:2443",
			"workspace_id":    "team_test",
		})
	}))
	defer srv.Close()

	code := runRegister([]string{
		"--server=" + srv.URL,
		"--token=enroll_token_abc",
		"--name=test-box",
	})
	if code != exitOK {
		t.Fatalf("runRegister exit=%d", code)
	}

	// Body assertions: machine_uuid_raw + host_info present (host_info
	// may be empty on platforms without a collector, that's fine).
	if len(bodyRaw) == 0 {
		t.Fatal("server saw empty body")
	}
	var sent map[string]any
	if err := json.Unmarshal(bodyRaw, &sent); err != nil {
		t.Fatalf("unmarshal sent body: %v\n%s", err, bodyRaw)
	}
	if _, ok := sent["host_info"]; !ok {
		t.Errorf("register body missing host_info: %s", bodyRaw)
	}
	// On platforms where the collector returns no UUID, the field is
	// omitempty — we don't assert presence, just absence of the legacy
	// `machine_uuid` field name.
	if _, ok := sent["machine_uuid"]; ok {
		t.Errorf("register body still emitting legacy machine_uuid (should be machine_uuid_raw): %s", bodyRaw)
	}

	got, err := LoadAgentConfig()
	if err != nil {
		t.Fatalf("load saved config: %v", err)
	}
	if got.Server != srv.URL {
		t.Errorf("server = %q, want %q", got.Server, srv.URL)
	}
	if got.Token != "polar_agent_RAW" {
		t.Errorf("token = %q", got.Token)
	}
	if got.AgentID != "ag_1111111111111111111111111111aaaa" {
		t.Errorf("agent_id = %q", got.AgentID)
	}
	if got.BotUserID != "bot_auto_created" {
		t.Errorf("bot_user_id = %q", got.BotUserID)
	}

	// File on disk must have agent_id + bot_user_id lines so a human
	// can audit / diff.
	raw, _ := os.ReadFile(cfgPath)
	for _, want := range []string{"agent_id =", "bot_user_id ="} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("agent.toml missing %q:\n%s", want, raw)
		}
	}
}

// TestRegister_LegacyServerOmitsV4Fields covers the rollback path
// where polar-hosts is pre-v4 (returns agent_token_raw but no
// agent_id / bot_user_id). agent.toml should still save cleanly —
// just without the new fields.
func TestRegister_LegacyServerOmitsV4Fields(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "agent.toml")
	t.Setenv("POLAR_AGENT_CONFIG", cfgPath)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"agent_token_raw": "polar_agent_legacy",
			// no agent_id, no bot_user_id, no host_id
		})
	}))
	defer srv.Close()

	code := runRegister([]string{
		"--server=" + srv.URL,
		"--token=enroll_legacy",
	})
	if code != exitOK {
		t.Fatalf("runRegister exit=%d", code)
	}
	got, err := LoadAgentConfig()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.AgentID != "" || got.BotUserID != "" {
		t.Errorf("v4 fields leaked from legacy response: %+v", got)
	}
	if got.Token != "polar_agent_legacy" {
		t.Errorf("token = %q", got.Token)
	}
}
