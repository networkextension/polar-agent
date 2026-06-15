package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// TestRegisterResponseParsesProxyFields proves the register response decoder
// picks up the P2 proxy creds (and tolerates their absence).
func TestRegisterResponseParsesProxyFields(t *testing.T) {
	body := `{"agent_id":"ag_1","host_id":"h_1","bot_user_id":"bot_1","agent_token_raw":"polar_agent_raw",
	          "proxy_token":"polar_proxy_z","proxy_base_url":"https://dock/api/proxy/v1","default_model":"cfg-9"}`
	var r registerResponse
	if err := json.Unmarshal([]byte(body), &r); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if r.ProxyToken != "polar_proxy_z" || r.ProxyBaseURL != "https://dock/api/proxy/v1" || r.DefaultModel != "cfg-9" {
		t.Errorf("proxy fields not parsed: %+v", r)
	}

	// Pre-P2 server: fields simply absent, no error.
	var legacy registerResponse
	if err := json.Unmarshal([]byte(`{"agent_id":"ag_2","agent_token_raw":"x"}`), &legacy); err != nil {
		t.Fatalf("legacy unmarshal: %v", err)
	}
	if legacy.ProxyToken != "" {
		t.Errorf("legacy ProxyToken should be empty, got %q", legacy.ProxyToken)
	}
}

// TestAgentConfigRoundTripProxy proves the P2 per-agent proxy creds persist
// to agent.toml and parse back, and stay omitted when empty.
func TestAgentConfigRoundTripProxy(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "agent.toml")
	t.Setenv("POLAR_AGENT_CONFIG", cfgPath)

	want := AgentConfig{
		Server:       "https://zen.4950.store:2443",
		Token:        "polar_agent_v4",
		AgentID:      "ag_a1b2c3d4e5f6789012345678901234ab",
		ProxyToken:   "polar_proxy_deadbeef",
		ProxyBaseURL: "https://zen.4950.store:2443/api/proxy/v1",
		DefaultModel: "cfg-7",
	}
	if err := want.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := LoadAgentConfig()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got != want {
		t.Errorf("round-trip mismatch:\n got: %+v\nwant: %+v", got, want)
	}
	if !got.hasProxyCreds() {
		t.Errorf("hasProxyCreds = false for fully-populated creds")
	}

	// Empty proxy creds must be omitted from the file.
	noProxy := AgentConfig{Server: "https://x", Token: "polar_agent_x"}
	if err := noProxy.Save(); err != nil {
		t.Fatalf("save no-proxy: %v", err)
	}
	if noProxy.hasProxyCreds() {
		t.Errorf("hasProxyCreds = true with empty creds")
	}
	raw, _ := LoadAgentConfig()
	if raw.ProxyToken != "" || raw.ProxyBaseURL != "" || raw.DefaultModel != "" {
		t.Errorf("empty proxy creds leaked: %+v", raw)
	}
}

// TestProxyLLMClient verifies the client is built only when creds exist.
func TestProxyLLMClient(t *testing.T) {
	none := AgentConfig{Server: "s", Token: "t"}
	if _, _, ok := none.proxyLLMClient(); ok {
		t.Errorf("proxyLLMClient ok=true without creds")
	}

	c := AgentConfig{
		Server: "s", Token: "t",
		ProxyToken:   "polar_proxy_x",
		ProxyBaseURL: "https://dock/api/proxy/v1",
		DefaultModel: "cfg-3",
	}
	client, model, ok := c.proxyLLMClient()
	if !ok || client == nil {
		t.Fatalf("proxyLLMClient ok=%v client=%v", ok, client)
	}
	if model != "cfg-3" {
		t.Errorf("default model = %q want cfg-3", model)
	}
	if client.baseURL != "https://dock/api/proxy/v1" || client.apiKey != "polar_proxy_x" {
		t.Errorf("client wired wrong: base=%q key=%q", client.baseURL, client.apiKey)
	}
}

// TestFetchProxyModels hits a fake OpenAI-shaped /models endpoint and checks
// the Bearer token + parse.
func TestFetchProxyModels(t *testing.T) {
	var gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"cfg-7"},{"id":"qwen-max"},{"id":""}]}`))
	}))
	defer srv.Close()

	cfg := AgentConfig{
		Server: "s", Token: "t",
		ProxyToken:   "polar_proxy_abc",
		ProxyBaseURL: srv.URL + "/api/proxy/v1",
	}
	models, err := fetchProxyModels(context.Background(), cfg)
	if err != nil {
		t.Fatalf("fetchProxyModels: %v", err)
	}
	if gotPath != "/api/proxy/v1/models" {
		t.Errorf("hit %q want /api/proxy/v1/models", gotPath)
	}
	if gotAuth != "Bearer polar_proxy_abc" {
		t.Errorf("auth = %q want Bearer polar_proxy_abc", gotAuth)
	}
	if len(models) != 2 || models[0] != "cfg-7" || models[1] != "qwen-max" {
		t.Errorf("models = %v want [cfg-7 qwen-max] (empty id dropped)", models)
	}
}

// TestFetchProxyModelsNoCreds — pre-P2 config errors cleanly, no panic.
func TestFetchProxyModelsNoCreds(t *testing.T) {
	_, err := fetchProxyModels(context.Background(), AgentConfig{Server: "s", Token: "t"})
	if err == nil || !strings.Contains(err.Error(), "pre-P2") {
		t.Errorf("want pre-P2 error, got %v", err)
	}
}
