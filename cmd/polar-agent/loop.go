package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/networkextension/polar-agent/cmd/polar-agent/skills"
)

// activeRuns tracks Run handles currently draining inside the
// skill.start goroutine. P1c only needed write-once (events pumped
// out), but P2 adds bidirectional envelopes (skill.stdin /
// skill.resize / skill.stop) that need to find the Run by run_id.
// Map is process-global because the agent has one WS connection at a
// time and a small number of concurrent runs.
type runRegistry struct {
	mu     sync.Mutex
	byID   map[int64]skills.Run
	kindBy map[int64]string // P1a: runID → SkillKind, for uninstall active-runs gate
}

var activeRuns = &runRegistry{
	byID:   map[int64]skills.Run{},
	kindBy: map[int64]string{},
}

func (r *runRegistry) put(runID int64, run skills.Run) {
	r.mu.Lock()
	r.byID[runID] = run
	r.mu.Unlock()
}

// putWithKind records the run + its skill kind so uninstall can later
// check whether removing a bundle would orphan active runs.
func (r *runRegistry) putWithKind(runID int64, run skills.Run, kind string) {
	r.mu.Lock()
	r.byID[runID] = run
	r.kindBy[runID] = kind
	r.mu.Unlock()
}

// kindsByRunID returns a snapshot of {runID: kind} for the
// uninstaller's active-runs gate.
func (r *runRegistry) kindsByRunID() map[int64]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[int64]string, len(r.kindBy))
	for k, v := range r.kindBy {
		out[k] = v
	}
	return out
}

func (r *runRegistry) get(runID int64) (skills.Run, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	run, ok := r.byID[runID]
	return run, ok
}

func (r *runRegistry) delete(runID int64) {
	r.mu.Lock()
	delete(r.byID, runID)
	delete(r.kindBy, runID)
	r.mu.Unlock()
}

// stopAll signals every active Run to exit. Called from the WS read
// loop when the connection to dock drops — without this, an
// orphaned shell could keep running on the host with no operator
// holding the other end. Returns synchronously after issuing stops;
// each Run's own Stop is idempotent + best-effort.
func (r *runRegistry) stopAll(reason string) {
	r.mu.Lock()
	runs := make([]skills.Run, 0, len(r.byID))
	for _, run := range r.byID {
		runs = append(runs, run)
	}
	r.mu.Unlock()
	for _, run := range runs {
		_ = run.Stop(reason)
	}
}

// skillStdinEnvelope, skillResizeEnvelope, skillStopEnvelope mirror
// the dock-side dispatch helpers in internal/app/dock/agent_hub.go.
// Single-direction (dock → agent); responses flow back as
// skill.event frames via the existing Run.Events() pump.
type skillStdinEnvelope struct {
	Kind     string `json:"kind"` // "skill.stdin"
	RunID    int64  `json:"run_id"`
	BytesB64 string `json:"bytes_b64"`
}

type skillResizeEnvelope struct {
	Kind  string `json:"kind"` // "skill.resize"
	RunID int64  `json:"run_id"`
	Rows  uint16 `json:"rows"`
	Cols  uint16 `json:"cols"`
}

type skillStopEnvelope struct {
	Kind   string `json:"kind"` // "skill.stop"
	RunID  int64  `json:"run_id"`
	Reason string `json:"reason"`
}

// hostName returns os.Hostname() with errors swallowed — the host
// info is non-critical metadata for the dock-side agent list.
func hostName() string {
	n, err := os.Hostname()
	if err != nil || n == "" {
		return "unknown"
	}
	return n
}

// isSafeSubpath blocks workdir_subpath values that would escape the
// agent's pinned workdir. Allowed: relative paths with no "..",
// no leading slash. Rejected: anything else. This is defense-in-
// depth; the platform won't normally send malicious values, but
// the path comes from a trusted-but-network channel.
func isSafeSubpath(p string) bool {
	if p == "" {
		return false
	}
	if filepath.IsAbs(p) {
		return false
	}
	clean := filepath.Clean(p)
	if clean == ".." || strings.HasPrefix(clean, "../") || strings.Contains(clean, "/../") {
		return false
	}
	return true
}

// runAgentLoop opens a WebSocket to /ws/agent, reads tool_call /
// chat_message envelopes, dispatches them appropriately, and sends
// back tool_result / chat_reply. Reconnects with exponential
// backoff on disconnect. spec is non-nil when running in
// passthrough mode (kimi/claude/codex/...); nil = tool-call loop.
func runAgentLoop(cfg AgentConfig, botID, workdir string, verbose bool, spec *toolSpec) error {
	backoff := 1 * time.Second
	for {
		err := runOneSession(cfg, botID, workdir, verbose, spec)
		if err == nil {
			return nil
		}
		log.Printf("session ended: %v — reconnect in %s", err, backoff)
		time.Sleep(backoff)
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

func runOneSession(cfg AgentConfig, botID, workdir string, verbose bool, spec *toolSpec) error {
	wsURL, err := buildWSURL(cfg.Server, cfg.Token, botID, workdir)
	if err != nil {
		return err
	}
	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	conn, _, err := dialer.Dial(wsURL, nil)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()
	log.Printf("connected to %s", strings.SplitN(wsURL, "?", 2)[0])

	// Serialize all writes — gorilla/websocket forbids concurrent
	// writers and we have multiple goroutines (one per tool_call /
	// chat_message) producing replies.
	var writeMu sync.Mutex
	send := func(payload []byte) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return conn.WriteMessage(websocket.TextMessage, payload)
	}

	hello := map[string]any{
		"kind":         "hello",
		"version":      "polar-agent 0.3",
		"workdir":      workdir,
		"capabilities": helloCapabilities(spec != nil),
		"host_os":      runtime.GOOS,
		"host_arch":    runtime.GOARCH,
		"host_name":    hostName(),
	}
	if spec != nil {
		hello["tool"] = spec.Name
	}
	if b, err := json.Marshal(hello); err == nil {
		_ = send(b)
	}

	// Host module Phase 0: immediately follow `hello` with
	// `skill.advertise`. Dock parses it and snapshots the kinds into
	// hosts.advertised_skills_json so the UI knows which skill cards to
	// render. In P0 the registry is empty (no skill implementations
	// are registered yet); the message still gets sent — empty list
	// proves the wire is up end-to-end. From P1 onward each skill
	// file's init() Register()s itself and the payload auto-populates.
	advertise := map[string]any{
		"kind":   "skill.advertise",
		"skills": skills.Default().Advertised(),
	}
	if b, err := json.Marshal(advertise); err == nil {
		_ = send(b)
	}

	conn.SetReadLimit(8 << 20)
	_ = conn.SetReadDeadline(time.Now().Add(90 * time.Second))
	conn.SetPingHandler(func(string) error {
		_ = conn.SetReadDeadline(time.Now().Add(90 * time.Second))
		return conn.WriteControl(websocket.PongMessage, nil, time.Now().Add(5*time.Second))
	})

	exec := newExecutor(workdir, verbose)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// P2: on WS disconnect, stop every in-flight Run (bash sessions,
	// future daemon-style skills). Without this an orphan bash could
	// keep running with no operator holding the other end.
	defer activeRuns.stopAll("agent_disconnect")

	// Periodic skill.advertise — keeps polar-hosts.last_seen_at fresh
	// while the WS stays open. Without this the only advertise frame is
	// the one above (right after hello), so a long-lived WS leaves the
	// UI thinking the host went idle. Cadence matches plugin heartbeat
	// (60s); the WS pings already cover liveness for the dock side.
	go func() {
		t := time.NewTicker(60 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				tick := map[string]any{
					"kind":   "skill.advertise",
					"skills": skills.Default().Advertised(),
				}
				b, err := json.Marshal(tick)
				if err != nil {
					continue
				}
				if err := send(b); err != nil {
					// WS likely dead; the read loop will return next and
					// trigger ctx.Done above, ending us cleanly.
					log.Printf("periodic skill.advertise send failed: %v", err)
					return
				}
			}
		}
	}()

	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return fmt.Errorf("read: %w", err)
		}
		var head struct {
			Kind string `json:"kind"`
		}
		if err := json.Unmarshal(raw, &head); err != nil {
			log.Printf("bad message: %v", err)
			continue
		}
		switch head.Kind {
		case "tool_call":
			var call toolCall
			if err := json.Unmarshal(raw, &call); err != nil {
				log.Printf("tool_call parse: %v", err)
				continue
			}
			go func(c toolCall) {
				result := exec.dispatch(ctx, c)
				if b, err := json.Marshal(result); err == nil {
					if err := send(b); err != nil {
						log.Printf("write tool_result: %v", err)
					}
				}
			}(call)
		case "chat_message":
			var msg chatMessage
			if err := json.Unmarshal(raw, &msg); err != nil {
				log.Printf("chat_message parse: %v", err)
				continue
			}
			if spec == nil {
				// Server shouldn't route a chat_message here without
				// the passthrough capability, but be defensive.
				emitChatReply(send, &chatReply{
					Kind:  "chat_reply",
					ID:    msg.ID,
					OK:    false,
					Error: "agent not started in passthrough mode (--tool=<name>)",
				})
				continue
			}
			go func(m chatMessage) {
				// Per-project workdir isolation: if the platform
				// supplies a subpath (project pickup does this with
				// the project id), spawn the tool inside
				// $workdir/<subpath> instead of $workdir. mkdir -p
				// happens here so the tool finds an empty dir on
				// first use; subsequent messages reuse the same
				// subdir, which is also what kimi-cli's --continue
				// keys on for session resume.
				effectiveWorkdir := workdir
				if sp := strings.TrimSpace(m.WorkdirSubpath); sp != "" {
					if !isSafeSubpath(sp) {
						emitChatReply(send, &chatReply{
							Kind:  "chat_reply",
							ID:    m.ID,
							OK:    false,
							Error: "invalid workdir_subpath",
						})
						return
					}
					sub := filepath.Join(workdir, sp)
					if err := os.MkdirAll(sub, 0o755); err != nil {
						emitChatReply(send, &chatReply{
							Kind:  "chat_reply",
							ID:    m.ID,
							OK:    false,
							Error: "mkdir " + sub + ": " + err.Error(),
						})
						return
					}
					effectiveWorkdir = sub
				}

				// Pre-task git plumbing — only when the platform
				// asked for it via git_remote_url. Failure here is
				// surfaced in the reply but doesn't abort the tool;
				// running a coder task on a not-yet-pushable repo
				// is still useful (output stays local).
				var preHEAD string
				var gitPrepErr error
				if remote := strings.TrimSpace(m.GitRemoteURL); remote != "" {
					log.Printf("[git] prepare workdir=%s remote=%s", effectiveWorkdir, remote)
					preHEAD, gitPrepErr = gitPrepare(ctx, effectiveWorkdir, remote)
					if gitPrepErr != nil {
						log.Printf("[git] prepare failed (continuing): %v", gitPrepErr)
					} else {
						log.Printf("[git] prepare ok preHEAD=%s", shortSHA(preHEAD))
					}
				}

				reply, err := invokeTool(ctx, spec, effectiveWorkdir, m, verbose)
				if err != nil {
					emitChatReply(send, &chatReply{
						Kind:  "chat_reply",
						ID:    m.ID,
						OK:    false,
						Error: err.Error(),
					})
					return
				}

				// Post-task git: stage tail, push. Only when the
				// platform set git_remote_url AND the pre step
				// succeeded enough to have a usable repo. Errors are
				// appended to the reply text — we never downgrade
				// reply.OK because git failed; the tool's output
				// is the user's value.
				if remote := strings.TrimSpace(m.GitRemoteURL); remote != "" && gitPrepErr == nil {
					commits, pushed, finErr := gitFinalize(ctx, effectiveWorkdir, preHEAD, "")
					log.Printf("[git] finalize commits=%d pushed=%v err=%v", len(commits), pushed, finErr)
					reply.Content += renderGitSummary(commits, pushed, finErr, remote)
				} else if gitPrepErr != nil {
					reply.Content += "\n\n---\n⚠️ git 准备失败：" + gitPrepErr.Error()
				}
				emitChatReply(send, reply)
			}(msg)
		case "research_task":
			var env researchTaskEnvelope
			if err := json.Unmarshal(raw, &env); err != nil {
				log.Printf("research_task parse: %v", err)
				continue
			}
			go func(e researchTaskEnvelope) {
				if err := runResearchTask(ctx, cfg, workdir, e, verbose); err != nil {
					log.Printf("[research] run=%d error: %v", e.ResearchRunID, err)
				}
			}(env)
		case "skill.start":
			// Host module P1c: dock dispatches a skill.start envelope
			// for any bot bound to a host_skill row. The skill's
			// registered Start() returns a Run whose Events() we pump
			// back to dock as skill.event frames keyed by run_id.
			var env skillStartEnvelope
			if err := json.Unmarshal(raw, &env); err != nil {
				log.Printf("skill.start parse: %v", err)
				continue
			}
			log.Printf("[agent] skill.start received run=%d kind=%s", env.RunID, env.SkillKind)
			skill, ok := skills.Default().Get(skills.SkillKind(env.SkillKind))
			if !ok {
				log.Printf("skill.start run=%d: kind=%q not registered", env.RunID, env.SkillKind)
				sendSkillEvent(send, env.RunID, skills.EventExit, map[string]any{
					"ok":    false,
					"error": "skill kind not registered on this agent: " + env.SkillKind,
				})
				continue
			}
			go func(e skillStartEnvelope, s skills.Skill) {
				log.Printf("[agent] skill.Start invoking run=%d kind=%s config=%s", e.RunID, e.SkillKind, truncate(string(e.Config), 200))
				run, err := s.Start(ctx, e.RunID, e.Config)
				if err != nil {
					log.Printf("skill.start run=%d kind=%s start error: %v", e.RunID, e.SkillKind, err)
					sendSkillEvent(send, e.RunID, skills.EventExit, map[string]any{
						"ok":    false,
						"error": err.Error(),
					})
					return
				}
				// P2 (Shell): register the Run handle so skill.stdin /
				// skill.resize / skill.stop envelopes can find it.
				// Coder Runs don't satisfy RunInput/RunResizer; that's
				// fine — those envelopes will type-assert and skip.
				log.Printf("[agent] skill.Start ok run=%d kind=%s — pumping events", e.RunID, e.SkillKind)
				activeRuns.putWithKind(e.RunID, run, e.SkillKind)
				defer activeRuns.delete(e.RunID)
				for ev := range run.Events() {
					sendSkillEvent(send, e.RunID, ev.Kind, ev.Data)
				}
				log.Printf("[agent] skill.Start exited run=%d kind=%s", e.RunID, e.SkillKind)
			}(env, skill)
		case "skill.stdin":
			// P2 (Shell): dock forwards a chunk of stdin bytes from
			// the operator's browser. Type-assert to RunInput; silently
			// drop for Runs that don't implement it.
			var env skillStdinEnvelope
			if err := json.Unmarshal(raw, &env); err != nil {
				log.Printf("skill.stdin parse: %v", err)
				continue
			}
			run, ok := activeRuns.get(env.RunID)
			if !ok {
				continue
			}
			input, ok := run.(skills.RunInput)
			if !ok {
				continue
			}
			data, err := base64.StdEncoding.DecodeString(env.BytesB64)
			if err != nil {
				log.Printf("skill.stdin run=%d base64 decode: %v", env.RunID, err)
				continue
			}
			if err := input.WriteInput(data); err != nil {
				log.Printf("skill.stdin run=%d WriteInput: %v", env.RunID, err)
			}
		case "skill.resize":
			// P2 (Shell): operator's xterm resized. Type-assert to
			// RunResizer; non-resizable Runs ignore.
			var env skillResizeEnvelope
			if err := json.Unmarshal(raw, &env); err != nil {
				log.Printf("skill.resize parse: %v", err)
				continue
			}
			run, ok := activeRuns.get(env.RunID)
			if !ok {
				continue
			}
			resizer, ok := run.(skills.RunResizer)
			if !ok {
				continue
			}
			if err := resizer.Resize(env.Rows, env.Cols); err != nil {
				log.Printf("skill.resize run=%d: %v", env.RunID, err)
			}
		case "skill.stop":
			// P2: explicit shutdown (browser closed, kicked by admin,
			// or operator stop button). Run.Stop is idempotent.
			var env skillStopEnvelope
			if err := json.Unmarshal(raw, &env); err != nil {
				log.Printf("skill.stop parse: %v", err)
				continue
			}
			run, ok := activeRuns.get(env.RunID)
			if !ok {
				continue
			}
			_ = run.Stop(env.Reason)
		case "skill.install":
			// P1a: dock asks agent to install a catalog bundle into
			// ~/.polar/bundles/. Result reported via skill.install.result.
			var req skills.InstallRequest
			if err := json.Unmarshal(raw, &req); err != nil {
				log.Printf("skill.install parse: %v", err)
				continue
			}
			log.Printf("[agent] skill.install received id=%s %s/%s@%s", req.InstallID, req.Publisher, req.SkillKind, req.Version)
			go func(r skills.InstallRequest) {
				inst := getInstaller()
				if inst == nil {
					log.Printf("skill.install: bundle skill not registered, cannot install")
					return
				}
				res := inst.Install(ctx, r)
				log.Printf("[agent] skill.install id=%s status=%s err=%q", res.InstallID, res.Status, res.Error)
				sendInstallResult(send, "skill.install.result", res)
				// Trigger an immediate advertise so dock sees the new
				// installed bundle without waiting for the 60s tick.
				if res.Status == skills.InstallStatusOK || res.Status == skills.InstallStatusAlreadyInstalled {
					tick := map[string]any{
						"kind":   "skill.advertise",
						"skills": skills.Default().Advertised(),
					}
					if b, err := json.Marshal(tick); err == nil {
						_ = send(b)
					}
				}
			}(req)
		case "skill.uninstall":
			// P1a: dock asks agent to remove an installed bundle.
			var req skills.UninstallRequest
			if err := json.Unmarshal(raw, &req); err != nil {
				log.Printf("skill.uninstall parse: %v", err)
				continue
			}
			log.Printf("[agent] skill.uninstall received id=%s %s/%s@%s force=%v",
				req.InstallID, req.Publisher, req.SkillKind, req.Version, req.Force)
			go func(r skills.UninstallRequest) {
				inst := getInstaller()
				if inst == nil {
					return
				}
				res := inst.Uninstall(r, activeRuns.kindsByRunID())
				log.Printf("[agent] skill.uninstall id=%s status=%s removed_runs=%d err=%q",
					res.InstallID, res.Status, res.RemovedRuns, res.Error)
				sendUninstallResult(send, "skill.uninstall.result", res)
				if res.Status == skills.InstallStatusOK {
					tick := map[string]any{
						"kind":   "skill.advertise",
						"skills": skills.Default().Advertised(),
					}
					if b, err := json.Marshal(tick); err == nil {
						_ = send(b)
					}
				}
			}(req)
		case "ping":
			// app-level ping (websocket protocol pings handled above)
		default:
			log.Printf("unknown kind=%s", head.Kind)
		}
	}
}

// skillStartEnvelope is the dock → agent dispatch frame for the
// Host module's skill subsystem. `config` is forwarded verbatim into
// Skill.Start; each skill kind owns the schema of its own config blob.
type skillStartEnvelope struct {
	Kind      string          `json:"kind"`
	RunID     int64           `json:"run_id"`
	SkillKind string          `json:"skill_kind"`
	Config    json.RawMessage `json:"config"`
}

// sendSkillEvent marshals a skill.event frame and writes it via the
// caller's serialized send func. Failures log + drop rather than
// propagating: the runner goroutine has no good way to surface a write
// error, and the WS read loop will catch a dead socket on its next
// ReadMessage call anyway.
func sendSkillEvent(send func([]byte) error, runID int64, kind skills.EventKind, data map[string]any) {
	frame := map[string]any{
		"kind":       "skill.event",
		"run_id":     runID,
		"event_kind": string(kind),
		"data":       data,
	}
	b, err := json.Marshal(frame)
	if err != nil {
		log.Printf("skill.event run=%d marshal: %v", runID, err)
		return
	}
	if err := send(b); err != nil {
		log.Printf("skill.event run=%d write: %v", runID, err)
	}
}

func helloCapabilities(passthrough bool) []string {
	caps := []string{"tools"}
	if passthrough {
		// "passthrough" is the new generic capability. "kimi" is
		// kept alongside for backward compat with platforms that
		// haven't been updated yet — they keyed routing on the
		// literal "kimi" string.
		caps = append(caps, "passthrough", "kimi")
	}
	return caps
}

func buildWSURL(server, token, botID, workdir string) (string, error) {
	u, err := url.Parse(strings.TrimRight(server, "/") + "/ws/agent")
	if err != nil {
		return "", err
	}
	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	}
	q := u.Query()
	q.Set("token", token)
	q.Set("bot_id", botID)
	q.Set("workdir", workdir)
	u.RawQuery = q.Encode()
	return u.String(), nil
}
