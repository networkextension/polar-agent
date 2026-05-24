package skills

// `echo` adapter — pure transport sanity check.
//
// Mirrors the wire-end-to-end validation we shipped in P0a but
// repackaged as an adapter so the rest of the codebase doesn't need
// to know about hand-listed built-in tools. Always loads; no
// preconditions.

import (
	"encoding/json"
	"fmt"
)

func init() {
	RegisterMCPAdapter("echo", func() (MCPAdapter, error) {
		return &echoAdapter{}, nil
	})
}

type echoAdapter struct{}

func (e *echoAdapter) Name() string { return "echo" }

func (e *echoAdapter) Tools() []MCPToolDef {
	return []MCPToolDef{{
		Name:        "echo",
		Description: "Echo the input message back. Sanity-check tool — proves the MCP transport works without any external dependency.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"message": map[string]any{
					"type":        "string",
					"description": "The text to echo back.",
				},
			},
			"required": []string{"message"},
		},
	}}
}

func (e *echoAdapter) Call(toolName string, args json.RawMessage) (map[string]any, error) {
	if toolName != "echo" {
		return nil, fmt.Errorf("echo adapter: unknown tool %q", toolName)
	}
	var p struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	return map[string]any{
		"content": []map[string]any{
			{"type": "text", "text": p.Message},
		},
	}, nil
}

func (e *echoAdapter) Close() error { return nil }
