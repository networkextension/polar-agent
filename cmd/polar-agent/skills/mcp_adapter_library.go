package skills

// `library` adapter — wraps dock's /api/library/... REST so the LLM
// can query the knowledge base mid-debug-loop (per
// doc/agent/jtag-debug-skill-design.md §3.5 / phase P-library-0b).
//
// 9 read-only tools. The most-used during debug:
//
//   library.functions.lookup_by_address  → MOST USED. When the LLM
//     sees PC=0x80001234 in a register read, it shouldn't have to
//     load the firmware binary into context — it asks the library
//     for the symbol + prototype + purpose in one round-trip.
//
//   library.functions.lookup_by_symbol   → reverse direction
//   library.functions.search             → fuzzy by name + purpose
//   library.firmwares.matching           → "what firmwares fit this
//                                          (cpid, bdid)?" pre-flight
//   library.firmwares.{list, get}
//   library.devices.{list, get, recent_seen}
//
// Write tools (devices.upsert, functions.add, firmwares.upload) are
// intentionally NOT exposed — admins go through /library.html. We
// can add agent-side write tools later if a workflow actually needs
// them; for now the adapter is a pure read surface.
//
// Auth: the adapter dials the dock at the agent's configured
// (server, token). If the agent isn't logged in to a dock, the
// factory returns errAdapterUnavailable and the MCP server skips
// this adapter (operator sees the rest of the catalogue).

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// libraryAdapterConfig is the (server, token) pair the adapter dials
// the dock with. Set by main via SetLibraryAdapterConfig before the
// MCP skill Start runs. Reading is safe from any goroutine.
type libraryAdapterConfig struct {
	mu     sync.RWMutex
	server string
	token  string
}

var libraryCfg = &libraryAdapterConfig{}

// SetLibraryAdapterConfig is the seam main calls during agent
// bootstrap (after LoadAgentConfig). The adapter factory reads from
// here at Start. Safe to call multiple times (last write wins).
func SetLibraryAdapterConfig(server, token string) {
	libraryCfg.mu.Lock()
	defer libraryCfg.mu.Unlock()
	libraryCfg.server = strings.TrimRight(strings.TrimSpace(server), "/")
	libraryCfg.token = strings.TrimSpace(token)
}

func (c *libraryAdapterConfig) snapshot() (string, string) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.server, c.token
}

func init() {
	RegisterMCPAdapter("library", func() (MCPAdapter, error) {
		server, token := libraryCfg.snapshot()
		if server == "" || token == "" {
			return nil, fmt.Errorf("library adapter: %w (agent not logged in, or skill loaded before SetLibraryAdapterConfig)", errAdapterUnavailable)
		}
		return &libraryAdapter{
			server: server,
			token:  token,
			client: &http.Client{Timeout: 20 * time.Second},
		}, nil
	})
}

type libraryAdapter struct {
	server string
	token  string
	client *http.Client
}

func (l *libraryAdapter) Name() string { return "library" }

func (l *libraryAdapter) Tools() []MCPToolDef {
	return []MCPToolDef{
		// devices
		{
			Name:        "library.devices.list",
			Description: "List cataloged hardware devices. Optional filters by cpid (chip id) and bdid (board id). Returns up to `limit` rows ordered by last_seen_at DESC.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"cpid":  map[string]any{"type": "integer", "description": "Chip identifier (e.g. 0x8101 = 33025)."},
					"bdid":  map[string]any{"type": "integer", "description": "Board identifier."},
					"limit": map[string]any{"type": "integer", "default": 50},
				},
			},
		},
		{
			Name:        "library.devices.get",
			Description: "Fetch one device row by its dock-side numeric id.",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"id": map[string]any{"type": "integer"}},
				"required":   []string{"id"},
			},
		},
		{
			Name:        "library.devices.recent_seen",
			Description: "Devices seen in the trailing N days (default 7). The 'active fleet' view.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"days":  map[string]any{"type": "integer", "default": 7},
					"limit": map[string]any{"type": "integer", "default": 50},
				},
			},
		},
		// firmwares
		{
			Name:        "library.firmwares.list",
			Description: "List cataloged firmware blobs. Optional filters: kind (e.g. iboot / wifi / sep), version, cpid.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"kind":    map[string]any{"type": "string"},
					"version": map[string]any{"type": "string"},
					"cpid":    map[string]any{"type": "integer"},
					"limit":   map[string]any{"type": "integer", "default": 50},
				},
			},
		},
		{
			Name:        "library.firmwares.get",
			Description: "Fetch one firmware row by id — includes blob_uri so the caller can fetch the bytes.",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"id": map[string]any{"type": "integer"}},
				"required":   []string{"id"},
			},
		},
		{
			Name:        "library.firmwares.matching",
			Description: "Pre-flight: what firmwares match this (cpid, bdid)? Returns all firmwares whose chip_id_compat / board_id_compat arrays include the given ids (or are NULL = universal).",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"cpid": map[string]any{"type": "integer"},
					"bdid": map[string]any{"type": "integer"},
				},
				"required": []string{"cpid"},
			},
		},
		// functions
		{
			Name:        "library.functions.lookup_by_address",
			Description: "MOST-USED debug tool. Given (firmware_id, address) → returns the function symbol + prototype + purpose. address may be decimal or 0x-hex. Returns 404-style empty result when no function is mapped at that address.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"firmware_id": map[string]any{"type": "integer"},
					"address":     map[string]any{"type": "string", "description": "Address as decimal or 0x-prefixed hex string."},
				},
				"required": []string{"firmware_id", "address"},
			},
		},
		{
			Name:        "library.functions.lookup_by_symbol",
			Description: "Reverse lookup: (firmware_id, symbol_name) → address + prototype + purpose. Exact symbol match.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"firmware_id": map[string]any{"type": "integer"},
					"name":        map[string]any{"type": "string"},
				},
				"required": []string{"firmware_id", "name"},
			},
		},
		{
			Name:        "library.functions.search",
			Description: "Fuzzy name+purpose search across ALL firmwares. ILIKE on symbol + purpose columns. Use when you don't know firmware_id (e.g. operator says 'find anything called *panic*').",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"q":     map[string]any{"type": "string"},
					"limit": map[string]any{"type": "integer", "default": 25},
				},
				"required": []string{"q"},
			},
		},
	}
}

func (l *libraryAdapter) Call(toolName string, args json.RawMessage) (map[string]any, error) {
	switch toolName {
	case "library.devices.list":
		var p struct {
			CPID  int `json:"cpid"`
			BDID  int `json:"bdid"`
			Limit int `json:"limit"`
		}
		_ = json.Unmarshal(args, &p)
		q := url.Values{}
		if p.CPID > 0 {
			q.Set("cpid", strconv.Itoa(p.CPID))
		}
		if p.BDID > 0 {
			q.Set("bdid", strconv.Itoa(p.BDID))
		}
		if p.Limit > 0 {
			q.Set("limit", strconv.Itoa(p.Limit))
		}
		return l.getJSONTool("/api/library/devices?" + q.Encode())

	case "library.devices.get":
		var p struct {
			ID int64 `json:"id"`
		}
		if err := json.Unmarshal(args, &p); err != nil || p.ID <= 0 {
			return nil, fmt.Errorf("id required")
		}
		return l.getJSONTool(fmt.Sprintf("/api/library/devices/%d", p.ID))

	case "library.devices.recent_seen":
		var p struct {
			Days  int `json:"days"`
			Limit int `json:"limit"`
		}
		_ = json.Unmarshal(args, &p)
		q := url.Values{}
		if p.Days > 0 {
			q.Set("days", strconv.Itoa(p.Days))
		}
		if p.Limit > 0 {
			q.Set("limit", strconv.Itoa(p.Limit))
		}
		return l.getJSONTool("/api/library/devices/recent?" + q.Encode())

	case "library.firmwares.list":
		var p struct {
			Kind    string `json:"kind"`
			Version string `json:"version"`
			CPID    int    `json:"cpid"`
			Limit   int    `json:"limit"`
		}
		_ = json.Unmarshal(args, &p)
		q := url.Values{}
		if p.Kind != "" {
			q.Set("kind", p.Kind)
		}
		if p.Version != "" {
			q.Set("version", p.Version)
		}
		if p.CPID > 0 {
			q.Set("cpid", strconv.Itoa(p.CPID))
		}
		if p.Limit > 0 {
			q.Set("limit", strconv.Itoa(p.Limit))
		}
		return l.getJSONTool("/api/library/firmwares?" + q.Encode())

	case "library.firmwares.get":
		var p struct {
			ID int64 `json:"id"`
		}
		if err := json.Unmarshal(args, &p); err != nil || p.ID <= 0 {
			return nil, fmt.Errorf("id required")
		}
		return l.getJSONTool(fmt.Sprintf("/api/library/firmwares/%d", p.ID))

	case "library.firmwares.matching":
		var p struct {
			CPID int `json:"cpid"`
			BDID int `json:"bdid"`
		}
		if err := json.Unmarshal(args, &p); err != nil || p.CPID == 0 {
			return nil, fmt.Errorf("cpid required")
		}
		q := url.Values{}
		q.Set("cpid", strconv.Itoa(p.CPID))
		if p.BDID > 0 {
			q.Set("bdid", strconv.Itoa(p.BDID))
		}
		return l.getJSONTool("/api/library/firmwares/matching?" + q.Encode())

	case "library.functions.lookup_by_address":
		var p struct {
			FirmwareID int64  `json:"firmware_id"`
			Address    string `json:"address"`
		}
		if err := json.Unmarshal(args, &p); err != nil || p.FirmwareID <= 0 || p.Address == "" {
			return nil, fmt.Errorf("firmware_id + address required")
		}
		q := url.Values{}
		q.Set("firmware_id", strconv.FormatInt(p.FirmwareID, 10))
		q.Set("address", p.Address)
		return l.getJSONTool("/api/library/functions/lookup-by-address?" + q.Encode())

	case "library.functions.lookup_by_symbol":
		var p struct {
			FirmwareID int64  `json:"firmware_id"`
			Name       string `json:"name"`
		}
		if err := json.Unmarshal(args, &p); err != nil || p.FirmwareID <= 0 || p.Name == "" {
			return nil, fmt.Errorf("firmware_id + name required")
		}
		q := url.Values{}
		q.Set("firmware_id", strconv.FormatInt(p.FirmwareID, 10))
		q.Set("name", p.Name)
		return l.getJSONTool("/api/library/functions/lookup-by-symbol?" + q.Encode())

	case "library.functions.search":
		var p struct {
			Q     string `json:"q"`
			Limit int    `json:"limit"`
		}
		if err := json.Unmarshal(args, &p); err != nil || p.Q == "" {
			return nil, fmt.Errorf("q required")
		}
		q := url.Values{}
		q.Set("q", p.Q)
		if p.Limit > 0 {
			q.Set("limit", strconv.Itoa(p.Limit))
		}
		return l.getJSONTool("/api/library/functions/search?" + q.Encode())
	}
	return nil, fmt.Errorf("library adapter: unknown tool %q", toolName)
}

func (l *libraryAdapter) Close() error { return nil }

// getJSONTool fetches the given dock path with agent-bearer auth,
// returns the response body as the MCP tool result. On non-2xx,
// surfaces the status + body to the LLM (more useful than swallowing
// — most "not found" cases are exact 404s that the LLM can act on).
func (l *libraryAdapter) getJSONTool(path string) (map[string]any, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, l.server+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+l.token)
	resp, err := l.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("dock unreachable: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	// The dock returns JSON on every path here. Parse defensively —
	// if parsing fails (rare; HTML 500 page from a misconfigured
	// reverse proxy etc.) fall back to surfacing the raw text.
	var parsed any
	if err := json.NewDecoder(bytes.NewReader(body)).Decode(&parsed); err != nil {
		return map[string]any{
			"content": []map[string]any{
				{"type": "text", "text": fmt.Sprintf("HTTP %d (non-JSON body): %s", resp.StatusCode, string(body))},
			},
			"isError": resp.StatusCode >= 400,
		}, nil
	}

	// Stringify the JSON for the text part; ALSO include the parsed
	// structure at the top level so tool callers that prefer
	// structured access (vs re-parsing the text content) can get
	// it without an extra round-trip.
	pretty, _ := json.Marshal(parsed)
	out := map[string]any{
		"content": []map[string]any{
			{"type": "text", "text": string(pretty)},
		},
	}
	if resp.StatusCode >= 400 {
		out["isError"] = true
	}
	// Surface the parsed JSON at a stable top-level key for
	// callers that want structured access.
	if obj, ok := parsed.(map[string]any); ok {
		for k, v := range obj {
			if _, exists := out[k]; !exists {
				out[k] = v
			}
		}
	}
	return out, nil
}
