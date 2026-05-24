package skills

// MCP adapter abstraction (Host module P3, P0b slice).
//
// An adapter is a named bundle of MCP tools that share lifecycle +
// (eventually) session state. The MCP server skill loads zero or more
// adapters at Start(); the union of their Tools() forms the
// tools/list catalogue; tools/call dispatches by tool name.
//
// Why an abstraction (vs hand-listing tools in mcp_server.go):
//   - Operator config selects which adapters load per host
//     (e.g. {"adapters":["gdb","frida"]} on the iOS-debug box,
//      {"adapters":["lldb"]} on the build box). Without an
//     abstraction we'd be wiring init() functions + compile-time
//     toggles.
//   - Each adapter owns its subprocess / session state. echo is
//     stateless; gdb/lldb/ida hold a long-running child + session
//     handles. The interface lets the skill iterate adapters
//     uniformly for tool dispatch + Close() on shutdown.
//   - Test surface: a `stub` adapter ships with deterministic tools
//     (stub.add / stub.error / stub.delay_ms) so we can validate
//     the dispatch chain without external dependencies.
//
// P0b ships echo + stub as adapters. gdb / lldb / ida lands in P0c+
// using this same shape.

import (
	"encoding/json"
	"errors"
	"fmt"
)

// MCPAdapter is the per-bundle plug-in.
type MCPAdapter interface {
	// Name uniquely identifies the adapter at config time
	// ({"adapters":["echo","stub","gdb"]}).
	Name() string

	// Tools is the catalogue this adapter contributes to tools/list.
	// Returned definitions appear verbatim in the JSON-RPC response.
	Tools() []MCPToolDef

	// Call dispatches a single tool invocation. Returning a non-nil
	// error becomes a JSON-RPC error (code -32000) on the wire.
	// The result map should follow the MCP spec — for text-replying
	// tools that means `{"content":[{"type":"text","text":...}]}`.
	Call(toolName string, args json.RawMessage) (map[string]any, error)

	// Close releases any held resources (subprocesses, sockets,
	// session handles). Called when the skill's Run is stopping.
	// Stateless adapters return nil.
	Close() error
}

// MCPToolDef is one row of the tools/list response.
type MCPToolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

// MCPAdapterFactory constructs an adapter at skill.Start time. Returning
// an error means the adapter is unavailable on this host (e.g. gdb
// not on PATH). The MCP skill logs the failure + continues with the
// other adapters rather than aborting — partial catalogues are better
// than no catalogue.
type MCPAdapterFactory func() (MCPAdapter, error)

// mcpAdapterFactories is the package-level registry — adapter files
// register their factory here in init(). The skill calls newMCPAdapter
// by name to instantiate.
var mcpAdapterFactories = map[string]MCPAdapterFactory{}

// RegisterMCPAdapter wires an adapter factory by name. Called from
// each adapter file's init() — see mcp_adapter_echo.go for the
// pattern. Panics on duplicate name (programmer error).
func RegisterMCPAdapter(name string, f MCPAdapterFactory) {
	if _, exists := mcpAdapterFactories[name]; exists {
		panic("mcp: duplicate adapter name: " + name)
	}
	mcpAdapterFactories[name] = f
}

// newMCPAdapter instantiates the named adapter or returns an error if
// the name isn't registered / the factory fails.
func newMCPAdapter(name string) (MCPAdapter, error) {
	f, ok := mcpAdapterFactories[name]
	if !ok {
		return nil, fmt.Errorf("unknown adapter %q (registered: %v)", name, registeredAdapterNames())
	}
	return f()
}

func registeredAdapterNames() []string {
	out := make([]string, 0, len(mcpAdapterFactories))
	for k := range mcpAdapterFactories {
		out = append(out, k)
	}
	return out
}

// errAdapterUnavailable is the conventional error type to return from
// a factory when the underlying tool isn't installed on this host
// (gdb missing, frida missing, etc.). The skill treats this as a
// soft failure: log + skip, keep loading other adapters.
var errAdapterUnavailable = errors.New("adapter unavailable on this host")

// SessionAware is an OPTIONAL sub-interface for adapters that hold
// per-attach state (LLDB target, JTAG probe attach, gdb subprocess,
// etc.). Type-asserted by the MCP server at tools/call time so it can
// validate session_ids + route to the right session state.
//
// Stateless adapters (echo) don't implement this. Stub implements it
// for harness validation.
//
// Lifecycle:
//   1. operator calls a "create" tool (e.g. lldb.attach) — adapter
//      implementation calls OpenSession to register and returns the
//      id in the tool result
//   2. operator calls subsequent tools passing session_id in args —
//      adapter dispatches by id internally
//   3. operator calls a "destroy" tool OR the MCP server's idle
//      watchdog fires — adapter's CloseSession releases resources
type SessionAware interface {
	// OpenSession registers a new session and returns its opaque id.
	// The adapter is free to back this with any state (subprocess,
	// file handle, etc.). Called from the adapter's "create" tool.
	OpenSession(meta map[string]any) (string, error)

	// CloseSession releases the session's resources. Idempotent; a
	// double close is a no-op. Called by the adapter's "destroy"
	// tool OR by the MCP server's idle watchdog OR by adapter Close
	// at shutdown.
	CloseSession(sessionID string) error

	// ListSessions enumerates active sessions for the
	// mcp.list_sessions framework tool. Returns id + opaque metadata
	// the adapter wants to surface (target, attached_at, etc.).
	ListSessions() []SessionInfo

	// TouchSession marks the session as recently active so the idle
	// watchdog leaves it alone. Adapters call this from any tool
	// that accepts a session_id.
	TouchSession(sessionID string)
}

// SessionInfo is one row of mcp.list_sessions output.
type SessionInfo struct {
	ID          string         `json:"id"`
	Adapter     string         `json:"adapter"`
	OpenedAt    int64          `json:"opened_at_unix"`     // seconds
	LastActive  int64          `json:"last_active_unix"`   // seconds
	Metadata    map[string]any `json:"metadata,omitempty"` // adapter-specific
}
