package main

// test_server.go — `polar-agent test-serve` subcommand.
//
// Standalone skill testbed. NO dock connection, NO bot user, NO host
// token. A TCP listener on loopback speaks the same envelope JSON as
// the dock↔agent WS, so any skill that works here works on dock with
// zero changes. Companion CLI is cmd/polar-agent-test/ (Phase B).
//
// Wire format: NDJSON — one envelope per line. Same shape as dock:
//   client → agent: skill.start | skill.stdin | skill.resize | skill.stop
//                   plus test-only: ping | list
//   agent → client: skill.event
//                   plus test-only: pong | skills
//
// SECURITY: bound to 127.0.0.1 only. There's no auth — anyone who can
// dial the local socket can spawn shells / VNC relays / etc. Dev mode
// only. See doc/agent/agent-test-harness.md for the threat model.

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"sync"
	"time"

	"github.com/networkextension/polar-agent/cmd/polar-agent/skills"
)

func decodeBytesB64(s string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(s)
}

const (
	testServerDefaultAddr = "127.0.0.1:7077"
)

// runTestServer is the entry point for `polar-agent test-serve`. Spins
// up the loopback listener, accepts connections, dispatches envelopes
// per connection.
func runTestServer(args []string) int {
	fs := flag.NewFlagSet("test-serve", flag.ContinueOnError)
	addr := fs.String("addr", "", "bind address (default 127.0.0.1:7077; override via POLAR_AGENT_TEST_ADDR)")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	bind := *addr
	if bind == "" {
		bind = os.Getenv("POLAR_AGENT_TEST_ADDR")
	}
	if bind == "" {
		bind = testServerDefaultAddr
	}
	// Refuse to bind a non-loopback address — the protocol has no auth.
	if !isLoopbackBind(bind) {
		fmt.Fprintf(os.Stderr, "test-serve: refusing non-loopback bind %q. Use 127.0.0.1:<port> or [::1]:<port>.\n", bind)
		return exitUsage
	}

	// Register all skills the same way `attach` does, but unconditional
	// — test mode wants everything available regardless of the
	// POLAR_AGENT_*_DISABLED env vars an operator might have on dev box.
	workdir, _ := os.Getwd()
	if workdir == "" {
		workdir = os.TempDir()
	}
	registerSkillsForTestServer(workdir)

	ln, err := net.Listen("tcp", bind)
	if err != nil {
		fmt.Fprintf(os.Stderr, "test-serve: listen %s: %v\n", bind, err)
		return exitNet
	}
	defer ln.Close()

	log.Printf("[test-serve] listening on %s — WARNING NO AUTH, dev use only", bind)
	log.Printf("[test-serve] registered skills: %v", listSkillKinds())

	for {
		conn, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return exitOK
			}
			log.Printf("[test-serve] accept: %v", err)
			continue
		}
		go handleTestConn(conn)
	}
}

// isLoopbackBind returns true when bind targets 127.0.0.0/8 or [::1].
// Anything else is rejected.
func isLoopbackBind(bind string) bool {
	host, _, err := net.SplitHostPort(bind)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback()
}

// registerSkillsForTestServer registers every shippable skill regardless
// of the per-skill *_DISABLED env vars. test-serve's whole purpose is
// to exercise skill code paths, so honoring the disable knobs would
// defeat it.
func registerSkillsForTestServer(workdir string) {
	// Skills register via package-level Register() into a global registry.
	// Skip re-registration if a prior subcommand call (in unusual embed
	// scenarios) already populated the registry.
	already := map[string]bool{}
	for _, sk := range skills.Default().Advertised() {
		already[sk.Kind] = true
	}
	tryRegister := func(s skills.Skill) {
		if s == nil {
			return
		}
		if already[string(s.Kind())] {
			return
		}
		skills.Register(s)
	}
	// Coder needs a runner — test mode skips it (coder's external
	// processes are heavy + need git config); a future scenario can
	// exercise coder.go directly via Go tests.
	tryRegister(skills.NewShellSkill(workdir))
	tryRegister(skills.NewProxySkill())
	tryRegister(skills.NewWireGuardSkill())
	tryRegister(skills.NewKDPSkill())
	tryRegister(skills.NewVncSkill())
	tryRegister(skills.NewMCPServerSkill())
	// Bundle skill — honor POLAR_AGENT_BUNDLES_DIR (set by orchestrator
	// to an isolated tmp dir so scenarios don't poison the operator's
	// ~/.polar/bundles/). Empty falls back to the default home path
	// inside NewBundleSkill.
	tryRegister(skills.NewBundleSkill(os.Getenv("POLAR_AGENT_BUNDLES_DIR")))
}

func listSkillKinds() []string {
	out := []string{}
	for _, sk := range skills.Default().Advertised() {
		out = append(out, sk.Kind)
	}
	return out
}

// handleTestConn services one client connection: reads envelopes,
// dispatches, pumps events back.
func handleTestConn(conn net.Conn) {
	defer conn.Close()
	log.Printf("[test-serve] client %s connected", conn.RemoteAddr())
	defer log.Printf("[test-serve] client %s disconnected", conn.RemoteAddr())

	// Serialize all writes through one goroutine via a channel so the
	// reader goroutine + per-run event pumpers don't interleave.
	out := make(chan []byte, 256)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go writeOutgoing(ctx, conn, out)

	sender := func(b []byte) error {
		// Append newline framing.
		framed := make([]byte, 0, len(b)+1)
		framed = append(framed, b...)
		framed = append(framed, '\n')
		select {
		case out <- framed:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	// Per-run tracking — needed for skill.stdin / skill.resize / skill.stop
	// to find the right Run handle.
	runs := newTestRunRegistry()

	startedAt := time.Now()
	reader := bufio.NewReader(conn)
	for {
		// NDJSON: one full envelope per line. Read the line raw so we
		// can do a two-pass parse — peek at kind, then unmarshal the
		// payload again with the matching struct.
		line, err := reader.ReadBytes('\n')
		if err != nil {
			return
		}
		line = trimTrailingNewline(line)
		if len(line) == 0 {
			continue
		}
		var head struct {
			Kind string `json:"kind"`
		}
		if err := json.Unmarshal(line, &head); err != nil {
			sendJSON(sender, map[string]any{"kind": "error", "error": "malformed JSON envelope"})
			continue
		}
		switch head.Kind {
		case "ping":
			sendJSON(sender, map[string]any{
				"kind":      "pong",
				"version":   "test-serve/1.0",
				"uptime_ms": time.Since(startedAt).Milliseconds(),
				"skills":    listSkillKinds(),
			})
		case "list":
			sendJSON(sender, map[string]any{
				"kind":   "skills",
				"skills": skills.Default().Advertised(),
			})
		case "skill.start":
			handleTestSkillStart(ctx, sender, runs, line)
		case "skill.stdin":
			handleTestSkillStdin(sender, runs, line)
		case "skill.resize":
			handleTestSkillResize(sender, runs, line)
		case "skill.stop":
			handleTestSkillStop(sender, runs, line)
		default:
			sendJSON(sender, map[string]any{
				"kind":  "error",
				"error": "unknown envelope kind: " + head.Kind,
			})
		}
	}
}

func trimTrailingNewline(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r') {
		b = b[:len(b)-1]
	}
	return b
}

func handleTestSkillStart(ctx context.Context, send func([]byte) error, runs *testRunRegistry, raw []byte) {
	var env skillStartEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		sendJSON(send, map[string]any{"kind": "error", "error": "skill.start parse: " + err.Error(), "raw": string(raw)})
		return
	}
	skill, ok := skills.Default().Get(skills.SkillKind(env.SkillKind))
	if !ok {
		sendJSON(send, map[string]any{
			"kind":  "skill.event",
			"run_id": env.RunID,
			"event_kind": "exit",
			"data": map[string]any{"ok": false, "error": "skill kind not registered: " + env.SkillKind},
		})
		return
	}
	run, err := skill.Start(ctx, env.RunID, env.Config)
	if err != nil {
		sendJSON(send, map[string]any{
			"kind":       "skill.event",
			"run_id":     env.RunID,
			"event_kind": "exit",
			"data":       map[string]any{"ok": false, "error": err.Error()},
		})
		return
	}
	runs.put(env.RunID, run)
	go func() {
		defer runs.delete(env.RunID)
		for ev := range run.Events() {
			sendSkillEvent(send, env.RunID, ev.Kind, ev.Data)
		}
	}()
}

func handleTestSkillStdin(send func([]byte) error, runs *testRunRegistry, raw []byte) {
	var env skillStdinEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		sendJSON(send, map[string]any{"kind": "error", "error": "skill.stdin parse: " + err.Error()})
		return
	}
	run, ok := runs.get(env.RunID)
	if !ok {
		sendJSON(send, map[string]any{"kind": "error", "error": fmt.Sprintf("skill.stdin: run %d not found", env.RunID)})
		return
	}
	input, ok := run.(skills.RunInput)
	if !ok {
		return // Run doesn't accept stdin (e.g. Coder); silent no-op matches dock behavior.
	}
	data, err := decodeBytesB64(env.BytesB64)
	if err != nil {
		sendJSON(send, map[string]any{"kind": "error", "error": "skill.stdin b64: " + err.Error()})
		return
	}
	_ = input.WriteInput(data)
}

func handleTestSkillResize(send func([]byte) error, runs *testRunRegistry, raw []byte) {
	var env skillResizeEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return
	}
	run, ok := runs.get(env.RunID)
	if !ok {
		return
	}
	if resizer, ok := run.(skills.RunResizer); ok {
		_ = resizer.Resize(env.Rows, env.Cols)
	}
}

func handleTestSkillStop(send func([]byte) error, runs *testRunRegistry, raw []byte) {
	var env skillStopEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return
	}
	run, ok := runs.get(env.RunID)
	if !ok {
		return
	}
	_ = run.Stop(env.Reason)
}

// writeOutgoing pumps the out channel to the TCP conn. Single writer
// keeps frames from interleaving.
func writeOutgoing(ctx context.Context, conn net.Conn, out <-chan []byte) {
	for {
		select {
		case <-ctx.Done():
			return
		case b, ok := <-out:
			if !ok {
				return
			}
			_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if _, err := conn.Write(b); err != nil {
				return
			}
		}
	}
}

func sendJSON(send func([]byte) error, payload map[string]any) {
	b, err := json.Marshal(payload)
	if err != nil {
		log.Printf("[test-serve] marshal: %v", err)
		return
	}
	if err := send(b); err != nil {
		log.Printf("[test-serve] send: %v", err)
	}
}

// testRunRegistry tracks active skill runs per connection so subsequent
// stdin/resize/stop envelopes find the right Run.
type testRunRegistry struct {
	mu   sync.Mutex
	runs map[int64]skills.Run
}

func newTestRunRegistry() *testRunRegistry {
	return &testRunRegistry{runs: map[int64]skills.Run{}}
}

func (r *testRunRegistry) put(id int64, run skills.Run) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.runs[id] = run
}

func (r *testRunRegistry) get(id int64) (skills.Run, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	run, ok := r.runs[id]
	return run, ok
}

func (r *testRunRegistry) delete(id int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.runs, id)
}
