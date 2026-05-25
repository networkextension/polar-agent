package main

// Scenarios — named end-to-end checks that each exercise one skill.
// Each scenario.run gets a fresh connection + context; on return nil
// the scenario passes. Errors include enough detail to debug without
// re-running.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

type scenario struct {
	name string
	desc string
	run  func(ctx context.Context, c *client) error
}

var scenarios = []scenario{
	{
		name: "ping",
		desc: "round-trip the ping/pong envelope",
		run:  scenarioPing,
	},
	{
		name: "list",
		desc: "request skill catalogue + verify shape",
		run:  scenarioList,
	},
	{
		name: "shell-echo",
		desc: "start shell, type 'echo hi; exit\\n', expect 'hi' in stdout + exit event",
		run:  scenarioShellEcho,
	},
	{
		name: "shell-stop",
		desc: "start shell, send skill.stop, expect exit event with operator reason",
		run:  scenarioShellStop,
	},
	{
		name: "vnc-banner",
		desc: "start vnc against 127.0.0.1:5900, expect 'RFB ' banner if Screen Sharing is on (skip-style if dial fails)",
		run:  scenarioVNCBanner,
	},
	{
		name: "unknown-skill",
		desc: "start a non-registered skill kind, expect exit event with error",
		run:  scenarioUnknownSkill,
	},
	// --- Shell edge cases ---
	{
		name: "shell-cwd",
		desc: "spawn shell with cwd=/tmp, expect pwd output to be /tmp",
		run:  scenarioShellCWD,
	},
	{
		name: "shell-env",
		desc: "spawn shell with env FOO=bar, expect echo $FOO → bar",
		run:  scenarioShellEnv,
	},
	{
		name: "shell-env-stripped",
		desc: "attempt to set LD_PRELOAD via env, expect it to be rejected at Validate",
		run:  scenarioShellEnvStripped,
	},
	{
		name: "shell-resize",
		desc: "spawn shell at 24x80, resize to 12x40, expect stty size to reflect",
		run:  scenarioShellResize,
	},
	{
		name: "shell-large-input",
		desc: "pipe 16 KiB of stdin, expect agent reads it intact (no chunk loss)",
		run:  scenarioShellLargeInput,
	},
	{
		name: "shell-multi-run",
		desc: "3 concurrent shell runs on one connection, each independent",
		run:  scenarioShellMultiRun,
	},
	{
		name: "shell-bad-cwd",
		desc: "spawn shell with cwd=/no/such/dir, expect start to fail at Validate",
		run:  scenarioShellBadCWD,
	},
	{
		name: "shell-bad-shell",
		desc: "spawn shell with shell=fish (not whitelisted), expect agent to fall back to auto-pick (warning not error)",
		run:  scenarioShellBadShell,
	},
	// --- VNC edge cases ---
	{
		name: "vnc-bad-target",
		desc: "vnc target='not a host', expect skill.Start to error (validation)",
		run:  scenarioVNCBadTarget,
	},
	{
		name: "vnc-public-rejected",
		desc: "vnc target=8.8.8.8:5900, expect rejected (loopback/private/mDNS whitelist)",
		run:  scenarioVNCPublicRejected,
	},
	{
		name: "vnc-dial-refused",
		desc: "vnc target=127.0.0.1:1 (closed port), expect quick dial failure",
		run:  scenarioVNCDialRefused,
	},
	// --- VNC with mock server (no macOS Screen Sharing required) ---
	{
		name: "vnc-mock-banner",
		desc: "mock RFB server sends banner, agent forwards 'RFB 003.008\\n' verbatim",
		run:  scenarioVNCMockBanner,
	},
	{
		name: "vnc-mock-echo",
		desc: "send bytes via skill.stdin, mock server echoes, agent forwards back to stdout",
		run:  scenarioVNCMockEcho,
	},
	{
		name: "vnc-mock-server-close",
		desc: "mock server closes after 4 bytes received; agent reader sees EOF, emits exit",
		run:  scenarioVNCMockServerClose,
	},
	{
		name: "vnc-mock-multi-viewer",
		desc: "start 2 vnc runs to the same mock target, each gets its own banner (parallel TCP)",
		run:  scenarioVNCMockMultiViewer,
	},
	// --- MCP server skill (P0a — scaffold + built-in echo tool) ---
	{
		name: "mcp-initialize",
		desc: "JSON-RPC initialize → result with protocolVersion + serverInfo + capabilities.tools",
		run:  scenarioMCPInitialize,
	},
	{
		name: "mcp-tools-list",
		desc: "JSON-RPC tools/list → result.tools[] contains 'echo' with inputSchema",
		run:  scenarioMCPToolsList,
	},
	{
		name: "mcp-tools-call-echo",
		desc: "JSON-RPC tools/call echo {message:'hi'} → result.content[0].text == 'hi'",
		run:  scenarioMCPToolsCallEcho,
	},
	{
		name: "mcp-bad-method",
		desc: "JSON-RPC unknown_method → error.code=-32601 (Method not found)",
		run:  scenarioMCPBadMethod,
	},
	{
		name: "mcp-parse-error",
		desc: "malformed JSON line → error.code=-32700 (Parse error)",
		run:  scenarioMCPParseError,
	},
	// --- MCP P0b — adapter framework + stub adapter ---
	{
		name: "mcp-adapter-default",
		desc: "no adapter config → defaults to ['echo']; tools/list contains echo only",
		run:  scenarioMCPAdapterDefault,
	},
	{
		name: "mcp-adapter-stub-only",
		desc: "adapters:['stub'] → tools/list shows stub.add / stub.error / stub.delay_ms, no echo",
		run:  scenarioMCPAdapterStubOnly,
	},
	{
		name: "mcp-adapter-multi",
		desc: "adapters:['echo','stub'] → tools/list contains both adapters' tools",
		run:  scenarioMCPAdapterMulti,
	},
	{
		name: "mcp-stub-add",
		desc: "stub.add {a:3,b:4} → result with sum=7",
		run:  scenarioMCPStubAdd,
	},
	{
		name: "mcp-stub-error",
		desc: "stub.error → JSON-RPC error code -32000 (tool execution failure)",
		run:  scenarioMCPStubError,
	},
	{
		name: "mcp-adapter-unknown",
		desc: "adapters:['nonesuch'] → run starts with empty catalogue + skipped_adapters in state",
		run:  scenarioMCPAdapterUnknown,
	},
	// --- MCP P0c — persistent sessions in the framework ---
	{
		name: "mcp-session-create",
		desc: "stub.open_session → session_id returned + appears in mcp.list_sessions",
		run:  scenarioMCPSessionCreate,
	},
	{
		name: "mcp-session-close",
		desc: "mcp.close_session by id → session removed from list",
		run:  scenarioMCPSessionClose,
	},
	{
		name: "mcp-session-bad-id",
		desc: "mcp.close_session with unknown id → JSON-RPC error",
		run:  scenarioMCPSessionBadID,
	},
	{
		name: "mcp-session-idle-evict",
		desc: "POLAR_MCP_SESSION_IDLE_SEC=1 + sleep > 30s tick → session auto-evicted",
		run:  scenarioMCPSessionIdleEvict,
	},
	// --- Bundle skill scenarios ---
	// These don't require dock — fixtures are either pre-staged on disk
	// (POLAR_AGENT_BUNDLES_DIR is propagated by the orchestrator) or
	// served from an in-process httptest server bound to 127.0.0.1.
	{
		name: "bundle-list",
		desc: "skill.list advertises kind=bundle with installed=[] in capabilities",
		run:  scenarioBundleList,
	},
	{
		name: "bundle-exec-shell",
		desc: "pre-staged .sh entrypoint, no download_url → exec succeeds, stdout line streamed, exit ok",
		run:  scenarioBundleExecShell,
	},
	{
		name: "bundle-exec-stderr-stream",
		desc: "entrypoint writes to stderr → EventLog{stream:stderr} received",
		run:  scenarioBundleExecStderrStream,
	},
	{
		name: "bundle-exec-nonzero-exit",
		desc: "entrypoint exits 7 → EventExit ok=false exit_code=7",
		run:  scenarioBundleExecNonzeroExit,
	},
	{
		name: "bundle-bad-name",
		desc: "skill.start with bundle=\"../evil\" → immediate exit event with validation error",
		run:  scenarioBundleBadName,
	},
	{
		name: "bundle-bad-entrypoint",
		desc: "entrypoint outside scripts/ → exit event with validation error",
		run:  scenarioBundleBadEntrypoint,
	},
	{
		name: "bundle-bad-env",
		desc: "env LD_PRELOAD → exit event with validation error",
		run:  scenarioBundleBadEnv,
	},
	{
		name: "bundle-missing-no-download",
		desc: "bundle not on disk + download_url empty → exit event with install error",
		run:  scenarioBundleMissingNoDownload,
	},
	{
		name: "bundle-http-install",
		desc: "httptest server serves a .skill ZIP → agent downloads, sha256-verifies, unzips, runs",
		run:  scenarioBundleHTTPInstall,
	},
	{
		name: "bundle-http-sha256-mismatch",
		desc: "httptest serves a ZIP but config carries wrong sha256 → exit event with mismatch error",
		run:  scenarioBundleHTTPSHA256Mismatch,
	},
	{
		name: "bundle-stop",
		desc: "long-running entrypoint, send skill.stop → exit event arrives in <2s",
		run:  scenarioBundleStop,
	},
	{
		name: "bundle-cached-skips-download",
		desc: "pre-staged + bogus download_url → succeeds, proves no HTTP call on cache hit",
		run:  scenarioBundleCachedSkipsDownload,
	},
}

func scenarioPing(ctx context.Context, c *client) error {
	if err := c.send(envelope{"kind": "ping"}); err != nil {
		return fmt.Errorf("send ping: %w", err)
	}
	env, err := c.waitFor(3*time.Second, "pong")
	if err != nil {
		return err
	}
	version, _ := env["version"].(string)
	if !strings.HasPrefix(version, "test-serve/") {
		return fmt.Errorf("unexpected version: %q", version)
	}
	return nil
}

func scenarioList(ctx context.Context, c *client) error {
	if err := c.send(envelope{"kind": "list"}); err != nil {
		return err
	}
	env, err := c.waitFor(3*time.Second, "skills")
	if err != nil {
		return err
	}
	rawSkills, _ := env["skills"].([]any)
	if len(rawSkills) == 0 {
		return fmt.Errorf("expected ≥1 skills, got 0")
	}
	// At minimum shell + vnc should always register on unix.
	have := map[string]bool{}
	for _, s := range rawSkills {
		if m, ok := s.(map[string]any); ok {
			if k, ok := m["kind"].(string); ok {
				have[k] = true
			}
		}
	}
	for _, must := range []string{"shell", "vnc"} {
		if !have[must] {
			return fmt.Errorf("missing required skill: %s (have %v)", must, have)
		}
	}
	return nil
}

func scenarioShellEcho(ctx context.Context, c *client) error {
	const runID = 100
	cfg, _ := json.Marshal(map[string]any{"rows": 24, "cols": 80, "idle_timeout_sec": 60})
	if err := c.send(envelope{
		"kind":       "skill.start",
		"run_id":     runID,
		"skill_kind": "shell",
		"config":     json.RawMessage(cfg),
	}); err != nil {
		return err
	}
	// Brief settle — let the agent dispatch + register the Run.
	time.Sleep(200 * time.Millisecond)
	if err := c.send(envelope{
		"kind":      "skill.stdin",
		"run_id":    runID,
		"bytes_b64": encodeBytes([]byte("echo polar-test-marker; exit\n")),
	}); err != nil {
		return err
	}
	exitEv, stdout, err := c.collectSkillEvent(runID, 10*time.Second, "exit")
	if err != nil {
		return fmt.Errorf("collect: %w (stdout so far: %q)", err, string(stdout))
	}
	if !strings.Contains(string(stdout), "polar-test-marker") {
		return fmt.Errorf("missing marker in stdout: %q", string(stdout))
	}
	if exitEv == nil {
		return fmt.Errorf("no exit event")
	}
	return nil
}

func scenarioShellStop(ctx context.Context, c *client) error {
	const runID = 101
	cfg, _ := json.Marshal(map[string]any{"rows": 24, "cols": 80})
	if err := c.send(envelope{
		"kind":       "skill.start",
		"run_id":     runID,
		"skill_kind": "shell",
		"config":     json.RawMessage(cfg),
	}); err != nil {
		return err
	}
	time.Sleep(200 * time.Millisecond)
	if err := c.send(envelope{
		"kind":   "skill.stop",
		"run_id": runID,
		"reason": "test_stop",
	}); err != nil {
		return err
	}
	exitEv, _, err := c.collectSkillEvent(runID, 5*time.Second, "exit")
	if err != nil {
		return err
	}
	data, _ := exitEv["data"].(map[string]any)
	reason, _ := data["reason"].(string)
	// Reader-loop sees pty close and reports "eof" — that's fine; the
	// stop reason gets passed through but the reader can race the
	// stop() call. Accept either.
	if reason != "test_stop" && reason != "eof" {
		return fmt.Errorf("unexpected exit reason: %q (want test_stop or eof)", reason)
	}
	return nil
}

func scenarioVNCBanner(ctx context.Context, c *client) error {
	const runID = 200
	// Defaults to 127.0.0.1:5900. If macOS Screen Sharing isn't on,
	// the agent's net.Dial will succeed against launchd's lazy
	// listener but bash never speaks RFB → we'll time out. Treat that
	// as a soft skip rather than a hard fail since it's an environment
	// dependency, not a code dependency.
	if err := c.send(envelope{
		"kind":       "skill.start",
		"run_id":     runID,
		"skill_kind": "vnc",
		"config":     json.RawMessage("{}"),
	}); err != nil {
		return err
	}
	ev, stdout, err := c.collectSkillEvent(runID, 4*time.Second, "exit", "state")
	if err != nil {
		// Likely no VNC server reachable; soft pass.
		return fmt.Errorf("no event within 4s — Screen Sharing off? (skipping as env dep): %w", err)
	}
	// Stop the run cleanly regardless of what we saw.
	defer c.send(envelope{"kind": "skill.stop", "run_id": runID, "reason": "scenario_done"})
	// Look for the RFB banner; might land in stdout collected above or
	// might still be in-flight. Wait a beat for at least 12 bytes.
	deadline := time.Now().Add(3 * time.Second)
	for len(stdout) < 12 && time.Now().Before(deadline) {
		_, more, err := c.collectSkillEvent(runID, 500*time.Millisecond, "exit")
		stdout = append(stdout, more...)
		if err == nil {
			break
		}
	}
	if !strings.HasPrefix(string(stdout), "RFB ") {
		return fmt.Errorf("expected RFB banner; got %q (ev kind=%v)", string(stdout), ev["event_kind"])
	}
	return nil
}

// startShellRun is a helper that dispatches skill.start kind=shell with
// the given config, waits 200ms for the agent to register the run, and
// returns. The runID is given by the caller so concurrent scenarios in
// one connection can pick non-clashing IDs.
func startShellRun(c *client, runID int64, configMap map[string]any) error {
	cfg, _ := json.Marshal(configMap)
	if err := c.send(envelope{
		"kind":       "skill.start",
		"run_id":     runID,
		"skill_kind": "shell",
		"config":     json.RawMessage(cfg),
	}); err != nil {
		return err
	}
	// Let the agent dispatch + register the Run before subsequent stdin.
	time.Sleep(200 * time.Millisecond)
	return nil
}

func sendStdin(c *client, runID int64, data []byte) error {
	return c.send(envelope{
		"kind":      "skill.stdin",
		"run_id":    runID,
		"bytes_b64": encodeBytes(data),
	})
}

func scenarioShellCWD(ctx context.Context, c *client) error {
	const runID = 110
	if err := startShellRun(c, runID, map[string]any{
		"rows": 24, "cols": 80,
		"cwd": "/tmp",
	}); err != nil {
		return err
	}
	if err := sendStdin(c, runID, []byte("pwd; exit\n")); err != nil {
		return err
	}
	_, stdout, err := c.collectSkillEvent(runID, 5*time.Second, "exit")
	if err != nil {
		return fmt.Errorf("collect: %w (stdout: %q)", err, string(stdout))
	}
	// Match "/tmp" or "/private/tmp" — macOS resolves /tmp to /private/tmp.
	if !strings.Contains(string(stdout), "/tmp") {
		return fmt.Errorf("expected '/tmp' in pwd output, got: %q", string(stdout))
	}
	return nil
}

func scenarioShellEnv(ctx context.Context, c *client) error {
	const runID = 111
	if err := startShellRun(c, runID, map[string]any{
		"rows": 24, "cols": 80,
		"env": map[string]string{"POLAR_TEST_FOO": "marker-value-7g3q"},
	}); err != nil {
		return err
	}
	if err := sendStdin(c, runID, []byte("echo $POLAR_TEST_FOO; exit\n")); err != nil {
		return err
	}
	_, stdout, err := c.collectSkillEvent(runID, 5*time.Second, "exit")
	if err != nil {
		return err
	}
	if !strings.Contains(string(stdout), "marker-value-7g3q") {
		return fmt.Errorf("env var didn't reach shell — stdout: %q", string(stdout))
	}
	return nil
}

func scenarioShellEnvStripped(ctx context.Context, c *client) error {
	// shell.go's buildShellEnv silently strips LD_PRELOAD (and other
	// dyld/ld_* keys) from operator-supplied env. So start succeeds
	// but the var must NOT reach the shell. Verify by echoing it.
	const runID = 112
	if err := startShellRun(c, runID, map[string]any{
		"env": map[string]string{"LD_PRELOAD": "/tmp/evil.so"},
	}); err != nil {
		return err
	}
	if err := sendStdin(c, runID, []byte("echo \"LD=[$LD_PRELOAD]\"; exit\n")); err != nil {
		return err
	}
	_, stdout, err := c.collectSkillEvent(runID, 5*time.Second, "exit")
	if err != nil {
		return err
	}
	if strings.Contains(string(stdout), "evil.so") {
		return fmt.Errorf("LD_PRELOAD leaked into shell env: %q", string(stdout))
	}
	return nil
}

func scenarioShellResize(ctx context.Context, c *client) error {
	const runID = 114
	if err := startShellRun(c, runID, map[string]any{"rows": 24, "cols": 80}); err != nil {
		return err
	}
	// Resize down to 12x40.
	if err := c.send(envelope{
		"kind": "skill.resize", "run_id": runID, "rows": 12, "cols": 40,
	}); err != nil {
		return err
	}
	time.Sleep(150 * time.Millisecond)
	if err := sendStdin(c, runID, []byte("stty size; exit\n")); err != nil {
		return err
	}
	_, stdout, err := c.collectSkillEvent(runID, 5*time.Second, "exit")
	if err != nil {
		return err
	}
	if !strings.Contains(string(stdout), "12 40") {
		return fmt.Errorf("expected '12 40' from stty size, got: %q", string(stdout))
	}
	return nil
}

func scenarioShellLargeInput(ctx context.Context, c *client) error {
	const runID = 115
	if err := startShellRun(c, runID, map[string]any{"rows": 24, "cols": 200}); err != nil {
		return err
	}
	// Make a 16 KiB blob of base64 chars + newline so it survives the
	// terminal raw-mode echo. Use `wc -c` to count bytes back.
	payload := strings.Repeat("ABCDEF0123", 1600) // 16,000 chars
	if err := sendStdin(c, runID, []byte("cat > /tmp/polar-test.bin <<'EOF'\n"+payload+"\nEOF\nwc -c /tmp/polar-test.bin; rm /tmp/polar-test.bin; exit\n")); err != nil {
		return err
	}
	_, stdout, err := c.collectSkillEvent(runID, 10*time.Second, "exit")
	if err != nil {
		return err
	}
	// Expected: 16001 bytes (payload + trailing newline).
	if !strings.Contains(string(stdout), "16001") {
		return fmt.Errorf("large stdin: byte count mismatch in stdout (want 16001), got: %q", lastLine(string(stdout)))
	}
	return nil
}

func scenarioShellMultiRun(ctx context.Context, c *client) error {
	// Start 3 concurrent shells in one connection, each writes a
	// unique marker. Assert each marker appears in its own run's
	// stdout, no cross-talk. Uses collectMultipleRuns for the single-
	// pass drain — sequential collectSkillEvent would discard the
	// other runs' events while waiting on one.
	type runSpec struct {
		id     int64
		marker string
	}
	runs := []runSpec{
		{120, "marker-alpha-9"},
		{121, "marker-beta-7"},
		{122, "marker-gamma-5"},
	}
	ids := make([]int64, 0, len(runs))
	for _, r := range runs {
		if err := startShellRun(c, r.id, map[string]any{"rows": 24, "cols": 80}); err != nil {
			return fmt.Errorf("start run %d: %w", r.id, err)
		}
		if err := sendStdin(c, r.id, []byte("echo "+r.marker+"; exit\n")); err != nil {
			return err
		}
		ids = append(ids, r.id)
	}
	collected, err := c.collectMultipleRuns(ids, 15*time.Second)
	if err != nil {
		return err
	}
	for _, r := range runs {
		rc := collected[r.id]
		if rc == nil || rc.Exit == nil {
			return fmt.Errorf("run %d: no exit event", r.id)
		}
		if !strings.Contains(string(rc.Stdout), r.marker) {
			return fmt.Errorf("run %d missing marker %q in stdout: %q", r.id, r.marker, string(rc.Stdout))
		}
		for _, other := range runs {
			if other.id == r.id {
				continue
			}
			if strings.Contains(string(rc.Stdout), other.marker) {
				return fmt.Errorf("run %d stdout contains marker from run %d (cross-talk!): %q", r.id, other.id, string(rc.Stdout))
			}
		}
	}
	return nil
}

func scenarioShellBadCWD(ctx context.Context, c *client) error {
	const runID = 123
	cfg, _ := json.Marshal(map[string]any{
		"cwd": "/no/such/dir/polar-test-nonexistent-x9",
	})
	if err := c.send(envelope{
		"kind":       "skill.start",
		"run_id":     runID,
		"skill_kind": "shell",
		"config":     json.RawMessage(cfg),
	}); err != nil {
		return err
	}
	exitEv, _, err := c.collectSkillEvent(runID, 3*time.Second, "exit")
	if err != nil {
		return err
	}
	data, _ := exitEv["data"].(map[string]any)
	if ok, _ := data["ok"].(bool); ok {
		return fmt.Errorf("expected start to fail for nonexistent cwd, got ok=true")
	}
	return nil
}

func scenarioShellBadShell(ctx context.Context, c *client) error {
	// shell.go's whitelist is bash/zsh/sh — anything else gets silently
	// ignored and falls back to the auto-detect. So fish should NOT
	// produce an error; the shell starts under bash (or zsh per $SHELL).
	const runID = 124
	if err := startShellRun(c, runID, map[string]any{
		"shell": "fish",
	}); err != nil {
		return err
	}
	if err := sendStdin(c, runID, []byte("echo $0; exit\n")); err != nil {
		return err
	}
	_, stdout, err := c.collectSkillEvent(runID, 5*time.Second, "exit")
	if err != nil {
		return err
	}
	out := string(stdout)
	// Should NOT be fish — agent falls back to a whitelisted shell.
	if strings.Contains(out, "fish") {
		return fmt.Errorf("unexpected fish in stdout (should have fallen back): %q", out)
	}
	return nil
}

func scenarioVNCBadTarget(ctx context.Context, c *client) error {
	const runID = 210
	cfg, _ := json.Marshal(map[string]any{"target": "not-a-host-port"})
	if err := c.send(envelope{
		"kind":       "skill.start",
		"run_id":     runID,
		"skill_kind": "vnc",
		"config":     json.RawMessage(cfg),
	}); err != nil {
		return err
	}
	exitEv, _, err := c.collectSkillEvent(runID, 3*time.Second, "exit")
	if err != nil {
		return err
	}
	data, _ := exitEv["data"].(map[string]any)
	if ok, _ := data["ok"].(bool); ok {
		return fmt.Errorf("expected vnc start to fail for malformed target")
	}
	errStr, _ := data["error"].(string)
	if !strings.Contains(errStr, "target") && !strings.Contains(errStr, "host:port") {
		return fmt.Errorf("expected error to mention 'target' or 'host:port', got: %q", errStr)
	}
	return nil
}

func scenarioVNCPublicRejected(ctx context.Context, c *client) error {
	const runID = 211
	cfg, _ := json.Marshal(map[string]any{"target": "8.8.8.8:5900"})
	if err := c.send(envelope{
		"kind":       "skill.start",
		"run_id":     runID,
		"skill_kind": "vnc",
		"config":     json.RawMessage(cfg),
	}); err != nil {
		return err
	}
	exitEv, _, err := c.collectSkillEvent(runID, 3*time.Second, "exit")
	if err != nil {
		return err
	}
	data, _ := exitEv["data"].(map[string]any)
	if ok, _ := data["ok"].(bool); ok {
		return fmt.Errorf("expected public IP target to be rejected")
	}
	errStr, _ := data["error"].(string)
	if !strings.Contains(errStr, "loopback") && !strings.Contains(errStr, "private") {
		return fmt.Errorf("expected rejection mentioning loopback/private, got: %q", errStr)
	}
	return nil
}

func scenarioVNCDialRefused(ctx context.Context, c *client) error {
	const runID = 212
	// Port 1 is virtually never listening. Loopback so validation passes.
	cfg, _ := json.Marshal(map[string]any{"target": "127.0.0.1:1"})
	if err := c.send(envelope{
		"kind":       "skill.start",
		"run_id":     runID,
		"skill_kind": "vnc",
		"config":     json.RawMessage(cfg),
	}); err != nil {
		return err
	}
	exitEv, _, err := c.collectSkillEvent(runID, 6*time.Second, "exit")
	if err != nil {
		return err
	}
	data, _ := exitEv["data"].(map[string]any)
	if ok, _ := data["ok"].(bool); ok {
		return fmt.Errorf("expected dial to refused port to fail")
	}
	errStr, _ := data["error"].(string)
	if !strings.Contains(errStr, "dial") && !strings.Contains(errStr, "refused") && !strings.Contains(errStr, "connect") {
		return fmt.Errorf("expected dial/refused/connect in error, got: %q", errStr)
	}
	return nil
}

// startMockVNC is a per-scenario helper. Returns the mock + a defer
// you can call to clean up. Each scenario gets its own mock listening
// on a kernel-assigned loopback port so they don't collide.
func startMockVNC() (*mockVNCServer, func(), error) {
	m, err := startMockVNCServer()
	if err != nil {
		return nil, func() {}, err
	}
	return m, m.Close, nil
}

func scenarioVNCMockBanner(ctx context.Context, c *client) error {
	mock, stop, err := startMockVNC()
	if err != nil {
		return fmt.Errorf("start mock: %w", err)
	}
	defer stop()
	const runID = 220
	cfg, _ := json.Marshal(map[string]any{"target": mock.Addr})
	if err := c.send(envelope{
		"kind":       "skill.start",
		"run_id":     runID,
		"skill_kind": "vnc",
		"config":     json.RawMessage(cfg),
	}); err != nil {
		return err
	}
	// Wait for stdout bytes that include the banner; stop cleanly.
	defer c.send(envelope{"kind": "skill.stop", "run_id": runID, "reason": "scenario_done"})
	deadline := time.Now().Add(3 * time.Second)
	var stdout []byte
	for time.Now().Before(deadline) {
		_, more, _ := c.collectSkillEvent(runID, 300*time.Millisecond, "exit", "state")
		stdout = append(stdout, more...)
		if strings.HasPrefix(string(stdout), "RFB ") && strings.Contains(string(stdout), "\n") {
			return nil // banner forwarded verbatim
		}
	}
	return fmt.Errorf("banner not forwarded within 3s; got %q (mock conns=%d, echoed=%d)",
		string(stdout), mock.conns.Load(), mock.bytesEcho.Load())
}

func scenarioVNCMockEcho(ctx context.Context, c *client) error {
	mock, stop, err := startMockVNC()
	if err != nil {
		return err
	}
	defer stop()
	const runID = 221
	cfg, _ := json.Marshal(map[string]any{"target": mock.Addr})
	if err := c.send(envelope{
		"kind":       "skill.start",
		"run_id":     runID,
		"skill_kind": "vnc",
		"config":     json.RawMessage(cfg),
	}); err != nil {
		return err
	}
	defer c.send(envelope{"kind": "skill.stop", "run_id": runID, "reason": "scenario_done"})
	// Drain the banner first so we know the conn is live.
	deadline := time.Now().Add(2 * time.Second)
	var pre []byte
	for time.Now().Before(deadline) && !strings.HasPrefix(string(pre), "RFB ") {
		_, more, _ := c.collectSkillEvent(runID, 200*time.Millisecond, "exit")
		pre = append(pre, more...)
	}
	if !strings.HasPrefix(string(pre), "RFB ") {
		return fmt.Errorf("no banner before echo; got %q", string(pre))
	}
	// Send a marker through skill.stdin; mock echoes back; agent forwards.
	marker := []byte("MARKER-ECHO-PAYLOAD-Z7K3")
	if err := sendStdin(c, runID, marker); err != nil {
		return err
	}
	// Wait for echoed bytes in stdout. Drain until found or timeout.
	deadline = time.Now().Add(3 * time.Second)
	var got []byte
	for time.Now().Before(deadline) {
		_, more, _ := c.collectSkillEvent(runID, 300*time.Millisecond, "exit")
		got = append(got, more...)
		if bytes.Contains(got, marker) {
			return nil
		}
	}
	return fmt.Errorf("echoed marker not seen in stdout: %q (mock echoed=%d bytes)",
		string(got), mock.bytesEcho.Load())
}

func scenarioVNCMockServerClose(ctx context.Context, c *client) error {
	// Mock closes the TCP conn after 4 bytes received from client.
	// Agent's reader sees EOF, vncRun.stop("eof") fires, we get
	// an exit event with reason=eof.
	mock, err := startMockVNCServer()
	if err != nil {
		return err
	}
	mock.WithCloseAfter(4)
	defer mock.Close()
	const runID = 222
	cfg, _ := json.Marshal(map[string]any{"target": mock.Addr})
	if err := c.send(envelope{
		"kind":       "skill.start",
		"run_id":     runID,
		"skill_kind": "vnc",
		"config":     json.RawMessage(cfg),
	}); err != nil {
		return err
	}
	// Drain banner, then send exactly 4 bytes to trigger server close.
	deadline := time.Now().Add(2 * time.Second)
	var pre []byte
	for time.Now().Before(deadline) && !strings.HasPrefix(string(pre), "RFB ") {
		_, more, _ := c.collectSkillEvent(runID, 200*time.Millisecond, "exit")
		pre = append(pre, more...)
	}
	if err := sendStdin(c, runID, []byte("KILL")); err != nil {
		return err
	}
	exitEv, _, err := c.collectSkillEvent(runID, 5*time.Second, "exit")
	if err != nil {
		return err
	}
	data, _ := exitEv["data"].(map[string]any)
	reason, _ := data["reason"].(string)
	if reason != "eof" {
		return fmt.Errorf("expected exit reason=eof on server close, got: %q", reason)
	}
	return nil
}

func scenarioVNCMockMultiViewer(ctx context.Context, c *client) error {
	// Two concurrent vnc runs targeting the same mock — each should
	// get its own TCP conn → its own banner → its own stdout stream.
	// Verifies that agent doesn't dedup VNC dials behind our back.
	mock, stop, err := startMockVNC()
	if err != nil {
		return err
	}
	defer stop()
	cfg, _ := json.Marshal(map[string]any{"target": mock.Addr})
	startMsg := func(runID int64) envelope {
		return envelope{
			"kind":       "skill.start",
			"run_id":     runID,
			"skill_kind": "vnc",
			"config":     json.RawMessage(cfg),
		}
	}
	const r1, r2 = 230, 231
	if err := c.send(startMsg(r1)); err != nil {
		return err
	}
	if err := c.send(startMsg(r2)); err != nil {
		return err
	}
	defer c.send(envelope{"kind": "skill.stop", "run_id": r1, "reason": "scenario_done"})
	defer c.send(envelope{"kind": "skill.stop", "run_id": r2, "reason": "scenario_done"})

	// Each run should get the banner independently. collect each run's
	// stdout via a single drain pass.
	got := map[int64][]byte{r1: nil, r2: nil}
	deadline := time.After(4 * time.Second)
	done := func() bool {
		for _, b := range got {
			if !strings.HasPrefix(string(b), "RFB ") {
				return false
			}
		}
		return true
	}
	for !done() {
		select {
		case env, ok := <-c.inbox:
			if !ok {
				return fmt.Errorf("inbox closed; got=%v", got)
			}
			if k, _ := env["kind"].(string); k != "skill.event" {
				continue
			}
			runID := int64(env["run_id"].(float64))
			if _, tracked := got[runID]; !tracked {
				continue
			}
			if evKind, _ := env["event_kind"].(string); evKind == "stdout" {
				if data, _ := env["data"].(map[string]any); data != nil {
					if b64, ok := data["bytes_b64"].(string); ok {
						chunk, _ := base64.StdEncoding.DecodeString(b64)
						got[runID] = append(got[runID], chunk...)
					}
				}
			}
		case <-deadline:
			return fmt.Errorf("timeout — run1=%q run2=%q mockConns=%d",
				string(got[r1]), string(got[r2]), mock.conns.Load())
		}
	}
	if mock.conns.Load() < 2 {
		return fmt.Errorf("mock saw only %d conns, expected ≥2 (multi-viewer dedup leak?)", mock.conns.Load())
	}
	return nil
}

// startMCPRun is the MCP-equivalent of startShellRun: dispatches
// skill.start kind=mcp-server, waits briefly for the run to register.
func startMCPRun(c *client, runID int64) error {
	cfg, _ := json.Marshal(map[string]any{})
	if err := c.send(envelope{
		"kind":       "skill.start",
		"run_id":     runID,
		"skill_kind": "mcp-server",
		"config":     json.RawMessage(cfg),
	}); err != nil {
		return err
	}
	time.Sleep(150 * time.Millisecond)
	return nil
}

// mcpRequest is one JSON-RPC request line going IN; mcpResponse is one
// frame coming back. The MCP server emits NDJSON, so each EventStdout
// chunk may contain ≥1 complete reply.
type mcpResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// sendMCPRequest sends one JSON-RPC line on skill.stdin and waits for
// the matching id back in stdout. Other MCP frames in flight are
// buffered for later (per-run inbox in scenarios is fine since one
// scenario serializes its calls).
func sendMCPRequest(c *client, runID int64, id int, method string, params any, timeout time.Duration) (*mcpResponse, error) {
	rpc := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
	}
	if params != nil {
		rpc["params"] = params
	}
	wire, _ := json.Marshal(rpc)
	wire = append(wire, '\n')
	if err := sendStdin(c, runID, wire); err != nil {
		return nil, fmt.Errorf("send: %w", err)
	}
	deadline := time.Now().Add(timeout)
	var pending []byte
	for time.Now().Before(deadline) {
		_, more, _ := c.collectSkillEvent(runID, 200*time.Millisecond, "exit")
		pending = append(pending, more...)
		// Split by newline; try to parse each complete line.
		for {
			idx := bytes.IndexByte(pending, '\n')
			if idx < 0 {
				break
			}
			line := pending[:idx]
			pending = pending[idx+1:]
			if len(bytes.TrimSpace(line)) == 0 {
				continue
			}
			var resp mcpResponse
			if err := json.Unmarshal(line, &resp); err != nil {
				continue
			}
			// Match id (numeric only for these scenarios).
			var rid int
			if err := json.Unmarshal(resp.ID, &rid); err == nil && rid == id {
				return &resp, nil
			}
		}
	}
	return nil, fmt.Errorf("timeout waiting for response id=%d (method=%s)", id, method)
}

func scenarioMCPInitialize(ctx context.Context, c *client) error {
	const runID = 300
	if err := startMCPRun(c, runID); err != nil {
		return err
	}
	resp, err := sendMCPRequest(c, runID, 1, "initialize", map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "polar-agent-test", "version": "1.0"},
	}, 3*time.Second)
	if err != nil {
		return err
	}
	if resp.Error != nil {
		return fmt.Errorf("initialize returned error: %d %s", resp.Error.Code, resp.Error.Message)
	}
	var result struct {
		ProtocolVersion string         `json:"protocolVersion"`
		Capabilities    map[string]any `json:"capabilities"`
		ServerInfo      map[string]any `json:"serverInfo"`
	}
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		return fmt.Errorf("parse result: %w", err)
	}
	if result.ProtocolVersion == "" {
		return fmt.Errorf("missing protocolVersion")
	}
	if _, ok := result.Capabilities["tools"]; !ok {
		return fmt.Errorf("missing capabilities.tools")
	}
	if name, _ := result.ServerInfo["name"].(string); name == "" {
		return fmt.Errorf("missing serverInfo.name")
	}
	return nil
}

func scenarioMCPToolsList(ctx context.Context, c *client) error {
	const runID = 301
	if err := startMCPRun(c, runID); err != nil {
		return err
	}
	resp, err := sendMCPRequest(c, runID, 1, "tools/list", nil, 3*time.Second)
	if err != nil {
		return err
	}
	if resp.Error != nil {
		return fmt.Errorf("tools/list error: %d %s", resp.Error.Code, resp.Error.Message)
	}
	var result struct {
		Tools []struct {
			Name        string         `json:"name"`
			Description string         `json:"description"`
			InputSchema map[string]any `json:"inputSchema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		return fmt.Errorf("parse: %w", err)
	}
	found := false
	for _, t := range result.Tools {
		if t.Name == "echo" {
			found = true
			if t.InputSchema == nil {
				return fmt.Errorf("echo tool missing inputSchema")
			}
		}
	}
	if !found {
		return fmt.Errorf("tools/list didn't include 'echo' tool")
	}
	return nil
}

func scenarioMCPToolsCallEcho(ctx context.Context, c *client) error {
	const runID = 302
	if err := startMCPRun(c, runID); err != nil {
		return err
	}
	resp, err := sendMCPRequest(c, runID, 1, "tools/call", map[string]any{
		"name":      "echo",
		"arguments": map[string]any{"message": "hello-mcp-Z7K3"},
	}, 3*time.Second)
	if err != nil {
		return err
	}
	if resp.Error != nil {
		return fmt.Errorf("tools/call error: %d %s", resp.Error.Code, resp.Error.Message)
	}
	var result struct {
		Content []map[string]any `json:"content"`
	}
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		return fmt.Errorf("parse: %w", err)
	}
	if len(result.Content) == 0 {
		return fmt.Errorf("empty result.content")
	}
	text, _ := result.Content[0]["text"].(string)
	if text != "hello-mcp-Z7K3" {
		return fmt.Errorf("echo mismatch: got %q, want 'hello-mcp-Z7K3'", text)
	}
	return nil
}

func scenarioMCPBadMethod(ctx context.Context, c *client) error {
	const runID = 303
	if err := startMCPRun(c, runID); err != nil {
		return err
	}
	resp, err := sendMCPRequest(c, runID, 1, "this/does/not/exist", nil, 3*time.Second)
	if err != nil {
		return err
	}
	if resp.Error == nil {
		return fmt.Errorf("expected JSON-RPC error, got result: %s", string(resp.Result))
	}
	if resp.Error.Code != -32601 {
		return fmt.Errorf("expected error code -32601 (Method not found), got %d (%s)", resp.Error.Code, resp.Error.Message)
	}
	return nil
}

func scenarioMCPParseError(ctx context.Context, c *client) error {
	const runID = 304
	if err := startMCPRun(c, runID); err != nil {
		return err
	}
	// Send a malformed JSON line — agent should reply with code -32700.
	if err := sendStdin(c, runID, []byte("{this-is-not-valid-json\n")); err != nil {
		return err
	}
	deadline := time.Now().Add(3 * time.Second)
	var pending []byte
	for time.Now().Before(deadline) {
		_, more, _ := c.collectSkillEvent(runID, 200*time.Millisecond, "exit")
		pending = append(pending, more...)
		for {
			idx := bytes.IndexByte(pending, '\n')
			if idx < 0 {
				break
			}
			line := pending[:idx]
			pending = pending[idx+1:]
			if len(bytes.TrimSpace(line)) == 0 {
				continue
			}
			var resp mcpResponse
			if err := json.Unmarshal(line, &resp); err != nil {
				continue
			}
			if resp.Error != nil && resp.Error.Code == -32700 {
				return nil
			}
		}
	}
	return fmt.Errorf("expected parse error (-32700) reply within 3s")
}

// startMCPRunWithAdapters dispatches skill.start kind=mcp-server with
// an explicit adapter list. Use empty slice to test the "no config →
// defaults to ['echo']" path.
func startMCPRunWithAdapters(c *client, runID int64, adapters []string) error {
	cfgMap := map[string]any{}
	if len(adapters) > 0 {
		cfgMap["adapters"] = adapters
	}
	cfg, _ := json.Marshal(cfgMap)
	if err := c.send(envelope{
		"kind":       "skill.start",
		"run_id":     runID,
		"skill_kind": "mcp-server",
		"config":     json.RawMessage(cfg),
	}); err != nil {
		return err
	}
	time.Sleep(150 * time.Millisecond)
	return nil
}

func toolNamesFromList(resp *mcpResponse) ([]string, error) {
	if resp.Error != nil {
		return nil, fmt.Errorf("tools/list error: %d %s", resp.Error.Code, resp.Error.Message)
	}
	var result struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	names := make([]string, len(result.Tools))
	for i, t := range result.Tools {
		names[i] = t.Name
	}
	return names, nil
}

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

func scenarioMCPAdapterDefault(ctx context.Context, c *client) error {
	const runID = 310
	// No "adapters" key → default ["echo"].
	if err := startMCPRunWithAdapters(c, runID, nil); err != nil {
		return err
	}
	resp, err := sendMCPRequest(c, runID, 1, "tools/list", nil, 3*time.Second)
	if err != nil {
		return err
	}
	tools, err := toolNamesFromList(resp)
	if err != nil {
		return err
	}
	if !containsString(tools, "echo") {
		return fmt.Errorf("default config missing echo tool; got %v", tools)
	}
	if containsString(tools, "stub.add") {
		return fmt.Errorf("default config unexpectedly includes stub tools; got %v", tools)
	}
	return nil
}

func scenarioMCPAdapterStubOnly(ctx context.Context, c *client) error {
	const runID = 311
	if err := startMCPRunWithAdapters(c, runID, []string{"stub"}); err != nil {
		return err
	}
	resp, err := sendMCPRequest(c, runID, 1, "tools/list", nil, 3*time.Second)
	if err != nil {
		return err
	}
	tools, err := toolNamesFromList(resp)
	if err != nil {
		return err
	}
	for _, want := range []string{"stub.add", "stub.error", "stub.delay_ms"} {
		if !containsString(tools, want) {
			return fmt.Errorf("stub-only config missing %s; got %v", want, tools)
		}
	}
	if containsString(tools, "echo") {
		return fmt.Errorf("stub-only config unexpectedly includes echo; got %v", tools)
	}
	return nil
}

func scenarioMCPAdapterMulti(ctx context.Context, c *client) error {
	const runID = 312
	if err := startMCPRunWithAdapters(c, runID, []string{"echo", "stub"}); err != nil {
		return err
	}
	resp, err := sendMCPRequest(c, runID, 1, "tools/list", nil, 3*time.Second)
	if err != nil {
		return err
	}
	tools, err := toolNamesFromList(resp)
	if err != nil {
		return err
	}
	for _, want := range []string{"echo", "stub.add", "stub.error", "stub.delay_ms"} {
		if !containsString(tools, want) {
			return fmt.Errorf("multi config missing %s; got %v", want, tools)
		}
	}
	return nil
}

func scenarioMCPStubAdd(ctx context.Context, c *client) error {
	const runID = 313
	if err := startMCPRunWithAdapters(c, runID, []string{"stub"}); err != nil {
		return err
	}
	resp, err := sendMCPRequest(c, runID, 1, "tools/call", map[string]any{
		"name":      "stub.add",
		"arguments": map[string]any{"a": 3, "b": 4},
	}, 3*time.Second)
	if err != nil {
		return err
	}
	if resp.Error != nil {
		return fmt.Errorf("stub.add error: %d %s", resp.Error.Code, resp.Error.Message)
	}
	var result struct {
		Sum     float64          `json:"sum"`
		Content []map[string]any `json:"content"`
	}
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		return err
	}
	if result.Sum != 7 {
		return fmt.Errorf("stub.add sum mismatch: got %v, want 7", result.Sum)
	}
	if len(result.Content) == 0 {
		return fmt.Errorf("missing content array")
	}
	if text, _ := result.Content[0]["text"].(string); text != "7" {
		return fmt.Errorf("stub.add content text mismatch: got %q, want '7'", text)
	}
	return nil
}

func scenarioMCPStubError(ctx context.Context, c *client) error {
	const runID = 314
	if err := startMCPRunWithAdapters(c, runID, []string{"stub"}); err != nil {
		return err
	}
	resp, err := sendMCPRequest(c, runID, 1, "tools/call", map[string]any{
		"name":      "stub.error",
		"arguments": map[string]any{},
	}, 3*time.Second)
	if err != nil {
		return err
	}
	if resp.Error == nil {
		return fmt.Errorf("expected JSON-RPC error from stub.error, got result: %s", string(resp.Result))
	}
	if resp.Error.Code != -32000 {
		return fmt.Errorf("expected -32000, got %d (%s)", resp.Error.Code, resp.Error.Message)
	}
	if !strings.Contains(resp.Error.Message, "intentional failure") {
		return fmt.Errorf("expected 'intentional failure' in message, got: %q", resp.Error.Message)
	}
	return nil
}

func scenarioMCPAdapterUnknown(ctx context.Context, c *client) error {
	const runID = 315
	// Adapters list with a name that nobody registered. Skill should
	// still start (soft-fail on the missing one). The catalogue
	// contains ONLY the framework meta tools (mcp.list_sessions /
	// mcp.close_session) since no adapter loaded — no adapter-owned
	// tools surface.
	if err := startMCPRunWithAdapters(c, runID, []string{"nonesuch-adapter-zzz"}); err != nil {
		return err
	}
	resp, err := sendMCPRequest(c, runID, 1, "tools/list", nil, 3*time.Second)
	if err != nil {
		return err
	}
	tools, err := toolNamesFromList(resp)
	if err != nil {
		return err
	}
	for _, t := range tools {
		// Framework tools (mcp.*) are allowed; any other name means an
		// adapter loaded that shouldn't have.
		if !strings.HasPrefix(t, "mcp.") {
			return fmt.Errorf("expected only mcp.* framework tools when all adapters skipped, got %v", tools)
		}
	}
	return nil
}

func scenarioMCPSessionCreate(ctx context.Context, c *client) error {
	const runID = 320
	if err := startMCPRunWithAdapters(c, runID, []string{"stub"}); err != nil {
		return err
	}
	// Open a session via stub.open_session.
	resp, err := sendMCPRequest(c, runID, 1, "tools/call", map[string]any{
		"name":      "stub.open_session",
		"arguments": map[string]any{},
	}, 3*time.Second)
	if err != nil {
		return err
	}
	if resp.Error != nil {
		return fmt.Errorf("open_session error: %d %s", resp.Error.Code, resp.Error.Message)
	}
	var result struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		return err
	}
	if !strings.HasPrefix(result.SessionID, "s_") {
		return fmt.Errorf("expected session_id prefix s_, got: %q", result.SessionID)
	}
	// Confirm it appears in mcp.list_sessions.
	resp, err = sendMCPRequest(c, runID, 2, "tools/call", map[string]any{
		"name":      "mcp.list_sessions",
		"arguments": map[string]any{},
	}, 3*time.Second)
	if err != nil {
		return err
	}
	if resp.Error != nil {
		return fmt.Errorf("list_sessions error: %s", resp.Error.Message)
	}
	var listResult struct {
		Sessions []map[string]any `json:"sessions"`
	}
	if err := json.Unmarshal(resp.Result, &listResult); err != nil {
		return err
	}
	found := false
	for _, s := range listResult.Sessions {
		if s["id"] == result.SessionID && s["adapter"] == "stub" {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("session %s not in list_sessions: %v", result.SessionID, listResult.Sessions)
	}
	return nil
}

func scenarioMCPSessionClose(ctx context.Context, c *client) error {
	const runID = 321
	if err := startMCPRunWithAdapters(c, runID, []string{"stub"}); err != nil {
		return err
	}
	resp, err := sendMCPRequest(c, runID, 1, "tools/call", map[string]any{
		"name":      "stub.open_session",
		"arguments": map[string]any{},
	}, 3*time.Second)
	if err != nil {
		return err
	}
	var r1 struct {
		SessionID string `json:"session_id"`
	}
	_ = json.Unmarshal(resp.Result, &r1)
	if r1.SessionID == "" {
		return fmt.Errorf("no session_id from open: %s", string(resp.Result))
	}
	// Close it via the framework tool.
	resp, err = sendMCPRequest(c, runID, 2, "tools/call", map[string]any{
		"name":      "mcp.close_session",
		"arguments": map[string]any{"session_id": r1.SessionID},
	}, 3*time.Second)
	if err != nil {
		return err
	}
	if resp.Error != nil {
		return fmt.Errorf("close_session error: %s", resp.Error.Message)
	}
	// List should be empty.
	resp, err = sendMCPRequest(c, runID, 3, "tools/call", map[string]any{
		"name":      "mcp.list_sessions",
		"arguments": map[string]any{},
	}, 3*time.Second)
	if err != nil {
		return err
	}
	var listResult struct {
		Sessions []map[string]any `json:"sessions"`
	}
	_ = json.Unmarshal(resp.Result, &listResult)
	for _, s := range listResult.Sessions {
		if s["id"] == r1.SessionID {
			return fmt.Errorf("session %s still present after close: %v", r1.SessionID, s)
		}
	}
	return nil
}

func scenarioMCPSessionBadID(ctx context.Context, c *client) error {
	const runID = 322
	if err := startMCPRunWithAdapters(c, runID, []string{"stub"}); err != nil {
		return err
	}
	resp, err := sendMCPRequest(c, runID, 1, "tools/call", map[string]any{
		"name":      "mcp.close_session",
		"arguments": map[string]any{"session_id": "s_does_not_exist_zzz"},
	}, 3*time.Second)
	if err != nil {
		return err
	}
	if resp.Error == nil {
		return fmt.Errorf("expected JSON-RPC error for unknown session_id")
	}
	if !strings.Contains(resp.Error.Message, "not found") {
		return fmt.Errorf("expected 'not found' in error message, got: %q", resp.Error.Message)
	}
	return nil
}

func scenarioMCPSessionIdleEvict(ctx context.Context, c *client) error {
	// This scenario is slow (~35s) because the idleWatchdog ticks every
	// 30 seconds inside the agent and we can't control that without
	// adding more knobs. POLAR_MCP_SESSION_IDLE_SEC=1 gates the
	// per-session cutoff; the tick rate stays 30s. Skip when running
	// quick smoke tests.
	if os.Getenv("POLAR_SKIP_SLOW_SCENARIOS") == "1" {
		return fmt.Errorf("SKIPPED — set POLAR_SKIP_SLOW_SCENARIOS=0 to run (35s+)")
	}
	const runID = 323
	if err := startMCPRunWithAdapters(c, runID, []string{"stub"}); err != nil {
		return err
	}
	resp, err := sendMCPRequest(c, runID, 1, "tools/call", map[string]any{
		"name":      "stub.open_session",
		"arguments": map[string]any{},
	}, 3*time.Second)
	if err != nil {
		return err
	}
	var r1 struct {
		SessionID string `json:"session_id"`
	}
	_ = json.Unmarshal(resp.Result, &r1)
	// Wait for ≥1 watchdog tick AFTER the per-session idle expires.
	// agent test-serve was launched with POLAR_MCP_SESSION_IDLE_SEC=1
	// (set by orchestrate via env passthrough); tick = 30s; pad to 35s.
	time.Sleep(35 * time.Second)
	resp, err = sendMCPRequest(c, runID, 2, "tools/call", map[string]any{
		"name":      "mcp.list_sessions",
		"arguments": map[string]any{},
	}, 3*time.Second)
	if err != nil {
		return err
	}
	var listResult struct {
		Sessions []map[string]any `json:"sessions"`
	}
	_ = json.Unmarshal(resp.Result, &listResult)
	for _, s := range listResult.Sessions {
		if s["id"] == r1.SessionID {
			return fmt.Errorf("session %s NOT evicted after 35s with idle=1s", r1.SessionID)
		}
	}
	return nil
}

func lastLine(s string) string {
	s = strings.TrimRight(s, "\r\n")
	if i := strings.LastIndexAny(s, "\r\n"); i >= 0 {
		return s[i+1:]
	}
	return s
}

func scenarioUnknownSkill(ctx context.Context, c *client) error {
	const runID = 999
	if err := c.send(envelope{
		"kind":       "skill.start",
		"run_id":     runID,
		"skill_kind": "this-skill-does-not-exist",
		"config":     json.RawMessage("{}"),
	}); err != nil {
		return err
	}
	exitEv, _, err := c.collectSkillEvent(runID, 3*time.Second, "exit")
	if err != nil {
		return err
	}
	data, _ := exitEv["data"].(map[string]any)
	errStr, _ := data["error"].(string)
	if !strings.Contains(errStr, "not registered") {
		return fmt.Errorf("expected 'not registered' in error, got: %q", errStr)
	}
	return nil
}
