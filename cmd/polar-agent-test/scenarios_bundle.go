package main

// Bundle-skill scenarios. Each opens with a fresh runID, drives the
// agent through one envelope sequence, and asserts on the resulting
// bundleRunResult. Fixtures are isolated by unique <name@version>
// strings so re-runs don't collide with state from earlier scenarios.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// startBundle is shared shorthand for sending skill.start with kind=bundle.
func startBundle(c *client, runID int64, cfg map[string]any) error {
	raw, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	return c.send(envelope{
		"kind":       "skill.start",
		"run_id":     runID,
		"skill_kind": "bundle",
		"config":     json.RawMessage(raw),
	})
}

// firstExitError extracts a human-readable error string from an
// EventExit envelope's data field. Empty string if missing — the
// agent emits both "reason" and "error" depending on where the
// failure originated.
func firstExitError(env envelope) string {
	data, _ := env["data"].(map[string]any)
	if s, _ := data["error"].(string); s != "" {
		return s
	}
	if s, _ := data["reason"].(string); s != "" {
		return s
	}
	return ""
}

func scenarioBundleList(ctx context.Context, c *client) error {
	if err := c.send(envelope{"kind": "list"}); err != nil {
		return err
	}
	env, err := c.waitFor(3*time.Second, "skills")
	if err != nil {
		return err
	}
	skills, _ := env["skills"].([]any)
	for _, raw := range skills {
		sk, _ := raw.(map[string]any)
		if kind, _ := sk["kind"].(string); kind == "bundle" {
			caps, _ := sk["capabilities"].(map[string]any)
			if _, ok := caps["installed"]; !ok {
				return fmt.Errorf("bundle capabilities missing installed field: %+v", caps)
			}
			return nil
		}
	}
	return fmt.Errorf("kind=bundle not present in skills list: %+v", skills)
}

func scenarioBundleExecShell(ctx context.Context, c *client) error {
	const (
		name    = "scn-exec-shell"
		version = "0.0.1"
		runID   = 600
	)
	defer removeStagedBundle(name, version)

	if err := stageBundleOnDisk(name, version, map[string]string{
		"SKILL.md":       "---\nname: " + name + "\n---\n",
		"scripts/run.sh": "#!/bin/sh\necho marker-stdout-1\necho marker-stdout-2\n",
	}); err != nil {
		return err
	}
	if err := startBundle(c, runID, map[string]any{
		"bundle":     name,
		"version":    version,
		"entrypoint": "scripts/run.sh",
	}); err != nil {
		return err
	}
	res, err := collectBundleEvent(c, runID, 5*time.Second, "exit")
	if err != nil {
		return fmt.Errorf("collect: %w", err)
	}
	joined := strings.Join(res.StdoutLines, "\n")
	if !strings.Contains(joined, "marker-stdout-1") || !strings.Contains(joined, "marker-stdout-2") {
		return fmt.Errorf("missing stdout markers, got: %q (stderr=%q)", joined, res.StderrLines)
	}
	data, _ := res.Exit["data"].(map[string]any)
	if ok, _ := data["ok"].(bool); !ok {
		return fmt.Errorf("exit not ok: %+v", data)
	}
	return nil
}

func scenarioBundleExecStderrStream(ctx context.Context, c *client) error {
	const (
		name    = "scn-exec-stderr"
		version = "0.0.1"
		runID   = 601
	)
	defer removeStagedBundle(name, version)

	if err := stageBundleOnDisk(name, version, map[string]string{
		"SKILL.md":       "---\nname: " + name + "\n---\n",
		"scripts/run.sh": "#!/bin/sh\necho on-stdout\necho on-stderr >&2\n",
	}); err != nil {
		return err
	}
	if err := startBundle(c, runID, map[string]any{
		"bundle":     name,
		"version":    version,
		"entrypoint": "scripts/run.sh",
	}); err != nil {
		return err
	}
	res, err := collectBundleEvent(c, runID, 5*time.Second, "exit")
	if err != nil {
		return err
	}
	if !strings.Contains(strings.Join(res.StdoutLines, "\n"), "on-stdout") {
		return fmt.Errorf("stdout missing 'on-stdout': %v", res.StdoutLines)
	}
	if !strings.Contains(strings.Join(res.StderrLines, "\n"), "on-stderr") {
		return fmt.Errorf("stderr missing 'on-stderr': %v (stdout=%v)", res.StderrLines, res.StdoutLines)
	}
	return nil
}

func scenarioBundleExecNonzeroExit(ctx context.Context, c *client) error {
	const (
		name    = "scn-exec-nonzero"
		version = "0.0.1"
		runID   = 602
	)
	defer removeStagedBundle(name, version)

	if err := stageBundleOnDisk(name, version, map[string]string{
		"SKILL.md":       "---\nname: " + name + "\n---\n",
		"scripts/run.sh": "#!/bin/sh\nexit 7\n",
	}); err != nil {
		return err
	}
	if err := startBundle(c, runID, map[string]any{
		"bundle":     name,
		"version":    version,
		"entrypoint": "scripts/run.sh",
	}); err != nil {
		return err
	}
	res, err := collectBundleEvent(c, runID, 5*time.Second, "exit")
	if err != nil {
		return err
	}
	data, _ := res.Exit["data"].(map[string]any)
	okFlag, _ := data["ok"].(bool)
	exitCodeF, _ := data["exit_code"].(float64)
	if okFlag {
		return fmt.Errorf("expected ok=false for non-zero exit, got %+v", data)
	}
	if int(exitCodeF) != 7 {
		return fmt.Errorf("expected exit_code=7, got %v", data["exit_code"])
	}
	return nil
}

func scenarioBundleBadName(ctx context.Context, c *client) error {
	const runID = 610
	if err := startBundle(c, runID, map[string]any{
		"bundle":     "../evil",
		"version":    "0.0.1",
		"entrypoint": "scripts/run.sh",
	}); err != nil {
		return err
	}
	res, err := collectBundleEvent(c, runID, 3*time.Second, "exit")
	if err != nil {
		return err
	}
	errMsg := firstExitError(res.Exit)
	if !strings.Contains(errMsg, "bundle name") && !strings.Contains(errMsg, "invalid") {
		return fmt.Errorf("expected validation error about bundle name, got %q", errMsg)
	}
	return nil
}

func scenarioBundleBadEntrypoint(ctx context.Context, c *client) error {
	const runID = 611
	if err := startBundle(c, runID, map[string]any{
		"bundle":     "anyname",
		"version":    "0.0.1",
		"entrypoint": "references/notes.md",
	}); err != nil {
		return err
	}
	res, err := collectBundleEvent(c, runID, 3*time.Second, "exit")
	if err != nil {
		return err
	}
	errMsg := firstExitError(res.Exit)
	if !strings.Contains(errMsg, "entrypoint") {
		return fmt.Errorf("expected validation error about entrypoint, got %q", errMsg)
	}
	return nil
}

func scenarioBundleBadEnv(ctx context.Context, c *client) error {
	const runID = 612
	if err := startBundle(c, runID, map[string]any{
		"bundle":     "anyname",
		"version":    "0.0.1",
		"entrypoint": "scripts/run.sh",
		"env":        map[string]string{"LD_PRELOAD": "evil.so"},
	}); err != nil {
		return err
	}
	res, err := collectBundleEvent(c, runID, 3*time.Second, "exit")
	if err != nil {
		return err
	}
	errMsg := firstExitError(res.Exit)
	if !strings.Contains(errMsg, "LD_PRELOAD") && !strings.Contains(strings.ToLower(errMsg), "env") {
		return fmt.Errorf("expected validation error about env, got %q", errMsg)
	}
	return nil
}

func scenarioBundleMissingNoDownload(ctx context.Context, c *client) error {
	const runID = 613
	if err := startBundle(c, runID, map[string]any{
		"bundle":     "never-installed",
		"version":    "9.9.9",
		"entrypoint": "scripts/run.sh",
	}); err != nil {
		return err
	}
	res, err := collectBundleEvent(c, runID, 3*time.Second, "exit")
	if err != nil {
		return err
	}
	errMsg := firstExitError(res.Exit)
	if !strings.Contains(errMsg, "download_url") && !strings.Contains(errMsg, "not installed") {
		return fmt.Errorf("expected install/download error, got %q", errMsg)
	}
	return nil
}

func scenarioBundleHTTPInstall(ctx context.Context, c *client) error {
	const (
		name    = "scn-http-install"
		version = "0.0.1"
		runID   = 620
	)
	defer removeStagedBundle(name, version)

	zipBytes, sha, err := buildBundleZip(name, map[string]string{
		"SKILL.md":       "---\nname: " + name + "\n---\n",
		"scripts/run.sh": "#!/bin/sh\necho installed-then-ran\n",
	})
	if err != nil {
		return err
	}
	srv, url, err := startBundleHTTPServer(zipBytes)
	if err != nil {
		return err
	}
	defer stopBundleHTTPServer(srv)

	if err := startBundle(c, runID, map[string]any{
		"bundle":       name,
		"version":      version,
		"download_url": url,
		"sha256":       sha,
		"entrypoint":   "scripts/run.sh",
	}); err != nil {
		return err
	}
	res, err := collectBundleEvent(c, runID, 8*time.Second, "exit")
	if err != nil {
		return err
	}
	data, _ := res.Exit["data"].(map[string]any)
	if ok, _ := data["ok"].(bool); !ok {
		return fmt.Errorf("exit not ok: %+v (stdout=%v stderr=%v)", data, res.StdoutLines, res.StderrLines)
	}
	if !strings.Contains(strings.Join(res.StdoutLines, "\n"), "installed-then-ran") {
		return fmt.Errorf("missing post-install marker: %v", res.StdoutLines)
	}
	return nil
}

func scenarioBundleHTTPSHA256Mismatch(ctx context.Context, c *client) error {
	const (
		name    = "scn-http-sha-mismatch"
		version = "0.0.1"
		runID   = 621
	)
	defer removeStagedBundle(name, version)

	zipBytes, _, err := buildBundleZip(name, map[string]string{
		"SKILL.md":       "---\nname: " + name + "\n---\n",
		"scripts/run.sh": "#!/bin/sh\necho should-never-run\n",
	})
	if err != nil {
		return err
	}
	srv, url, err := startBundleHTTPServer(zipBytes)
	if err != nil {
		return err
	}
	defer stopBundleHTTPServer(srv)

	if err := startBundle(c, runID, map[string]any{
		"bundle":       name,
		"version":      version,
		"download_url": url,
		// Wrong sha256 — agent must reject and not unpack.
		"sha256":     "0000000000000000000000000000000000000000000000000000000000000000",
		"entrypoint": "scripts/run.sh",
	}); err != nil {
		return err
	}
	res, err := collectBundleEvent(c, runID, 5*time.Second, "exit")
	if err != nil {
		return err
	}
	errMsg := firstExitError(res.Exit)
	if !strings.Contains(strings.ToLower(errMsg), "sha256") {
		return fmt.Errorf("expected sha256 error, got %q", errMsg)
	}
	if len(res.StdoutLines) > 0 {
		return fmt.Errorf("script should not have run; got stdout: %v", res.StdoutLines)
	}
	return nil
}

func scenarioBundleStop(ctx context.Context, c *client) error {
	const (
		name    = "scn-stop"
		version = "0.0.1"
		runID   = 622
	)
	defer removeStagedBundle(name, version)

	if err := stageBundleOnDisk(name, version, map[string]string{
		"SKILL.md": "---\nname: " + name + "\n---\n",
		// 60s sleep — way longer than the test's 2s deadline. skill.stop
		// must shoot it in the head before then.
		"scripts/run.sh": "#!/bin/sh\necho starting\nsleep 60\n",
	}); err != nil {
		return err
	}
	if err := startBundle(c, runID, map[string]any{
		"bundle":     name,
		"version":    version,
		"entrypoint": "scripts/run.sh",
	}); err != nil {
		return err
	}
	// Let the script actually start so the process group is alive.
	time.Sleep(300 * time.Millisecond)
	if err := c.send(envelope{
		"kind":   "skill.stop",
		"run_id": runID,
		"reason": "scenario_stop",
	}); err != nil {
		return err
	}
	res, err := collectBundleEvent(c, runID, 8*time.Second, "exit")
	if err != nil {
		return fmt.Errorf("no exit after stop: %w", err)
	}
	if res.Exit == nil {
		return fmt.Errorf("no exit event")
	}
	// We don't assert on exit_code — SIGTERM'd sh on macOS may exit
	// 0 or 143 depending on signal handling. Existence of the exit
	// event within the budget is the assertion.
	return nil
}

func scenarioBundleCachedSkipsDownload(ctx context.Context, c *client) error {
	const (
		name    = "scn-cache-hit"
		version = "0.0.1"
		runID   = 623
	)
	defer removeStagedBundle(name, version)

	if err := stageBundleOnDisk(name, version, map[string]string{
		"SKILL.md":       "---\nname: " + name + "\n---\n",
		"scripts/run.sh": "#!/bin/sh\necho cache-hit-ok\n",
	}); err != nil {
		return err
	}

	// A download_url pointing at a port that's not listening — if the
	// agent tried to download, this would fail with a connection
	// refused and the run would error. Since the bundle is cached,
	// the agent should skip the download entirely.
	if err := startBundle(c, runID, map[string]any{
		"bundle":       name,
		"version":      version,
		"download_url": "http://127.0.0.1:1/bundle.skill",
		"sha256":       "0000000000000000000000000000000000000000000000000000000000000000",
		"entrypoint":   "scripts/run.sh",
	}); err != nil {
		return err
	}
	res, err := collectBundleEvent(c, runID, 5*time.Second, "exit")
	if err != nil {
		return err
	}
	data, _ := res.Exit["data"].(map[string]any)
	if ok, _ := data["ok"].(bool); !ok {
		return fmt.Errorf("expected cached path to skip download, got exit %+v", data)
	}
	if !strings.Contains(strings.Join(res.StdoutLines, "\n"), "cache-hit-ok") {
		return fmt.Errorf("missing marker: %v", res.StdoutLines)
	}
	return nil
}
