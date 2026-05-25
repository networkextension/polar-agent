//go:build unix

package skills

// Bundle skill (Host module — dock-managed declarative skills).
//
// One skill kind ("bundle") dispatches to many installed bundles. The
// agent does NOT parse SKILL.md or per-bundle manifests — dock owns
// that. The agent just:
//
//   1. On skill.start, if <name>@<version> isn't already in
//      ~/.polar/bundles/, HTTP GET the .skill ZIP from download_url,
//      verify sha256, unzip into <name>/<version>/.
//   2. If the bundle ships requirements.txt, set up a per-bundle venv
//      via `uv venv` + `uv pip install` (fallback: python3 -m venv +
//      pip). Cached — second start of the same version skips it.
//   3. exec the requested entrypoint with the bundle's venv python
//      (or /bin/sh for *.sh, or direct exec for other modes). Pipe
//      stdout/stderr as EventLog frames line-buffered, with a
//      `stream` field distinguishing the two.
//   4. On Stop, SIGTERM the process group → 5s → SIGKILL fallback.
//
// Entrypoint paths are constrained to `scripts/` and rejected if they
// contain `..` or are absolute. Bundle names must not contain path
// separators. ZIP entries are checked for zip-slip.

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"bufio"
)

const (
	bundleHTTPTimeout    = 5 * time.Minute  // big bundles can be slow
	bundleMaxBytes       = 256 << 20        // 256 MiB hard cap on a single .skill
	bundleStopTimeout    = 5 * time.Second
	bundleMaxLogLineSize = 8 << 10          // 8 KiB
	bundleEventBuffer    = 64
	bundleVenvSetupBudget = 10 * time.Minute // pip install for big deps (torch et al) can be slow
)

// danger env keys — same allow-the-rest, block-the-bad list as shell.go.
var bundleStrippedEnv = []string{
	"LD_PRELOAD", "LD_LIBRARY_PATH",
	"DYLD_INSERT_LIBRARIES", "DYLD_LIBRARY_PATH",
	"DYLD_FRAMEWORK_PATH", "DYLD_FALLBACK_LIBRARY_PATH",
}

// BundleConfig is the skill.start config blob for KindBundle.
//
// Dock has already validated args against the bundle's declared
// schema; agent just sanity-checks the bundle/version/entrypoint
// triple is well-formed and not path-malicious.
type BundleConfig struct {
	Bundle      string            `json:"bundle"`              // e.g. "local-llm-serve"
	Version     string            `json:"version"`             // e.g. "0.1.0"
	DownloadURL string            `json:"download_url,omitempty"` // dock-hosted; omitted if already installed
	SHA256      string            `json:"sha256,omitempty"`     // verified after download
	Entrypoint  string            `json:"entrypoint"`           // e.g. "scripts/detect_hardware.py"
	Args        []string          `json:"args,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
}

type bundleSkill struct {
	rootDir   string // ~/.polar/bundles
	installMu sync.Mutex
	httpClient *http.Client
}

// NewBundleSkill constructs the bundle dispatcher. rootDir override is
// for tests; pass "" in production to use ~/.polar/bundles.
func NewBundleSkill(rootDir string) Skill {
	if rootDir == "" {
		home, err := os.UserHomeDir()
		if err == nil {
			rootDir = filepath.Join(home, ".polar", "bundles")
		} else {
			rootDir = filepath.Join(os.TempDir(), "polar-bundles")
		}
	}
	_ = os.MkdirAll(rootDir, 0o755)
	return &bundleSkill{
		rootDir:    rootDir,
		httpClient: &http.Client{Timeout: bundleHTTPTimeout},
	}
}

func (b *bundleSkill) Kind() SkillKind { return KindBundle }
func (b *bundleSkill) Version() string { return "0.1.0" }

func (b *bundleSkill) Capabilities() map[string]any {
	return map[string]any{
		"installed":          b.listInstalled(),
		"exec_modes":         []string{"python_venv", "shell", "exec"},
		"max_bundle_bytes":   bundleMaxBytes,
		"stop_timeout_sec":   int(bundleStopTimeout.Seconds()),
		"uv_available":       b.hasUV(),
		"python3_available":  b.hasPython3(),
	}
}

func (b *bundleSkill) hasUV() bool {
	_, err := exec.LookPath("uv")
	return err == nil
}

func (b *bundleSkill) hasPython3() bool {
	_, err := exec.LookPath("python3")
	return err == nil
}

// installedBundle is one entry in the advertise capabilities payload.
type installedBundle struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	HasVenv bool   `json:"has_venv"`
}

func (b *bundleSkill) listInstalled() []installedBundle {
	out := []installedBundle{}
	nameEntries, err := os.ReadDir(b.rootDir)
	if err != nil {
		return out
	}
	for _, ne := range nameEntries {
		if !ne.IsDir() || strings.HasPrefix(ne.Name(), ".") {
			continue
		}
		verDir := filepath.Join(b.rootDir, ne.Name())
		versions, err := os.ReadDir(verDir)
		if err != nil {
			continue
		}
		for _, ve := range versions {
			if !ve.IsDir() || strings.HasPrefix(ve.Name(), ".") {
				continue
			}
			bd := filepath.Join(verDir, ve.Name())
			_, venvErr := os.Stat(filepath.Join(bd, ".venv"))
			out = append(out, installedBundle{
				Name:    ne.Name(),
				Version: ve.Name(),
				HasVenv: venvErr == nil,
			})
		}
	}
	return out
}

// Validate is called by the agent before Start. Catches obviously bad
// configs without paying download cost.
func (b *bundleSkill) Validate(config json.RawMessage) error {
	var cfg BundleConfig
	if err := json.Unmarshal(config, &cfg); err != nil {
		return fmt.Errorf("parse bundle config: %w", err)
	}
	return b.validateConfig(&cfg)
}

func (b *bundleSkill) validateConfig(cfg *BundleConfig) error {
	if cfg.Bundle == "" {
		return errors.New("bundle name required")
	}
	if cfg.Version == "" {
		return errors.New("bundle version required")
	}
	if cfg.Entrypoint == "" {
		return errors.New("entrypoint required")
	}
	if !isSafeBundleName(cfg.Bundle) {
		return fmt.Errorf("invalid bundle name: %q", cfg.Bundle)
	}
	if !isSafeBundleName(cfg.Version) {
		return fmt.Errorf("invalid version: %q", cfg.Version)
	}
	if !isSafeEntrypoint(cfg.Entrypoint) {
		return fmt.Errorf("invalid entrypoint (must be under scripts/, no traversal): %q", cfg.Entrypoint)
	}
	for k := range cfg.Env {
		if strings.ContainsAny(k, "=\x00") {
			return fmt.Errorf("invalid env key: %q", k)
		}
		for _, banned := range bundleStrippedEnv {
			if strings.EqualFold(k, banned) {
				return fmt.Errorf("env key %q is not allowed for bundle skill", k)
			}
		}
	}
	return nil
}

// isSafeBundleName rejects path separators, leading dots, and empty
// components. Lets dock keep ids like "local-llm-serve" or "0.1.0".
func isSafeBundleName(s string) bool {
	if s == "" || strings.HasPrefix(s, ".") {
		return false
	}
	if strings.ContainsAny(s, "/\\\x00") {
		return false
	}
	if strings.Contains(s, "..") {
		return false
	}
	return true
}

// isSafeEntrypoint constrains paths to forward-slash, relative,
// under scripts/, with no parent traversal.
func isSafeEntrypoint(p string) bool {
	if p == "" {
		return false
	}
	if filepath.IsAbs(p) || strings.HasPrefix(p, "/") || strings.HasPrefix(p, "\\") {
		return false
	}
	clean := filepath.Clean(p)
	if clean == ".." || strings.HasPrefix(clean, "../") || strings.Contains(clean, "/../") {
		return false
	}
	// Require explicit scripts/ prefix. This is intentionally strict —
	// dock-side bundle authors should put callable entrypoints under
	// scripts/, and references/ / docs/ stay readable but not executable.
	if !strings.HasPrefix(clean, "scripts/") {
		return false
	}
	return true
}

// Start implements the Skill contract. Installs the bundle if needed,
// then exec's the entrypoint and returns a bundleRun pumping events.
func (b *bundleSkill) Start(ctx context.Context, runID int64, config json.RawMessage) (Run, error) {
	var cfg BundleConfig
	if err := json.Unmarshal(config, &cfg); err != nil {
		return nil, fmt.Errorf("parse bundle config: %w", err)
	}
	if err := b.validateConfig(&cfg); err != nil {
		return nil, err
	}

	bundleDir := filepath.Join(b.rootDir, cfg.Bundle, cfg.Version)
	if _, err := os.Stat(bundleDir); errors.Is(err, os.ErrNotExist) {
		if err := b.install(ctx, cfg, bundleDir); err != nil {
			return nil, fmt.Errorf("install %s@%s: %w", cfg.Bundle, cfg.Version, err)
		}
	} else if err != nil {
		return nil, fmt.Errorf("stat bundle dir: %w", err)
	}

	scriptPath := filepath.Join(bundleDir, filepath.FromSlash(cfg.Entrypoint))
	// Re-resolve via filepath.EvalSymlinks would be safer but isn't
	// strictly needed: isSafeEntrypoint already blocks `..`, and zip
	// extraction also blocked zip-slip on install. Symlinks inside an
	// installed bundle are an operator-trust matter.
	if _, err := os.Stat(scriptPath); err != nil {
		return nil, fmt.Errorf("entrypoint not found: %w", err)
	}

	cmd, err := b.buildCommand(ctx, bundleDir, scriptPath, cfg)
	if err != nil {
		return nil, err
	}

	stdoutR, stdoutW := io.Pipe()
	stderrR, stderrW := io.Pipe()
	cmd.Stdout = stdoutW
	cmd.Stderr = stderrW

	if err := cmd.Start(); err != nil {
		_ = stdoutW.Close()
		_ = stderrW.Close()
		return nil, fmt.Errorf("exec start: %w", err)
	}

	runCtx, cancel := context.WithCancel(ctx)
	run := &bundleRun{
		id:        runID,
		cmd:       cmd,
		events:    make(chan Event, bundleEventBuffer),
		ctxCancel: cancel,
		stdoutR:   stdoutR,
		stdoutW:   stdoutW,
		stderrR:   stderrR,
		stderrW:   stderrW,
		startedAt: time.Now(),
	}

	run.send(Event{Kind: EventState, Data: map[string]any{
		"status":     "running",
		"pid":        cmd.Process.Pid,
		"bundle":     cfg.Bundle,
		"version":    cfg.Version,
		"entrypoint": cfg.Entrypoint,
	}})

	go run.logPump(runCtx, run.stdoutR, "stdout")
	go run.logPump(runCtx, run.stderrR, "stderr")
	go run.waiter()

	return run, nil
}

// buildCommand picks the right interpreter for the entrypoint and
// returns a configured *exec.Cmd. Caller still needs to set Stdout/Stderr
// and call Start.
func (b *bundleSkill) buildCommand(ctx context.Context, bundleDir, scriptPath string, cfg BundleConfig) (*exec.Cmd, error) {
	var cmd *exec.Cmd
	switch {
	case strings.HasSuffix(scriptPath, ".py"):
		venvPy := filepath.Join(bundleDir, ".venv", "bin", "python")
		py := ""
		if _, err := os.Stat(venvPy); err == nil {
			py = venvPy
		} else if p, err := exec.LookPath("python3"); err == nil {
			py = p
		} else if p, err := exec.LookPath("python"); err == nil {
			py = p
		} else {
			return nil, errors.New("no python interpreter (need python3 on PATH or a .venv in the bundle)")
		}
		args := append([]string{scriptPath}, cfg.Args...)
		cmd = exec.CommandContext(ctx, py, args...)
	case strings.HasSuffix(scriptPath, ".sh"):
		args := append([]string{scriptPath}, cfg.Args...)
		cmd = exec.CommandContext(ctx, "/bin/sh", args...)
	default:
		// Direct exec — require the bit to be set, otherwise reject so
		// we don't silently fall through to "permission denied" buried
		// in stderr.
		info, err := os.Stat(scriptPath)
		if err != nil {
			return nil, fmt.Errorf("stat entrypoint: %w", err)
		}
		if info.Mode()&0o111 == 0 {
			return nil, fmt.Errorf("entrypoint %s is not executable (chmod +x or use a .py/.sh extension)", cfg.Entrypoint)
		}
		cmd = exec.CommandContext(ctx, scriptPath, cfg.Args...)
	}
	cmd.Dir = bundleDir
	cmd.Env = buildBundleEnv(cfg.Env)
	// Put the child in its own process group so SIGTERM/SIGKILL on
	// Stop reaches subprocesses (e.g. pip install spawning helpers).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return cmd, nil
}

func buildBundleEnv(overrides map[string]string) []string {
	out := make([]string, 0, len(os.Environ())+len(overrides))
	overrideKeys := make(map[string]bool, len(overrides))
	for _, e := range os.Environ() {
		eq := strings.IndexByte(e, '=')
		if eq <= 0 {
			out = append(out, e)
			continue
		}
		key := e[:eq]
		if isStrippedBundleEnvKey(key) {
			continue
		}
		if _, ok := overrides[key]; ok {
			continue
		}
		out = append(out, e)
	}
	for k, v := range overrides {
		if isStrippedBundleEnvKey(k) {
			continue
		}
		if strings.ContainsAny(k, "=\x00") {
			continue
		}
		out = append(out, k+"="+v)
		overrideKeys[k] = true
	}
	return out
}

func isStrippedBundleEnvKey(key string) bool {
	for _, banned := range bundleStrippedEnv {
		if strings.EqualFold(key, banned) {
			return true
		}
	}
	return false
}

// install downloads the bundle ZIP to a tmpfile, sha256-verifies it,
// and unzips to dest. Atomic via a staging dir + rename. Then runs
// venv setup if requirements.txt is present.
func (b *bundleSkill) install(ctx context.Context, cfg BundleConfig, dest string) error {
	if cfg.DownloadURL == "" {
		return errors.New("bundle not installed and download_url is empty")
	}

	// Serialize installs across all goroutines — protects against two
	// concurrent skill.start envelopes both trying to install the same
	// version (race on the staging dir rename).
	b.installMu.Lock()
	defer b.installMu.Unlock()

	// Re-check after acquiring the lock — another goroutine may have
	// installed it while we were blocked.
	if _, err := os.Stat(dest); err == nil {
		return nil
	}

	req, err := http.NewRequestWithContext(ctx, "GET", cfg.DownloadURL, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	resp, err := b.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download status %d", resp.StatusCode)
	}

	// Stream to a tmpfile and hash on the fly — avoids holding the full
	// bundle in memory (could be hundreds of MB for model bundles).
	tmpZip, err := os.CreateTemp(b.rootDir, "."+cfg.Bundle+"-"+cfg.Version+"-*.zip")
	if err != nil {
		return fmt.Errorf("create tmp zip: %w", err)
	}
	tmpPath := tmpZip.Name()
	defer os.Remove(tmpPath)

	hasher := sha256.New()
	limited := io.LimitReader(resp.Body, bundleMaxBytes+1)
	written, err := io.Copy(io.MultiWriter(tmpZip, hasher), limited)
	_ = tmpZip.Close()
	if err != nil {
		return fmt.Errorf("download body: %w", err)
	}
	if written > bundleMaxBytes {
		return fmt.Errorf("bundle exceeds max size %d bytes", bundleMaxBytes)
	}

	if cfg.SHA256 != "" {
		got := hex.EncodeToString(hasher.Sum(nil))
		want := strings.ToLower(strings.TrimSpace(cfg.SHA256))
		if got != want {
			return fmt.Errorf("sha256 mismatch: got %s want %s", got, want)
		}
	}

	staging := dest + ".staging"
	_ = os.RemoveAll(staging)
	if err := unzipBundle(tmpPath, staging, cfg.Bundle); err != nil {
		_ = os.RemoveAll(staging)
		return fmt.Errorf("unzip: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		_ = os.RemoveAll(staging)
		return fmt.Errorf("mkdir parent: %w", err)
	}
	if err := os.Rename(staging, dest); err != nil {
		_ = os.RemoveAll(staging)
		return fmt.Errorf("rename staging: %w", err)
	}

	// Venv setup is best-effort — if it fails we leave the bundle
	// installed without .venv and let the operator decide how to handle
	// it. The next Start without a .venv will fall back to system
	// python3, which the operator can then `pip install` manually.
	reqFile := filepath.Join(dest, "requirements.txt")
	if _, err := os.Stat(reqFile); err == nil {
		venvCtx, cancel := context.WithTimeout(ctx, bundleVenvSetupBudget)
		defer cancel()
		if err := b.setupVenv(venvCtx, dest, reqFile); err != nil {
			return fmt.Errorf("venv setup: %w", err)
		}
	}
	return nil
}

// unzipBundle extracts zipPath into dest. expectedTopDir is the
// single top-level directory name we expect inside the ZIP
// (matching the skill's `name`); entries are unwrapped so the bundle
// content sits directly at dest/ — not dest/<name>/.
//
// Zip slip protection: any entry that, after Clean, escapes dest is
// rejected. Symlink entries are also rejected to keep the trust
// surface small.
func unzipBundle(zipPath, dest, expectedTopDir string) error {
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return err
	}
	defer r.Close()

	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}
	absDest, err := filepath.Abs(dest)
	if err != nil {
		return err
	}

	prefix := expectedTopDir + "/"

	for _, f := range r.File {
		name := f.Name
		// Strip the expected top-level dir; leave entries without that
		// prefix (some bundle producers may flatten) untouched.
		switch {
		case name == expectedTopDir, name == expectedTopDir+"/":
			continue
		case strings.HasPrefix(name, prefix):
			name = strings.TrimPrefix(name, prefix)
		}
		if name == "" {
			continue
		}

		// Reject symlinks defensively.
		if f.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink entries not supported: %s", f.Name)
		}

		cleaned := filepath.Clean(name)
		if cleaned == "." || cleaned == ".." {
			continue
		}
		if strings.HasPrefix(cleaned, "..") || filepath.IsAbs(cleaned) {
			return fmt.Errorf("zip slip: %s", f.Name)
		}
		target := filepath.Join(absDest, cleaned)
		// Final check: target must be under absDest.
		rel, err := filepath.Rel(absDest, target)
		if err != nil || strings.HasPrefix(rel, "..") {
			return fmt.Errorf("zip slip (relpath check): %s", f.Name)
		}

		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		mode := f.Mode().Perm()
		if mode == 0 {
			mode = 0o644
		}
		w, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
		if err != nil {
			rc.Close()
			return err
		}
		_, err = io.Copy(w, rc)
		rc.Close()
		w.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// setupVenv prefers `uv` (fast + self-managing) and falls back to
// `python3 -m venv` + `pip install -r`.  Either path leaves a
// usable interpreter at .venv/bin/python.
func (b *bundleSkill) setupVenv(ctx context.Context, bundleDir, reqFile string) error {
	venvDir := filepath.Join(bundleDir, ".venv")

	if _, err := exec.LookPath("uv"); err == nil {
		// uv venv creates the env; uv pip install uses --python to
		// target it without needing to source activate scripts.
		if out, err := runWithCtx(ctx, bundleDir, "uv", "venv", venvDir); err != nil {
			return fmt.Errorf("uv venv: %w: %s", err, out)
		}
		py := filepath.Join(venvDir, "bin", "python")
		if out, err := runWithCtx(ctx, bundleDir, "uv", "pip", "install", "--python", py, "-r", reqFile); err != nil {
			return fmt.Errorf("uv pip install: %w: %s", err, out)
		}
		return nil
	}

	if _, err := exec.LookPath("python3"); err != nil {
		return errors.New("neither uv nor python3 found on PATH — install one to enable venv-based bundles")
	}
	if out, err := runWithCtx(ctx, bundleDir, "python3", "-m", "venv", venvDir); err != nil {
		return fmt.Errorf("python3 -m venv: %w: %s", err, out)
	}
	pip := filepath.Join(venvDir, "bin", "pip")
	if out, err := runWithCtx(ctx, bundleDir, pip, "install", "-r", reqFile); err != nil {
		return fmt.Errorf("pip install: %w: %s", err, out)
	}
	return nil
}

func runWithCtx(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	return cmd.CombinedOutput()
}

// bundleRun is the long-lived Run for one bundle exec. Mirrors the
// proxy.go pattern (run-to-completion, line-buffered logs, no stdin)
// rather than the shell.go pattern (interactive pty).
type bundleRun struct {
	id        int64
	cmd       *exec.Cmd
	events    chan Event
	ctxCancel context.CancelFunc

	stdoutR *io.PipeReader
	stdoutW *io.PipeWriter
	stderrR *io.PipeReader
	stderrW *io.PipeWriter

	startedAt time.Time
	stopOnce  sync.Once
	closed    sync.WaitGroup // wait for both logPumps to drain before close(events)
	pumpsDone chan struct{}  // closed when both pumps have returned
	eventsClosed atomicBool
}

// atomicBool is a tiny inline replacement to keep the file dep-free
// (using sync/atomic.Bool needs go 1.19+, which this module already
// requires, but keeping the surface small).
type atomicBool struct {
	mu sync.Mutex
	v  bool
}

func (a *atomicBool) Set(v bool)   { a.mu.Lock(); a.v = v; a.mu.Unlock() }
func (a *atomicBool) Get() bool    { a.mu.Lock(); defer a.mu.Unlock(); return a.v }

func (r *bundleRun) ID() int64            { return r.id }
func (r *bundleRun) Events() <-chan Event { return r.events }

func (r *bundleRun) send(ev Event) {
	if r.eventsClosed.Get() {
		return
	}
	select {
	case r.events <- ev:
	default:
		// Drop on full — keeping the agent responsive matters more than
		// every single log line surviving. State + exit events are
		// emitted only at lifecycle boundaries so they're rarely racy.
	}
}

func (r *bundleRun) logPump(ctx context.Context, rd *io.PipeReader, stream string) {
	defer rd.Close()
	scanner := bufio.NewScanner(rd)
	scanner.Buffer(make([]byte, bundleMaxLogLineSize), bundleMaxLogLineSize)
	for scanner.Scan() {
		if ctx.Err() != nil {
			return
		}
		line := scanner.Text()
		r.send(Event{Kind: EventLog, Data: map[string]any{
			"stream": stream,
			"line":   line,
		}})
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, io.ErrClosedPipe) {
		r.send(Event{Kind: EventLog, Data: map[string]any{
			"stream": stream,
			"line":   "[bundle: " + stream + " pump error: " + err.Error() + "]",
		}})
	}
}

// waiter blocks on cmd.Wait, then sends EventExit and closes the
// events channel. Closing the pipe writers (Stdout/Stderr) lets the
// logPumps return so we don't close events while they're still
// writing.
func (r *bundleRun) waiter() {
	err := r.cmd.Wait()
	// Close the pipe writers so the logPumps see EOF and return.
	_ = r.stdoutW.Close()
	_ = r.stderrW.Close()

	exitCode := -1
	if r.cmd.ProcessState != nil {
		exitCode = r.cmd.ProcessState.ExitCode()
	}
	ok := exitCode == 0 && err == nil
	reason := ""
	if err != nil {
		reason = err.Error()
	}

	r.send(Event{Kind: EventExit, Data: map[string]any{
		"ok":        ok,
		"exit_code": exitCode,
		"reason":    reason,
	}})

	r.eventsClosed.Set(true)
	close(r.events)
	r.ctxCancel()
}

// Stop signals SIGTERM to the process group, waits up to
// bundleStopTimeout for graceful exit, then escalates to SIGKILL.
// Idempotent — subsequent calls are no-ops.
func (r *bundleRun) Stop(reason string) error {
	r.stopOnce.Do(func() {
		if r.cmd.Process == nil {
			return
		}
		pgid, perr := syscall.Getpgid(r.cmd.Process.Pid)
		if perr == nil {
			_ = syscall.Kill(-pgid, syscall.SIGTERM)
		} else {
			_ = r.cmd.Process.Signal(syscall.SIGTERM)
		}

		done := make(chan struct{})
		go func() {
			// ProcessState becomes non-nil after Wait returns; that's
			// what waiter() polls. Spin on it briefly instead of
			// blocking Wait again from here (Wait can only be called
			// once and is already running in waiter()).
			for {
				if r.cmd.ProcessState != nil {
					close(done)
					return
				}
				time.Sleep(50 * time.Millisecond)
			}
		}()
		select {
		case <-done:
			return
		case <-time.After(bundleStopTimeout):
			if pgid, perr := syscall.Getpgid(r.cmd.Process.Pid); perr == nil {
				_ = syscall.Kill(-pgid, syscall.SIGKILL)
			} else {
				_ = r.cmd.Process.Kill()
			}
		}
	})
	return nil
}
