package skills

// MCP-server skill (Host module P3, P0a slice — protocol scaffold +
// built-in echo tool). See doc/agent/mcp-skill-design.md (the "why") and
// doc/agent/mcp-skill-dev.md (the phase plan).
//
// Wire shape: byte-stream skill, same as shell — operator-side stdin
// is line-delimited JSON-RPC 2.0 (per MCP spec); EventStdout emits
// the JSON-RPC responses. No TTY semantics (no resize). This means
// the existing test-serve / WS bridge / supervisor all work
// unchanged — JSON-RPC framing is just bytes from the transport's
// perspective.
//
// What's implemented in P0a:
//   - `initialize`     — handshake, returns server capabilities
//   - `tools/list`     — enumerate registered tools (just `echo` for now)
//   - `tools/call`     — dispatch to tool; echo returns its `message` arg
//   - Standard JSON-RPC error returns on bad method / params / parse
//
// What's NOT in P0a (deferred to P0b+):
//   - External adapters (gdb-mi, lldb-dap, ida-rpc, frida, radare2)
//   - Persistent sessions across tool calls
//   - resources/* / prompts/* methods
//   - Notifications (server-pushed)
//   - The `github.com/modelcontextprotocol/go-sdk` integration — we
//     hand-roll the 3 RPC methods to keep P0a dependency-free; swap
//     in later PR when the adapter surface grows.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

const (
	mcpProtocolVersion = "2024-11-05"
	mcpServerName      = "polar-mcp-server"
	mcpServerVersion   = "0.2-p0c"
	mcpStdoutChunkCap  = 64 << 10
	mcpDefaultIdleSec  = 1800
	mcpHardCapSec      = 21600

	// mcpSessionIdleSec is the per-session idle eviction window. The
	// framework's tick (idleWatchdog) asks every SessionAware adapter
	// to drop sessions whose last_active is older than this. Default
	// 30 min matches the per-run idle in mcpDefaultIdleSec — sessions
	// inside a run can be reaped before the run itself.
	mcpSessionIdleSec = 1800
)

// MCPConfig is the per-run config inside skill.start.
type MCPConfig struct {
	// Adapters: list of adapter names to load. Default ["echo"]
	// when empty. Each name must be a registered MCPAdapterFactory
	// (see mcp_adapter.go's RegisterMCPAdapter). Unknown / unavailable
	// adapters are logged + skipped rather than aborting the whole
	// MCP server.
	Adapters []string `json:"adapters,omitempty"`
}

type mcpSkill struct{}

// NewMCPServerSkill returns the MCP server skill instance. Single
// global server per agent — multiple skill.start calls each get their
// own Run, but tools are shared (stateless in P0a; per-session state
// lands in P0c).
func NewMCPServerSkill() Skill { return &mcpSkill{} }

func (m *mcpSkill) Kind() SkillKind { return KindMCPServer }
func (m *mcpSkill) Version() string { return mcpServerVersion }

func (m *mcpSkill) Capabilities() map[string]any {
	return map[string]any{
		"protocol":            "jsonrpc-ndjson",
		"mcp_version":         mcpProtocolVersion,
		"available_adapters":  registeredAdapterNames(),
		"default_idle_sec":    mcpDefaultIdleSec,
		"hard_cap_sec":        mcpHardCapSec,
	}
}

func (m *mcpSkill) Validate(config json.RawMessage) error {
	if len(config) == 0 || string(config) == "null" {
		return nil
	}
	var cfg MCPConfig
	return json.Unmarshal(config, &cfg)
}

func (m *mcpSkill) Start(ctx context.Context, runID int64, config json.RawMessage) (Run, error) {
	var cfg MCPConfig
	if len(config) > 0 && string(config) != "null" {
		if err := json.Unmarshal(config, &cfg); err != nil {
			return nil, fmt.Errorf("mcp: parse config: %w", err)
		}
	}
	// Default adapter set when operator didn't specify — keep the
	// P0a-equivalent behavior of "echo tool available out of the box".
	if len(cfg.Adapters) == 0 {
		cfg.Adapters = []string{"echo"}
	}

	// Instantiate each requested adapter. Soft-fail on any that aren't
	// available (e.g. gdb not on PATH) — partial catalogues beat no
	// catalogue. Track loaded + skipped for the state event.
	adapters := []MCPAdapter{}
	skipped := map[string]string{}
	for _, name := range cfg.Adapters {
		ad, err := newMCPAdapter(name)
		if err != nil {
			skipped[name] = err.Error()
			log.Printf("[mcp] run=%d adapter %q skipped: %v", runID, name, err)
			continue
		}
		adapters = append(adapters, ad)
	}

	runCtx, cancel := context.WithCancel(ctx)
	run := &mcpRun{
		id:        runID,
		cfg:       cfg,
		adapters:  adapters,
		events:    make(chan Event, 64),
		ctxCancel: cancel,
		inBuf:     &bytes.Buffer{},
		startedAt: time.Now(),
	}
	run.lastActivity.Store(time.Now().UnixNano())
	run.send(Event{Kind: EventState, Data: map[string]any{
		"status":        "running",
		"mcp_version":   mcpProtocolVersion,
		"server_name":   mcpServerName,
		"loaded_adapters": run.adapterNames(),
		"skipped_adapters": skipped,
		"tools":         run.allToolNames(),
	}})
	go run.idleWatchdog(runCtx)
	return run, nil
}

// mcpRun is one MCP server session. JSON-RPC requests arrive via
// WriteInput (line-delimited NDJSON over the skill's stdin), responses
// stream out as EventStdout.
type mcpRun struct {
	id       int64
	cfg      MCPConfig
	adapters []MCPAdapter

	events    chan Event
	ctxCancel context.CancelFunc
	startedAt time.Time

	inMu  sync.Mutex
	inBuf *bytes.Buffer // accumulates partial-line input across WriteInput calls

	lastActivity atomic.Int64
	stopOnce     sync.Once
	closed       atomic.Bool
}

func (r *mcpRun) adapterNames() []string {
	out := make([]string, len(r.adapters))
	for i, a := range r.adapters {
		out[i] = a.Name()
	}
	return out
}

func (r *mcpRun) allTools() []MCPToolDef {
	out := []MCPToolDef{}
	for _, a := range r.adapters {
		out = append(out, a.Tools()...)
	}
	return out
}

func (r *mcpRun) allToolNames() []string {
	out := []string{}
	for _, a := range r.adapters {
		for _, t := range a.Tools() {
			out = append(out, t.Name)
		}
	}
	return out
}

func (r *mcpRun) findToolAdapter(toolName string) MCPAdapter {
	for _, a := range r.adapters {
		for _, t := range a.Tools() {
			if t.Name == toolName {
				return a
			}
		}
	}
	return nil
}

func (r *mcpRun) ID() int64                { return r.id }
func (r *mcpRun) Events() <-chan Event     { return r.events }
func (r *mcpRun) Stop(reason string) error { return r.stop(reason) }

// Resize is a no-op for MCP — JSON-RPC has no terminal size concept.
func (r *mcpRun) Resize(rows, cols uint16) error { return nil }

// WriteInput accumulates bytes and dispatches complete JSON-RPC lines.
// Multiple requests can arrive in one WriteInput call; partial trailers
// are kept until the next call. Matches NDJSON conventions.
func (r *mcpRun) WriteInput(data []byte) error {
	if r.closed.Load() {
		return errors.New("mcp run closed")
	}
	r.lastActivity.Store(time.Now().UnixNano())
	r.inMu.Lock()
	r.inBuf.Write(data)
	scanner := bufio.NewScanner(bytes.NewReader(r.inBuf.Bytes()))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var consumed int
	var lines [][]byte
	for scanner.Scan() {
		raw := append([]byte(nil), scanner.Bytes()...)
		// Track consumed byte count: scanner gives us trimmed lines, so
		// we need to seek through inBuf to find the newline boundary.
		idx := bytes.IndexByte(r.inBuf.Bytes()[consumed:], '\n')
		if idx < 0 {
			// Scanner returned a final partial line; rewind so we keep
			// buffering until the trailing newline.
			break
		}
		consumed += idx + 1
		if len(bytes.TrimSpace(raw)) == 0 {
			continue
		}
		lines = append(lines, raw)
	}
	// Drop consumed bytes, keep the (possibly empty) partial trailer.
	if consumed > 0 {
		trailer := append([]byte(nil), r.inBuf.Bytes()[consumed:]...)
		r.inBuf.Reset()
		r.inBuf.Write(trailer)
	}
	r.inMu.Unlock()

	for _, line := range lines {
		r.dispatchLine(line)
	}
	return nil
}

// dispatchLine parses one JSON-RPC request line and replies (request
// or notification). Per JSON-RPC 2.0: requests with `id` get a reply,
// notifications (no id) don't.
func (r *mcpRun) dispatchLine(line []byte) {
	var req struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id,omitempty"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params,omitempty"`
	}
	if err := json.Unmarshal(line, &req); err != nil {
		r.replyError(nil, -32700, "parse error: "+err.Error())
		return
	}
	if req.JSONRPC != "2.0" {
		r.replyError(req.ID, -32600, `"jsonrpc" must be "2.0"`)
		return
	}
	isNotif := len(req.ID) == 0
	switch req.Method {
	case "initialize":
		if isNotif {
			return // ignore notification form
		}
		var params struct {
			ProtocolVersion string         `json:"protocolVersion"`
			Capabilities    map[string]any `json:"capabilities"`
			ClientInfo      map[string]any `json:"clientInfo"`
		}
		_ = json.Unmarshal(req.Params, &params)
		r.replyResult(req.ID, map[string]any{
			"protocolVersion": mcpProtocolVersion,
			"capabilities": map[string]any{
				"tools": map[string]any{},
			},
			"serverInfo": map[string]any{
				"name":    mcpServerName,
				"version": mcpServerVersion,
			},
		})
	case "notifications/initialized":
		// Notification, no reply. Could update state here later.
	case "tools/list":
		if isNotif {
			return
		}
		// Marshal each adapter's tool defs verbatim into the response,
		// plus the framework-owned mcp.* meta tools (list_sessions /
		// close_session) which apply across all adapters that
		// implement SessionAware.
		toolList := []map[string]any{}
		for _, t := range r.allTools() {
			toolList = append(toolList, map[string]any{
				"name":        t.Name,
				"description": t.Description,
				"inputSchema": t.InputSchema,
			})
		}
		for _, t := range mcpFrameworkTools() {
			toolList = append(toolList, map[string]any{
				"name":        t.Name,
				"description": t.Description,
				"inputSchema": t.InputSchema,
			})
		}
		r.replyResult(req.ID, map[string]any{"tools": toolList})
	case "tools/call":
		if isNotif {
			return
		}
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			r.replyError(req.ID, -32602, "invalid params: "+err.Error())
			return
		}
		// Framework meta tools (mcp.list_sessions / mcp.close_session)
		// don't belong to any adapter — handled directly.
		if isMCPFrameworkTool(p.Name) {
			result, callErr := r.callFrameworkTool(p.Name, p.Arguments)
			if callErr != nil {
				r.replyError(req.ID, -32000, callErr.Error())
				return
			}
			r.replyResult(req.ID, result)
			return
		}
		adapter := r.findToolAdapter(p.Name)
		if adapter == nil {
			r.replyError(req.ID, -32601, "unknown tool: "+p.Name)
			return
		}
		result, callErr := adapter.Call(p.Name, p.Arguments)
		if callErr != nil {
			r.replyError(req.ID, -32000, callErr.Error())
			return
		}
		r.replyResult(req.ID, result)
	default:
		if !isNotif {
			r.replyError(req.ID, -32601, "method not found: "+req.Method)
		}
	}
}

func (r *mcpRun) replyResult(id json.RawMessage, result any) {
	frame := map[string]any{
		"jsonrpc": "2.0",
		"id":      json.RawMessage(id),
		"result":  result,
	}
	r.emitJSON(frame)
}

func (r *mcpRun) replyError(id json.RawMessage, code int, message string) {
	if id == nil {
		id = json.RawMessage("null")
	}
	frame := map[string]any{
		"jsonrpc": "2.0",
		"id":      json.RawMessage(id),
		"error": map[string]any{
			"code":    code,
			"message": message,
		},
	}
	r.emitJSON(frame)
}

func (r *mcpRun) emitJSON(frame map[string]any) {
	b, err := json.Marshal(frame)
	if err != nil {
		log.Printf("[mcp] run=%d marshal: %v", r.id, err)
		return
	}
	// NDJSON: every reply gets a trailing newline.
	b = append(b, '\n')
	r.send(Event{Kind: EventStdout, Data: map[string]any{
		"bytes_b64": base64.StdEncoding.EncodeToString(b),
	}})
}

func (r *mcpRun) idleWatchdog(ctx context.Context) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			if r.closed.Load() {
				return
			}
			last := time.Unix(0, r.lastActivity.Load())
			if now.Sub(last) >= mcpDefaultIdleSec*time.Second {
				r.stop("idle")
				return
			}
			if now.Sub(r.startedAt) >= mcpHardCapSec*time.Second {
				r.stop("hard_cap")
				return
			}
			// P0c: reap idle sessions inside still-running adapters.
			// Tick uses the same 30s cadence; per-session timeout is
			// mcpSessionIdleSec.
			r.reapIdleSessions()
		}
	}
}

func (r *mcpRun) stop(reason string) error {
	r.stopOnce.Do(func() {
		r.closed.Store(true)
		// Close adapters in reverse-load order so a later adapter's
		// resources (e.g. a subprocess started AFTER something else
		// it depends on) get torn down first.
		for i := len(r.adapters) - 1; i >= 0; i-- {
			if err := r.adapters[i].Close(); err != nil {
				log.Printf("[mcp] run=%d adapter %q close: %v", r.id, r.adapters[i].Name(), err)
			}
		}
		r.send(Event{Kind: EventExit, Data: map[string]any{
			"ok":     reason == "operator" || reason == "idle" || reason == "hard_cap",
			"reason": reason,
		}})
		r.ctxCancel()
		close(r.events)
	})
	return nil
}

func (r *mcpRun) send(ev Event) {
	defer func() { _ = recover() }()
	select {
	case r.events <- ev:
	default:
		if ev.Kind == EventStdout {
			return
		}
		select {
		case r.events <- ev:
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// Tools are owned by adapters (mcp_adapter_echo.go, mcp_adapter_stub.go,
// future gdb/lldb/ida/frida/radare2). Skill dispatches via
// mcpRun.findToolAdapter — see Start() for the load-from-config path.

// ---- framework meta tools (mcp.list_sessions / mcp.close_session) ----
// These belong to the MCP server itself, not to any adapter. They
// iterate every loaded adapter that implements SessionAware and union
// the results. Adapters that don't implement SessionAware contribute
// nothing — no error.

func mcpFrameworkTools() []MCPToolDef {
	return []MCPToolDef{
		{
			Name:        "mcp.list_sessions",
			Description: "List all open sessions across every SessionAware adapter currently loaded. Returns id + adapter + opened_at + last_active + adapter-specific metadata.",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
		{
			Name:        "mcp.close_session",
			Description: "Close a session by id. Searches every SessionAware adapter; errors if no adapter owns the id.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"session_id": map[string]any{"type": "string"},
				},
				"required": []string{"session_id"},
			},
		},
	}
}

func isMCPFrameworkTool(name string) bool {
	for _, t := range mcpFrameworkTools() {
		if t.Name == name {
			return true
		}
	}
	return false
}

func (r *mcpRun) callFrameworkTool(name string, args json.RawMessage) (map[string]any, error) {
	switch name {
	case "mcp.list_sessions":
		all := []SessionInfo{}
		for _, ad := range r.adapters {
			if sa, ok := ad.(SessionAware); ok {
				all = append(all, sa.ListSessions()...)
			}
		}
		return map[string]any{
			"content": []map[string]any{
				{"type": "text", "text": fmt.Sprintf("%d open session(s)", len(all))},
			},
			"sessions": all,
		}, nil
	case "mcp.close_session":
		var p struct {
			SessionID string `json:"session_id"`
		}
		if err := json.Unmarshal(args, &p); err != nil {
			return nil, fmt.Errorf("invalid params: %w", err)
		}
		// Find the adapter that owns this id.
		for _, ad := range r.adapters {
			sa, ok := ad.(SessionAware)
			if !ok {
				continue
			}
			for _, info := range sa.ListSessions() {
				if info.ID == p.SessionID {
					if err := sa.CloseSession(p.SessionID); err != nil {
						return nil, err
					}
					return map[string]any{
						"content": []map[string]any{{"type": "text", "text": "closed"}},
						"ok":      true,
					}, nil
				}
			}
		}
		return nil, fmt.Errorf("session %q not found", p.SessionID)
	}
	return nil, fmt.Errorf("unknown framework tool: %s", name)
}

// reapIdleSessions runs as part of the existing idleWatchdog tick;
// asks every SessionAware adapter to drop sessions whose last_active
// is older than the configured cutoff. Env override
// POLAR_MCP_SESSION_IDLE_SEC lets the harness set tiny windows
// (e.g. 2 seconds) so the idle-evict scenario doesn't have to wait
// the production 30-min default.
func (r *mcpRun) reapIdleSessions() {
	cutoff := time.Now().Unix() - sessionIdleSec()
	for _, ad := range r.adapters {
		sa, ok := ad.(SessionAware)
		if !ok {
			continue
		}
		for _, info := range sa.ListSessions() {
			if info.LastActive < cutoff {
				log.Printf("[mcp] run=%d evicting idle session %s (adapter=%s, idle=%ds)",
					r.id, info.ID, info.Adapter, time.Now().Unix()-info.LastActive)
				_ = sa.CloseSession(info.ID)
			}
		}
	}
}

func sessionIdleSec() int64 {
	if v := os.Getenv("POLAR_MCP_SESSION_IDLE_SEC"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			return n
		}
	}
	return mcpSessionIdleSec
}
