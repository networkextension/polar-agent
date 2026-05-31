package skills

// mcp_adapter_apicatalog.go — an MCP adapter that lets the agent discover
// the dock API by querying dock's self-describing catalog
// (`GET /api/_catalog`, built by cmd/api-catalog-gen) instead of grepping
// source. Two tools:
//
//	api_list     {prefix?}        → endpoints (method, path, auth, body fields)
//	api_describe {method, path}   → one endpoint's full contract
//
// Mirrors mcp_adapter_library.go (the proven dock-REST adapter pattern):
// main injects (server, token) via SetAPICatalogConfig before the MCP
// skill starts; the factory reads it at Start; calls reuse the agent's
// own token + server from agent.toml.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

type apiCatalogAdapterConfig struct {
	mu     sync.RWMutex
	server string
	token  string
}

var apiCatalogCfg = &apiCatalogAdapterConfig{}

// SetAPICatalogConfig is the seam main calls during agent bootstrap
// (after LoadAgentConfig), alongside SetLibraryAdapterConfig.
func SetAPICatalogConfig(server, token string) {
	apiCatalogCfg.mu.Lock()
	defer apiCatalogCfg.mu.Unlock()
	apiCatalogCfg.server = strings.TrimRight(strings.TrimSpace(server), "/")
	apiCatalogCfg.token = strings.TrimSpace(token)
}

func (c *apiCatalogAdapterConfig) snapshot() (string, string) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.server, c.token
}

func init() {
	RegisterMCPAdapter("apicatalog", func() (MCPAdapter, error) {
		server, token := apiCatalogCfg.snapshot()
		if server == "" || token == "" {
			return nil, fmt.Errorf("apicatalog adapter: %w (agent not logged in, or skill loaded before SetAPICatalogConfig)", errAdapterUnavailable)
		}
		return &apiCatalogAdapter{
			server: server,
			token:  token,
			client: &http.Client{Timeout: 15 * time.Second},
		}, nil
	})
}

type apiCatalogAdapter struct {
	server string
	token  string
	client *http.Client
}

func (a *apiCatalogAdapter) Name() string { return "apicatalog" }

func (a *apiCatalogAdapter) Tools() []MCPToolDef {
	return []MCPToolDef{
		{
			Name:        "api_list",
			Description: "List dock API endpoints (method, path, auth, request-body fields). Optional path prefix filter, e.g. /api/llm-proxy. Use this instead of grepping source to find an endpoint.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"prefix": map[string]any{"type": "string", "description": "filter endpoints whose path starts with this, e.g. /api/assets"},
				},
			},
		},
		{
			Name:        "api_describe",
			Description: "Describe one dock API endpoint: its request-body fields (name/type/required), query params, path params, headers, and auth.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"method": map[string]any{"type": "string", "description": "HTTP method, e.g. POST"},
					"path":   map[string]any{"type": "string", "description": "route path, e.g. /api/llm-proxy/tokens"},
				},
				"required": []any{"method", "path"},
			},
		},
	}
}

func (a *apiCatalogAdapter) Call(toolName string, args json.RawMessage) (map[string]any, error) {
	switch toolName {
	case "api_list":
		var in struct {
			Prefix string `json:"prefix"`
		}
		_ = json.Unmarshal(args, &in)
		path := "/api/_catalog"
		if p := strings.TrimSpace(in.Prefix); p != "" {
			path += "?prefix=" + p
		}
		body, err := a.getJSON(path)
		if err != nil {
			return nil, err
		}
		return textResult(body), nil

	case "api_describe":
		var in struct {
			Method string `json:"method"`
			Path   string `json:"path"`
		}
		if err := json.Unmarshal(args, &in); err != nil {
			return nil, fmt.Errorf("api_describe: bad args: %w", err)
		}
		in.Method = strings.ToUpper(strings.TrimSpace(in.Method))
		in.Path = strings.TrimSpace(in.Path)
		if in.Method == "" || in.Path == "" {
			return nil, fmt.Errorf("api_describe: method and path are required")
		}
		body, err := a.getJSON("/api/_catalog?prefix=" + in.Path)
		if err != nil {
			return nil, err
		}
		var cat struct {
			Endpoints []map[string]any `json:"endpoints"`
		}
		if err := json.Unmarshal(body, &cat); err != nil {
			return nil, fmt.Errorf("api_describe: decode catalog: %w", err)
		}
		for _, e := range cat.Endpoints {
			if m, _ := e["method"].(string); m == in.Method {
				if p, _ := e["path"].(string); p == in.Path {
					b, _ := json.MarshalIndent(e, "", "  ")
					return textResult(b), nil
				}
			}
		}
		return textResult([]byte(fmt.Sprintf("no endpoint %s %s in catalog", in.Method, in.Path))), nil

	default:
		return nil, fmt.Errorf("apicatalog: unknown tool %q", toolName)
	}
}

func (a *apiCatalogAdapter) Close() error { return nil }

// getJSON GETs server+path with the agent bearer token and returns the raw body.
func (a *apiCatalogAdapter) getJSON(path string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.server+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+a.token)
	resp, err := a.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("dock %s → HTTP %d: %s", path, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return body, nil
}

// textResult wraps a payload as an MCP text content result.
func textResult(b []byte) map[string]any {
	return map[string]any{"content": []any{map[string]any{"type": "text", "text": string(b)}}}
}
