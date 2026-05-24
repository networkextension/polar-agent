package main

// Shared types + helpers used by tools.go's multi-tool dispatch.
//
// The original kimi-only file (kimi.go) was replaced by tools.go +
// this header; the tool-specific spawn logic moved to tools.go and
// is now driven by toolSpec entries (kimi/claude/codex built-in,
// extra entries via ~/.polar/tools.json). This file keeps the
// pieces that aren't kimi-specific:
//
//   - chat_message / chat_reply wire shapes
//   - bounded output capture (capBuffer)
//   - prefix-tagged tee writer (prefixWriter)
//   - timeout env override (kimiTimeout — name kept for compat)
//   - emitChatReply marshal+send helper
//
// "kimi" persists in some symbol names for backwards-compat with
// existing env vars and constants; functionally these are
// generic-passthrough plumbing and apply to claude/codex etc.

import (
	"encoding/json"
	"io"
	"log"
	"os"
	"strings"
	"time"
)

type chatMessage struct {
	Kind     string `json:"kind"`
	ID       string `json:"id"`
	ThreadID int64  `json:"thread_id"`
	Content  string `json:"content"`
	// WorkdirSubpath, if non-empty, scopes the tool's cwd to
	// $workdir/<subpath> instead of $workdir. The platform sets
	// this from project pickup so each project's code lives in
	// its own subdir. Empty = use the agent's pinned root workdir.
	// We mkdir -p the subdir at spawn time.
	WorkdirSubpath string `json:"workdir_subpath,omitempty"`
	// GitRemoteURL, if non-empty, asks the agent to do pre/post
	// git plumbing around the tool run: ensure repo + remote, then
	// commit + push. Empty = skip git work. Phase 1 of project
	// pipeline Stage 5 (push code).
	GitRemoteURL string `json:"git_remote_url,omitempty"`
}

type chatReply struct {
	Kind    string `json:"kind"`
	ID      string `json:"id"`
	OK      bool   `json:"ok"`
	Content string `json:"content,omitempty"`
	Stderr  string `json:"stderr,omitempty"`
	Error   string `json:"error,omitempty"`
}

const (
	kimiDefaultTimeout = 30 * time.Minute
	kimiTimeoutEnvVar  = "POLAR_KIMI_TIMEOUT"
	kimiMaxStdout      = 200_000
	kimiMaxStderr      = 50_000
)

// kimiTimeout reads POLAR_KIMI_TIMEOUT (Go duration like "45m" or
// "1h") on each invocation, falling back to 30min. Per-call (not
// init-time) so the operator can tweak via env without restart.
// Env var name kept for compat with v1 polar-agent users.
func kimiTimeout() time.Duration {
	if raw := strings.TrimSpace(os.Getenv(kimiTimeoutEnvVar)); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil && d > 0 {
			return d
		}
	}
	return kimiDefaultTimeout
}

// capBuffer is an io.Writer that appends bytes up to a cap, then
// drops further input with a one-shot "[truncated]" marker. Used
// so a runaway tool can't blow memory while still surfacing a
// reasonable head of output to the chat reply.
type capBuffer struct {
	cap int
	buf []byte
	cut bool
}

func (b *capBuffer) Write(p []byte) (int, error) {
	if len(b.buf) >= b.cap {
		return len(p), nil
	}
	room := b.cap - len(b.buf)
	if len(p) <= room {
		b.buf = append(b.buf, p...)
		return len(p), nil
	}
	b.buf = append(b.buf, p[:room]...)
	if !b.cut {
		b.buf = append(b.buf, []byte("\n...[truncated]\n")...)
		b.cut = true
	}
	return len(p), nil
}

func (b *capBuffer) Bytes() []byte { return b.buf }

// prefixWriter prepends a tag to every line emitted to w. Lines
// are detected by '\n'; mid-line buffering across writes is not
// preserved (re-prefixes on every Write call). Cheap, good enough
// for kimi/claude/codex progress output where lines are usually
// flushed whole.
type prefixWriter struct {
	w      io.Writer
	prefix string
}

func (pw prefixWriter) Write(p []byte) (int, error) {
	if pw.w == nil || len(p) == 0 {
		return len(p), nil
	}
	out := make([]byte, 0, len(p)+len(pw.prefix))
	out = append(out, []byte(pw.prefix)...)
	for i, c := range p {
		out = append(out, c)
		if c == '\n' && i < len(p)-1 {
			out = append(out, []byte(pw.prefix)...)
		}
	}
	if _, err := pw.w.Write(out); err != nil {
		return 0, err
	}
	return len(p), nil
}

// emitChatReply marshals + sends a chat_reply via the shared write
// helper. Errors are logged, not returned, because the caller is
// usually a goroutine that has nowhere meaningful to surface them.
func emitChatReply(send func([]byte) error, reply *chatReply) {
	b, err := json.Marshal(reply)
	if err != nil {
		log.Printf("[passthrough] marshal chat_reply: %v", err)
		return
	}
	if err := send(b); err != nil {
		log.Printf("[passthrough] write chat_reply: %v", err)
	}
}
