package skills

// `stub` adapter — deterministic test tools.
//
// Used by polar-agent-test scenarios to exercise the full
// JSON-RPC + adapter dispatch + tool-result-shape chain WITHOUT
// any external subprocess. The three tools cover the common return
// shapes the framework needs to handle:
//   - stub.add       → success with structured numeric result
//   - stub.error     → tool-level failure (JSON-RPC error)
//   - stub.delay_ms  → success after a real delay (lets scenarios
//                      validate timeout / concurrency)
//
// Not registered on production hosts unless the operator explicitly
// includes "stub" in the skill config's adapters list — this is a
// test surface, not a feature.

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"
)

func init() {
	RegisterMCPAdapter("stub", func() (MCPAdapter, error) {
		return &stubAdapter{sessions: map[string]*stubSession{}}, nil
	})
}

type stubSession struct {
	id         string
	openedAt   int64
	lastActive int64
	meta       map[string]any
}

type stubAdapter struct {
	mu       sync.Mutex
	sessions map[string]*stubSession
}

func (s *stubAdapter) Name() string { return "stub" }

func (s *stubAdapter) Tools() []MCPToolDef {
	return []MCPToolDef{
		{
			Name:        "stub.open_session",
			Description: "Open a new session. Tests the SessionAware sub-interface — returns a session_id usable by other stub.* tools that take one.",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
		{
			Name:        "stub.close_session",
			Description: "Close a previously-opened session by id.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"session_id": map[string]any{"type": "string"},
				},
				"required": []string{"session_id"},
			},
		},
		{
			Name:        "stub.session_ping",
			Description: "Touch a session (resets idle timer) and return its metadata. Errors if session_id is unknown.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"session_id": map[string]any{"type": "string"},
				},
				"required": []string{"session_id"},
			},
		},
		{
			Name:        "stub.add",
			Description: "Return the sum of two numbers. Test tool — always succeeds with a numeric result.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"a": map[string]any{"type": "number"},
					"b": map[string]any{"type": "number"},
				},
				"required": []string{"a", "b"},
			},
		},
		{
			Name:        "stub.error",
			Description: "Always return a tool-execution error. Tests the JSON-RPC error path (-32000).",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
		{
			Name:        "stub.delay_ms",
			Description: "Sleep for the given milliseconds, then return ok. Tests timeout + concurrent dispatch.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"ms": map[string]any{"type": "integer"},
				},
				"required": []string{"ms"},
			},
		},
	}
}

func (s *stubAdapter) Call(toolName string, args json.RawMessage) (map[string]any, error) {
	switch toolName {
	case "stub.open_session":
		id, err := s.OpenSession(nil)
		if err != nil {
			return nil, err
		}
		return map[string]any{
			"content": []map[string]any{
				{"type": "text", "text": "session opened: " + id},
			},
			"session_id": id,
		}, nil
	case "stub.close_session":
		var p struct {
			SessionID string `json:"session_id"`
		}
		if err := json.Unmarshal(args, &p); err != nil {
			return nil, fmt.Errorf("invalid arguments: %w", err)
		}
		if err := s.CloseSession(p.SessionID); err != nil {
			return nil, err
		}
		return map[string]any{
			"content": []map[string]any{{"type": "text", "text": "closed"}},
			"ok":      true,
		}, nil
	case "stub.session_ping":
		var p struct {
			SessionID string `json:"session_id"`
		}
		if err := json.Unmarshal(args, &p); err != nil {
			return nil, fmt.Errorf("invalid arguments: %w", err)
		}
		s.mu.Lock()
		sess, ok := s.sessions[p.SessionID]
		if !ok {
			s.mu.Unlock()
			return nil, fmt.Errorf("unknown session: %s", p.SessionID)
		}
		sess.lastActive = time.Now().Unix()
		s.mu.Unlock()
		s.TouchSession(p.SessionID)
		return map[string]any{
			"content": []map[string]any{{"type": "text", "text": "pong " + p.SessionID}},
			"session_id": p.SessionID,
		}, nil
	case "stub.add":
		var p struct {
			A float64 `json:"a"`
			B float64 `json:"b"`
		}
		if err := json.Unmarshal(args, &p); err != nil {
			return nil, fmt.Errorf("invalid arguments: %w", err)
		}
		sum := p.A + p.B
		return map[string]any{
			"content": []map[string]any{
				{"type": "text", "text": fmt.Sprintf("%v", sum)},
			},
			"sum": sum, // structured result alongside content for tools that prefer typed access
		}, nil
	case "stub.error":
		return nil, errors.New("stub.error: intentional failure for testing")
	case "stub.delay_ms":
		var p struct {
			MS int `json:"ms"`
		}
		if err := json.Unmarshal(args, &p); err != nil {
			return nil, fmt.Errorf("invalid arguments: %w", err)
		}
		// Cap at 10s defensively — a runaway delay would tie up a
		// scenario indefinitely.
		if p.MS < 0 {
			p.MS = 0
		}
		if p.MS > 10000 {
			p.MS = 10000
		}
		time.Sleep(time.Duration(p.MS) * time.Millisecond)
		return map[string]any{
			"content": []map[string]any{
				{"type": "text", "text": fmt.Sprintf("slept %dms", p.MS)},
			},
		}, nil
	default:
		return nil, fmt.Errorf("stub adapter: unknown tool %q", toolName)
	}
}

func (s *stubAdapter) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	// CloseSession on each (idempotent), then drop the map.
	s.sessions = map[string]*stubSession{}
	return nil
}

// --- SessionAware implementation ---

func (s *stubAdapter) OpenSession(meta map[string]any) (string, error) {
	id := newSessionID()
	now := time.Now().Unix()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[id] = &stubSession{
		id:         id,
		openedAt:   now,
		lastActive: now,
		meta:       meta,
	}
	return id, nil
}

func (s *stubAdapter) CloseSession(sessionID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, sessionID) // idempotent: deleting an absent key is a no-op
	return nil
}

func (s *stubAdapter) ListSessions() []SessionInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]SessionInfo, 0, len(s.sessions))
	for _, sess := range s.sessions {
		out = append(out, SessionInfo{
			ID:         sess.id,
			Adapter:    "stub",
			OpenedAt:   sess.openedAt,
			LastActive: sess.lastActive,
			Metadata:   sess.meta,
		})
	}
	return out
}

func (s *stubAdapter) TouchSession(sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess, ok := s.sessions[sessionID]; ok {
		sess.lastActive = time.Now().Unix()
	}
}

// newSessionID returns "s_" + 16 random hex chars. Short enough to
// type, long enough to be globally unique within an agent process.
func newSessionID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "s_" + hex.EncodeToString(b)
}
