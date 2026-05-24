package main

// Minimal OpenAI-compatible chat completions client used by the
// agent's research runner. Same wire shape as dock-side
// internal/app/dock/ai_agent.go, kept independent so the agent
// binary doesn't import the dock package.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type llmChatMessage struct {
	Role       string         `json:"role"`
	Content    string         `json:"content,omitempty"`
	ToolCalls  []llmToolCall  `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
	Name       string         `json:"name,omitempty"`
}

type llmToolCall struct {
	ID       string          `json:"id"`
	Type     string          `json:"type"`
	Function llmToolCallFunc `json:"function"`
}

type llmToolCallFunc struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type llmToolSpec struct {
	Type     string          `json:"type"`
	Function llmToolFunction `json:"function"`
}

type llmToolFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

type llmChatRequest struct {
	Model      string           `json:"model"`
	Messages   []llmChatMessage `json:"messages"`
	Tools      []llmToolSpec    `json:"tools,omitempty"`
	ToolChoice string           `json:"tool_choice,omitempty"`
	MaxTokens  int              `json:"max_tokens,omitempty"`
}

type llmChatResponse struct {
	Choices []struct {
		Message      llmChatMessage `json:"message"`
		FinishReason string         `json:"finish_reason"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error,omitempty"`
}

type llmClient struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

// newLLMClient builds an HTTP client for the LLM endpoint.
//
// proxyURL: when non-empty, route LLM requests through this HTTP(S)
// proxy. Comes from llm_configs.proxy_url plumbed by dock.
//
// dockBypassURL: when non-empty, requests whose host matches this
// URL's host are sent direct (bypassing the proxy). Used so the
// agent's own dock callbacks (research /start, /result) don't go
// through the upstream-LLM proxy. Loopback / 192.168.* / 10.* hosts
// are also auto-bypassed since they're never reachable through an
// upstream proxy.
func newLLMClient(baseURL, apiKey, proxyURL, dockBypassURL string) *llmClient {
	transport := &http.Transport{
		Proxy: buildProxyFunc(proxyURL, dockBypassURL),
	}
	return &llmClient{
		baseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		apiKey:  strings.TrimSpace(apiKey),
		http: &http.Client{
			Timeout:   180 * time.Second,
			Transport: transport,
		},
	}
}

// buildProxyFunc returns a Proxy func suitable for http.Transport.Proxy.
// Honors three rules:
//  1. Empty proxyURL → always direct (no proxy at all).
//  2. Bypass when request host == dockBypassURL host. Strips port
//     from both sides for comparison.
//  3. Bypass loopback + RFC1918 private addresses. Upstream proxies
//     never have a route to "192.168.x.x" so sending traffic there
//     just times out.
//
// Otherwise the configured proxyURL is used. Errors parsing proxyURL
// fall back to "direct" so a misconfig doesn't block all traffic.
func buildProxyFunc(proxyURL, dockBypassURL string) func(*http.Request) (*url.URL, error) {
	parsedProxy, err := url.Parse(strings.TrimSpace(proxyURL))
	// Require a recognizable scheme + host. Anything else (typo,
	// missing scheme, empty value) → direct fallback so a misconfig
	// doesn't black-hole all LLM traffic.
	if strings.TrimSpace(proxyURL) == "" || err != nil || parsedProxy.Host == "" {
		return func(*http.Request) (*url.URL, error) { return nil, nil }
	}
	switch parsedProxy.Scheme {
	case "http", "https", "socks5", "socks5h":
	default:
		return func(*http.Request) (*url.URL, error) { return nil, nil }
	}
	dockHost := ""
	if u, err := url.Parse(strings.TrimSpace(dockBypassURL)); err == nil {
		dockHost = u.Hostname()
	}
	return func(req *http.Request) (*url.URL, error) {
		host := req.URL.Hostname()
		if dockHost != "" && strings.EqualFold(host, dockHost) {
			return nil, nil
		}
		if host == "" || host == "localhost" || strings.HasPrefix(host, "127.") || strings.HasPrefix(host, "10.") || strings.HasPrefix(host, "192.168.") || strings.HasPrefix(host, "172.16.") {
			return nil, nil
		}
		return parsedProxy, nil
	}
}

// chatCompletions hits POST <baseURL>/chat/completions. baseURL
// follows the OpenAI convention — callers supply the prefix that
// already ends just before "/chat/completions" (matches the dock-
// side LLMConfig.BaseURL contract).
func (c *llmClient) chatCompletions(ctx context.Context, req llmChatRequest) (*llmChatResponse, error) {
	if c.baseURL == "" || c.apiKey == "" {
		return nil, errors.New("llm client missing base_url or api_key")
	}
	endpoint, err := url.JoinPath(c.baseURL, "chat", "completions")
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("llm http %d: %s", resp.StatusCode, truncateForErr(string(raw)))
	}
	var parsed llmChatResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("llm decode: %w (body: %s)", err, truncateForErr(string(raw)))
	}
	if parsed.Error != nil && parsed.Error.Message != "" {
		return nil, fmt.Errorf("llm error: %s", parsed.Error.Message)
	}
	if len(parsed.Choices) == 0 {
		return nil, errors.New("llm response has no choices")
	}
	return &parsed, nil
}

func truncateForErr(s string) string {
	if len(s) > 512 {
		return s[:512] + "...[truncated]"
	}
	return s
}
