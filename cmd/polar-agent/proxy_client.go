package main

// Per-agent LLM proxy client (P2 of doc/arch/agent-proxy-and-task-dispatch.md).
//
// Dock mints a durable `agent:<agent_id>` proxy token at register and returns
// it (proxy_token / proxy_base_url / default_model), which `register` persists
// in agent.toml. This file turns those persisted creds into a usable client:
// any LLM call the agent ORIGINATES itself (as opposed to running a
// dock-pushed research envelope, which already carries its own token) can go
// through the dock proxy under this agent's identity — so it's billed,
// audited, and revocable per agent.
//
// The proxy speaks the OpenAI shape, so proxy_base_url ends just before
// "/chat/completions" (same contract as researchLLMSnapshot.BaseURL): chat =
// <base>/chat/completions, model list = <base>/models (dock's
// GET /api/proxy/v1/models).

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// runModels implements `polar-agent models`: print the models the agent's
// own proxy token can use. A quick way to confirm register persisted usable
// per-agent proxy creds and that the dock proxy honors the token.
func runModels(args []string) int {
	fs := flag.NewFlagSet("models", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	cfg, err := LoadAgentConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "models: %v\n", err)
		return exitConfig
	}
	if !cfg.hasProxyCreds() {
		fmt.Fprintln(os.Stderr, "models: no per-agent proxy creds in agent.toml — re-run register against a P2 dock.")
		return exitConfig
	}
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	models, err := fetchProxyModels(ctx, cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "models: %v\n", err)
		return exitNet
	}
	if cfg.DefaultModel != "" {
		fmt.Printf("default: %s\n", cfg.DefaultModel)
	}
	for _, m := range models {
		fmt.Println(m)
	}
	return exitOK
}

// proxyLLMClient builds an llmClient bound to the agent's own proxy token,
// plus the default model alias. ok=false when register didn't supply creds
// (legacy / pre-P2 server) — callers fall back to whatever the task envelope
// gives them. The agent talks DIRECT to the dock proxy (no upstream proxy),
// so proxyURL is empty.
func (c AgentConfig) proxyLLMClient() (client *llmClient, defaultModel string, ok bool) {
	if !c.hasProxyCreds() {
		return nil, "", false
	}
	return newLLMClient(c.ProxyBaseURL, c.ProxyToken, "", ""), strings.TrimSpace(c.DefaultModel), true
}

// fetchProxyModels lists the models the agent's proxy token may use
// (GET <proxy_base_url>/models, OpenAI-compatible). Used for discovery /
// validation; the dispatched research envelope still names its own model.
func fetchProxyModels(ctx context.Context, c AgentConfig) ([]string, error) {
	if !c.hasProxyCreds() {
		return nil, errors.New("no per-agent proxy creds (pre-P2 server?)")
	}
	endpoint, err := url.JoinPath(strings.TrimRight(c.ProxyBaseURL, "/"), "models")
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(c.ProxyToken))
	httpClient := &http.Client{Timeout: 30 * time.Second}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("proxy models: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var parsed struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("proxy models: decode: %w", err)
	}
	out := make([]string, 0, len(parsed.Data))
	for _, m := range parsed.Data {
		if id := strings.TrimSpace(m.ID); id != "" {
			out = append(out, id)
		}
	}
	return out, nil
}
