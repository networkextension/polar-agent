package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// filmscan_job.go — runs claimed agent_jobs of kind filmscan_extract /
// filmscan_analyze by exec'ing the filmscan binary (pulled + verified from
// polar-release, see release_fetch.go).
//
//	extract: download the source video → `filmscan extract` (uploads audio→music
//	         lib, keyframes/faces→photo lib) → report the emitted manifest.
//	analyze: write the manifest from the payload → `filmscan analyze --from-manifest`
//	         (pulls audio from the music lib, transcribes + diarizes, pushes the
//	         SRT to film) → report done.
//
// Capability routing is enforced server-side (the claim endpoint never hands an
// analyze job to an x86 agent), so the runner just trusts the kind it received.

const filmscanJobTimeout = 3 * time.Hour

// filmscanChannel is the polar-release channel to resolve filmscan from.
// Defaults to dev (publish/finalize land there; beta/stable are gated).
func filmscanChannel() string {
	if v := strings.TrimSpace(os.Getenv("POLAR_FILMSCAN_CHANNEL")); v != "" {
		return v
	}
	return "dev"
}

// runClaimedJob dispatches a claimed job to its per-kind runner.
func runClaimedJob(ctx context.Context, cfg AgentConfig, workdir string, job claimedJob, verbose bool) error {
	switch job.Kind {
	case "filmscan_extract", "filmscan_analyze":
		return runFilmscanJob(ctx, cfg, job)
	default:
		log.Printf("[job] id=%d: unhandled kind %q — skipping", job.JobID, job.Kind)
		return nil
	}
}

type filmscanExtractPayload struct {
	VideoURL    string `json:"video_url"`
	WorkspaceID string `json:"workspace_id"`
	MediaID     string `json:"media_id"`
}

type filmscanAnalyzePayload struct {
	Manifest    json.RawMessage `json:"manifest"`
	WorkspaceID string          `json:"workspace_id"`
	MediaID     string          `json:"media_id"`
}

func runFilmscanJob(ctx context.Context, cfg AgentConfig, job claimedJob) error {
	ctx, cancel := context.WithTimeout(ctx, filmscanJobTimeout)
	defer cancel()

	bin, err := ensureReleaseBinary(ctx, cfg, "filmscan", filmscanChannel())
	if err != nil {
		return reportJobFail(ctx, cfg, job.JobID, "fetch filmscan: "+err.Error())
	}

	work, err := os.MkdirTemp("", fmt.Sprintf("filmscan-job-%d-", job.JobID))
	if err != nil {
		return reportJobFail(ctx, cfg, job.JobID, "mktemp: "+err.Error())
	}
	defer os.RemoveAll(work)

	switch job.Kind {
	case "filmscan_extract":
		return runFilmscanExtract(ctx, cfg, bin, work, job)
	case "filmscan_analyze":
		return runFilmscanAnalyze(ctx, cfg, bin, work, job)
	}
	return nil
}

func runFilmscanExtract(ctx context.Context, cfg AgentConfig, bin, work string, job claimedJob) error {
	var p filmscanExtractPayload
	if err := json.Unmarshal(job.Payload, &p); err != nil || p.VideoURL == "" {
		return reportJobFail(ctx, cfg, job.JobID, "bad extract payload (need video_url)")
	}

	video := filepath.Join(work, "source")
	if err := downloadToFile(ctx, p.VideoURL, video); err != nil {
		return reportJobFail(ctx, cfg, job.JobID, "download video: "+err.Error())
	}

	args := []string{"extract", video,
		"--server", cfg.Server, "--token", cfg.Token, "-o", work}
	if p.WorkspaceID != "" {
		args = append(args, "--workspace-id", p.WorkspaceID)
	}
	if out, err := runFilmscan(ctx, bin, args); err != nil {
		return reportJobFail(ctx, cfg, job.JobID, "extract: "+err.Error()+" :: "+tail(out))
	}

	// filmscan writes <stem>.filmscan/extract-manifest.json next to -o.
	manifest, err := readManifest(work)
	if err != nil {
		return reportJobFail(ctx, cfg, job.JobID, "read manifest: "+err.Error())
	}
	result, _ := json.Marshal(map[string]any{
		"manifest":     json.RawMessage(manifest),
		"media_id":     p.MediaID,
		"workspace_id": p.WorkspaceID,
	})
	return reportJobComplete(ctx, cfg, job.JobID, result)
}

func runFilmscanAnalyze(ctx context.Context, cfg AgentConfig, bin, work string, job claimedJob) error {
	var p filmscanAnalyzePayload
	if err := json.Unmarshal(job.Payload, &p); err != nil || len(p.Manifest) == 0 {
		return reportJobFail(ctx, cfg, job.JobID, "bad analyze payload (need manifest)")
	}

	manifestPath := filepath.Join(work, "extract-manifest.json")
	if err := os.WriteFile(manifestPath, p.Manifest, 0o644); err != nil {
		return reportJobFail(ctx, cfg, job.JobID, "write manifest: "+err.Error())
	}

	args := []string{"analyze", "--from-manifest", manifestPath,
		"--server", cfg.Server, "--token", cfg.Token, "-o", work}
	if p.WorkspaceID != "" {
		args = append(args, "--workspace-id", p.WorkspaceID)
	}
	if p.MediaID != "" {
		// Push the resulting SRT to the film movie (keyframes/faces were
		// already uploaded by the extract stage).
		args = append(args, "--media-id", p.MediaID)
	}
	if out, err := runFilmscan(ctx, bin, args); err != nil {
		return reportJobFail(ctx, cfg, job.JobID, "analyze: "+err.Error()+" :: "+tail(out))
	}
	result, _ := json.Marshal(map[string]any{"media_id": p.MediaID})
	return reportJobComplete(ctx, cfg, job.JobID, result)
}

// runFilmscan execs the binary, mirroring stdout/stderr to the agent log and
// returning the combined output (capped) for diagnostics.
func runFilmscan(ctx context.Context, bin string, args []string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	var buf bytes.Buffer
	cmd.Stdout = io.MultiWriter(&buf, logWriter{"filmscan"})
	cmd.Stderr = io.MultiWriter(&buf, logWriter{"filmscan"})
	err := cmd.Run()
	return buf.Bytes(), err
}

// readManifest finds the single <stem>.filmscan/extract-manifest.json under work.
func readManifest(work string) ([]byte, error) {
	entries, err := os.ReadDir(work)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.IsDir() && strings.HasSuffix(e.Name(), ".filmscan") {
			p := filepath.Join(work, e.Name(), "extract-manifest.json")
			if b, err := os.ReadFile(p); err == nil {
				return b, nil
			}
		}
	}
	// Fallback: a flat -o that wrote the manifest directly.
	if b, err := os.ReadFile(filepath.Join(work, "extract-manifest.json")); err == nil {
		return b, nil
	}
	return nil, fmt.Errorf("extract-manifest.json not found under %s", work)
}

func downloadToFile(ctx context.Context, url, dest string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 30 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, resp.Body)
	return err
}

func reportJobComplete(ctx context.Context, cfg AgentConfig, jobID int64, result []byte) error {
	return postJobResult(ctx, cfg, fmt.Sprintf("/api/agent/tasks/%d/complete", jobID),
		map[string]any{"result": json.RawMessage(result)})
}

func reportJobFail(ctx context.Context, cfg AgentConfig, jobID int64, msg string) error {
	log.Printf("[job] id=%d FAIL: %s", jobID, msg)
	return postJobResult(ctx, cfg, fmt.Sprintf("/api/agent/tasks/%d/fail", jobID),
		map[string]any{"error": msg})
}

func postJobResult(ctx context.Context, cfg AgentConfig, path string, body map[string]any) error {
	endpoint, err := researchCallbackURL(cfg.Server, path)
	if err != nil {
		return err
	}
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.Token)
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode >= 400 {
		return fmt.Errorf("report %s: http %d", path, resp.StatusCode)
	}
	return nil
}

// logWriter pipes a child's output stream to the agent log line-buffered-ish.
type logWriter struct{ prefix string }

func (w logWriter) Write(p []byte) (int, error) {
	for _, line := range strings.Split(strings.TrimRight(string(p), "\n"), "\n") {
		if line != "" {
			log.Printf("[%s] %s", w.prefix, line)
		}
	}
	return len(p), nil
}

func tail(b []byte) string {
	const max = 2000
	if len(b) > max {
		b = b[len(b)-max:]
	}
	return strings.TrimSpace(string(b))
}
