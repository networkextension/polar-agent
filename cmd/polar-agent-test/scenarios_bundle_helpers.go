package main

// Helpers used by the bundle-* scenarios. Bundle events are EventLog
// (stream + line) rather than EventStdout (bytes_b64), so the generic
// collectSkillEvent doesn't quite work — we need a per-stream
// accumulator. We also need to stage bundle fixtures on disk (in the
// agent's POLAR_AGENT_BUNDLES_DIR) and stand up an httptest.Server
// to feed the agent's runtime install path without dock.

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// bundleRunResult is the per-run accumulator a bundle scenario reads
// after the run exits.
type bundleRunResult struct {
	StdoutLines []string
	StderrLines []string
	State       envelope // last EventState payload, if any
	Exit        envelope // EventExit envelope (the full skill.event)
}

// collectBundleEvent drains skill.event envelopes for runID until one
// of wantedEvKinds shows up (typically "exit"). Along the way it
// accumulates EventLog lines by stream into the returned struct.
func collectBundleEvent(c *client, runID int64, timeout time.Duration, wantedEvKinds ...string) (*bundleRunResult, error) {
	want := map[string]bool{}
	for _, k := range wantedEvKinds {
		want[k] = true
	}
	res := &bundleRunResult{}
	deadline := time.After(timeout)
	for {
		select {
		case env, ok := <-c.inbox:
			if !ok {
				return res, errors.New("connection closed")
			}
			kind, _ := env["kind"].(string)
			if kind != "skill.event" {
				continue
			}
			runIDf, _ := env["run_id"].(float64)
			if int64(runIDf) != runID {
				continue
			}
			evKind, _ := env["event_kind"].(string)
			data, _ := env["data"].(map[string]any)
			switch evKind {
			case "state":
				res.State = env
			case "log":
				stream, _ := data["stream"].(string)
				line, _ := data["line"].(string)
				switch stream {
				case "stdout":
					res.StdoutLines = append(res.StdoutLines, line)
				case "stderr":
					res.StderrLines = append(res.StderrLines, line)
				default:
					// Unknown stream — record under stdout to keep it visible
					// in failure reports rather than dropping.
					res.StdoutLines = append(res.StdoutLines, "["+stream+"] "+line)
				}
			case "exit":
				res.Exit = env
			}
			if want[evKind] {
				return res, nil
			}
		case <-deadline:
			return res, fmt.Errorf("timeout waiting for run=%d event_kind in %v (stdout=%d stderr=%d lines)",
				runID, wantedEvKinds, len(res.StdoutLines), len(res.StderrLines))
		}
	}
}

// bundlesDir returns the bundle install root the agent and the
// scenarios share. Empty if not configured (callers should error
// rather than guess).
func bundlesDir() string {
	return os.Getenv("POLAR_AGENT_BUNDLES_DIR")
}

// stageBundleOnDisk writes a fake "already installed" bundle into the
// agent's bundles dir. files maps relative paths (e.g.
// "scripts/run.sh") to file contents. Files with .sh extension are
// chmod 0755 so they can exec without an extra step.
//
// This simulates "agent already has this bundle cached" so a
// skill.start without download_url succeeds immediately.
func stageBundleOnDisk(name, version string, files map[string]string) error {
	root := bundlesDir()
	if root == "" {
		return errors.New("POLAR_AGENT_BUNDLES_DIR not set — orchestrator should propagate it")
	}
	dest := filepath.Join(root, name, version)
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dest, err)
	}
	for rel, content := range files {
		clean := filepath.Clean(rel)
		if strings.HasPrefix(clean, "..") || filepath.IsAbs(clean) {
			return fmt.Errorf("staging path escapes bundle: %q", rel)
		}
		full := filepath.Join(dest, clean)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		mode := os.FileMode(0o644)
		if strings.HasSuffix(clean, ".sh") {
			mode = 0o755
		}
		if err := os.WriteFile(full, []byte(content), mode); err != nil {
			return err
		}
	}
	return nil
}

// removeStagedBundle deletes the staged bundle dir — scenarios should
// call this in deferred cleanup so a re-run on the same name+version
// works.
func removeStagedBundle(name, version string) {
	root := bundlesDir()
	if root == "" {
		return
	}
	_ = os.RemoveAll(filepath.Join(root, name, version))
}

// buildBundleZip produces an in-memory .skill (ZIP) whose top-level
// directory matches `name`. files maps relative-to-top-dir paths
// (e.g. "scripts/run.sh") to content.
func buildBundleZip(name string, files map[string]string) ([]byte, string, error) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	// Top-level dir entry — mirrors how `zip -r foo.skill foo/` packs it.
	if _, err := zw.Create(name + "/"); err != nil {
		return nil, "", err
	}
	for path, content := range files {
		hdr := &zip.FileHeader{
			Name:   name + "/" + path,
			Method: zip.Deflate,
		}
		mode := os.FileMode(0o644)
		if strings.HasSuffix(path, ".sh") {
			mode = 0o755
		}
		hdr.SetMode(mode)
		w, err := zw.CreateHeader(hdr)
		if err != nil {
			return nil, "", err
		}
		if _, err := io.WriteString(w, content); err != nil {
			return nil, "", err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, "", err
	}
	zipBytes := buf.Bytes()
	sum := sha256.Sum256(zipBytes)
	return zipBytes, hex.EncodeToString(sum[:]), nil
}

// startBundleHTTPServer stands up a one-shot HTTP server on a
// loopback port that serves the given bytes for any GET. Caller must
// call Close() to release the port. Bound to 127.0.0.1 so the agent
// subprocess can reach it without firewall surprises.
func startBundleHTTPServer(payload []byte) (*http.Server, string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, "", fmt.Errorf("listen loopback: %w", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/zip")
		_, _ = w.Write(payload)
	})
	srv := &http.Server{Handler: mux}
	go func() {
		_ = srv.Serve(ln)
	}()
	addr := ln.Addr().String()
	url := "http://" + addr + "/bundle.skill"
	return srv, url, nil
}

// stopBundleHTTPServer shuts down with a short grace period. Use in
// deferred cleanup.
func stopBundleHTTPServer(srv *http.Server) {
	if srv == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_ = srv.Shutdown(ctx)
}
