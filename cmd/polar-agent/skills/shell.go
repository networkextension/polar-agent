//go:build unix

package skills

// Shell skill (Host module Phase 2).
//
// Spawns `bash -i` (falls back to `sh -i`) inside a pty, then exposes
// three streams to the rest of polar-agent:
//
//   stdin  : data bytes from dock (via the agent loop's skill.stdin
//            envelope) → WriteInput → pty.Write
//   stdout : pty.Read → chunks → Event{Kind:EventStdout, data.bytes_b64}
//            → dock → browser xterm
//   resize : winsize from dock (via skill.resize) → Resize → pty.Setsize
//
// Lifecycle:
//   Start  → pty.StartWithSize bash, three goroutines: reader, idle
//            watchdog, cmd.Wait waiter
//   Stop   → SIGTERM the pgid, drain pty up to 1s, SIGKILL fallback,
//            emit EventExit, close events
//   Idle   → no stdin OR stdout for IdleTimeoutSec → Stop("idle")
//   HardCap→ runtime > 6h → Stop("hard_cap")
//
// Build tag is `unix` so Windows builds skip this file; the !unix
// stub in shell_stub.go registers nothing on Windows agents.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/creack/pty"
)

const (
	shellDefaultIdleSec    = 1800     // 30 min
	shellMaxIdleSec        = 21600    // 6 h
	shellDefaultRows       = 24
	shellDefaultCols       = 80
	shellMaxRows           = 1000
	shellMaxCols           = 1000
	shellStdoutChunkBytes  = 32 << 10 // 32 KiB
	shellIdleCheckInterval = 30 * time.Second
	shellStopDrainTimeout  = time.Second
)

// envVarsStrippedFromShell — defense-in-depth: refuse operator-supplied
// values for these. The Shell skill is admin-only and the operator
// already has the box's shell trust, but blocking these denies the
// most obvious "smuggle a library in" attack against the agent's other
// children.
var envVarsStrippedFromShell = []string{
	"LD_PRELOAD", "LD_LIBRARY_PATH", "DYLD_INSERT_LIBRARIES", "DYLD_LIBRARY_PATH",
	"DYLD_FRAMEWORK_PATH", "DYLD_FALLBACK_LIBRARY_PATH",
}

// ShellConfig is the dispatch-time config inside skill.start.
type ShellConfig struct {
	Rows           uint16            `json:"rows,omitempty"`
	Cols           uint16            `json:"cols,omitempty"`
	Cwd            string            `json:"cwd,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
	IdleTimeoutSec int               `json:"idle_timeout_sec,omitempty"`
	// Shell is the operator's preferred interpreter: "bash", "zsh",
	// or "sh". Empty / "auto" / unknown values fall back to the
	// agent's default (bash > zsh > sh). Whitelisted on purpose so
	// the picker can't smuggle an arbitrary executable path.
	Shell string `json:"shell,omitempty"`
}

// shellAllowed is the whitelist of shells the picker may request.
// `sh` is included so minimal containers still work; anything outside
// this set falls back to the agent's auto-detect.
var shellAllowed = map[string]bool{"bash": true, "zsh": true, "sh": true}

type shellSkill struct {
	defaultWorkdir string
}

// NewShellSkill constructs the agent's Shell skill. The defaultWorkdir
// is used as the cwd when ShellConfig.Cwd is empty.
func NewShellSkill(defaultWorkdir string) Skill {
	return &shellSkill{defaultWorkdir: defaultWorkdir}
}

func (s *shellSkill) Kind() SkillKind { return KindShell }
func (s *shellSkill) Version() string { return "1.0" }

func (s *shellSkill) Capabilities() map[string]any {
	return map[string]any{
		"protocol":          "pty-bytes",
		"default_shell":     "bash",
		"max_rows":          shellMaxRows,
		"max_cols":          shellMaxCols,
		"default_idle_sec":  shellDefaultIdleSec,
		"hard_cap_sec":      shellMaxIdleSec,
		"stdout_chunk_size": shellStdoutChunkBytes,
	}
}

func (s *shellSkill) Validate(config json.RawMessage) error {
	if len(config) == 0 || string(config) == "null" {
		return nil
	}
	var cfg ShellConfig
	if err := json.Unmarshal(config, &cfg); err != nil {
		return err
	}
	if cfg.Rows > shellMaxRows || cfg.Cols > shellMaxCols {
		return fmt.Errorf("rows/cols capped at %d/%d", shellMaxRows, shellMaxCols)
	}
	if cfg.IdleTimeoutSec > shellMaxIdleSec {
		return fmt.Errorf("idle_timeout_sec capped at %d (6h)", shellMaxIdleSec)
	}
	if cfg.Cwd != "" {
		if !filepath.IsAbs(cfg.Cwd) {
			return fmt.Errorf("cwd must be absolute, got %q", cfg.Cwd)
		}
		info, err := os.Stat(cfg.Cwd)
		if err != nil {
			return fmt.Errorf("cwd %q: %w", cfg.Cwd, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("cwd %q is not a directory", cfg.Cwd)
		}
	}
	for k := range cfg.Env {
		if strings.ContainsAny(k, "=\x00") {
			return fmt.Errorf("invalid env key %q", k)
		}
		for _, banned := range envVarsStrippedFromShell {
			if strings.EqualFold(k, banned) {
				return fmt.Errorf("env key %q is not allowed for shell skill", k)
			}
		}
	}
	return nil
}

func (s *shellSkill) Start(ctx context.Context, runID int64, config json.RawMessage) (Run, error) {
	var cfg ShellConfig
	if len(config) > 0 && string(config) != "null" {
		if err := json.Unmarshal(config, &cfg); err != nil {
			return nil, fmt.Errorf("parse shell config: %w", err)
		}
	}
	if cfg.Rows == 0 {
		cfg.Rows = shellDefaultRows
	}
	if cfg.Cols == 0 {
		cfg.Cols = shellDefaultCols
	}
	if cfg.Rows > shellMaxRows {
		cfg.Rows = shellMaxRows
	}
	if cfg.Cols > shellMaxCols {
		cfg.Cols = shellMaxCols
	}
	if cfg.IdleTimeoutSec <= 0 {
		cfg.IdleTimeoutSec = shellDefaultIdleSec
	}
	if cfg.IdleTimeoutSec > shellMaxIdleSec {
		cfg.IdleTimeoutSec = shellMaxIdleSec
	}
	cwd := cfg.Cwd
	if cwd == "" {
		cwd = s.defaultWorkdir
	}

	// Pick a shell, in priority order:
	//   1) Operator preference (cfg.Shell, whitelisted) — explicit win
	//   2) $SHELL env var (the user's actual default shell). Matters
	//      a lot on macOS where SHELL=/bin/zsh: bash -i wouldn't source
	//      ~/.zshrc, so brew shellenv / PATH / aliases the operator
	//      configured for their daily shell would be invisible, and
	//      `clear` / other commands installed via brew end up "not found".
	//      Using $SHELL means the rich rc file actually loads.
	//   3) Hardcoded fallback (bash > zsh > sh) for environments that
	//      somehow have no SHELL set (containers, minimal Alpine).
	shellPath, shellName := "", ""
	if pref := strings.ToLower(strings.TrimSpace(cfg.Shell)); pref != "" && shellAllowed[pref] {
		if p, err := exec.LookPath(pref); err == nil {
			shellPath, shellName = p, pref
		}
	}
	if shellPath == "" {
		if envShell := strings.TrimSpace(os.Getenv("SHELL")); envShell != "" {
			base := strings.ToLower(filepath.Base(envShell))
			if shellAllowed[base] {
				if _, err := os.Stat(envShell); err == nil {
					shellPath, shellName = envShell, base
				}
			}
		}
	}
	if shellPath == "" {
		for _, candidate := range []string{"bash", "zsh", "sh"} {
			if p, err := exec.LookPath(candidate); err == nil {
				shellPath, shellName = p, candidate
				break
			}
		}
	}
	if shellPath == "" {
		return nil, errors.New("no usable shell (bash, zsh, sh) on PATH")
	}

	runCtx, cancel := context.WithCancel(ctx)
	// `-i` (interactive only, NOT login). `-il` was tried but bash/zsh
	// login files on some user envs hang or break input flow at
	// session start (likely an interactive-prompt sourced from
	// .bash_profile / .zprofile that fights xterm). Keep `-i`; rely
	// on launchd plist EnvironmentVariables for PATH, and let
	// operators source .bashrc themselves from the pane if they need
	// brew shellenv / NVM / etc.
	cmd := exec.CommandContext(runCtx, shellPath, "-i")
	if cwd != "" {
		cmd.Dir = cwd
	}
	cmd.Env = buildShellEnv(cfg.Env)
	// Put bash in its own process group so we can signal the whole
	// session (it forks subshells, jobs, etc.) without taking out the
	// agent.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	tty, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: cfg.Rows, Cols: cfg.Cols})
	if err != nil {
		cancel()
		return nil, fmt.Errorf("pty.Start: %w", err)
	}

	run := &shellRun{
		id:             runID,
		cfg:            cfg,
		shellPath:      shellPath,
		shellName:      shellName,
		pty:            tty,
		cmd:            cmd,
		events:         make(chan Event, 32),
		ctxCancel:      cancel,
		idleTimeout:    time.Duration(cfg.IdleTimeoutSec) * time.Second,
		hardCapTimeout: shellMaxIdleSec * time.Second,
		startedAt:      time.Now(),
	}
	run.lastActivity.Store(time.Now().UnixNano())

	// Initial state event so dock can flip status starting → running.
	run.send(Event{Kind: EventState, Data: map[string]any{
		"status": "running",
		"pid":    cmd.Process.Pid,
		"rows":   cfg.Rows,
		"cols":   cfg.Cols,
		"shell":  shellName,
	}})

	go run.readerLoop(runCtx)
	go run.idleWatchdog(runCtx)
	go run.waiterLoop()

	return run, nil
}

// buildShellEnv merges os.Environ with operator-supplied overrides,
// stripping the danger keys. The operator's PATH (if set) overrides
// the agent's — they want their tools.
func buildShellEnv(overrides map[string]string) []string {
	out := make([]string, 0, len(os.Environ())+len(overrides))
	used := make(map[string]bool, len(overrides))
	hasTerm := false
	for _, e := range os.Environ() {
		eq := strings.IndexByte(e, '=')
		if eq <= 0 {
			out = append(out, e)
			continue
		}
		key := e[:eq]
		if isStrippedEnvKey(key) {
			continue
		}
		if _, ok := overrides[key]; ok {
			continue // overridden below
		}
		if strings.EqualFold(key, "TERM") {
			hasTerm = true
		}
		out = append(out, e)
	}
	for k, v := range overrides {
		if isStrippedEnvKey(k) {
			continue
		}
		if strings.ContainsAny(k, "=\x00") {
			continue
		}
		if strings.EqualFold(k, "TERM") {
			hasTerm = true
		}
		out = append(out, k+"="+v)
		used[k] = true
	}
	// Launchd-spawned agents don't inherit TERM, so ncurses programs
	// (vim, top, less, htop) error with "Error opening terminal:
	// unknown" or render blind (no cursor positioning, blank top).
	// xterm.js announces itself as xterm-256color over the PTY-bytes
	// protocol; set it explicitly when nothing else has.
	if !hasTerm {
		out = append(out, "TERM=xterm-256color")
	}
	_ = used
	return out
}

func isStrippedEnvKey(key string) bool {
	for _, banned := range envVarsStrippedFromShell {
		if strings.EqualFold(key, banned) {
			return true
		}
	}
	return false
}

// shellRun is the long-lived Run for a single pty + bash session.
// Implements Run + RunInput + RunResizer.
type shellRun struct {
	id        int64
	cfg       ShellConfig
	shellPath string
	shellName string
	pty       *os.File
	cmd       *exec.Cmd

	events       chan Event
	ctxCancel    context.CancelFunc
	idleTimeout  time.Duration
	hardCapTimeout time.Duration
	startedAt    time.Time

	lastActivity atomic.Int64 // unix nanos
	stopOnce     sync.Once
	stopReason   atomic.Value // string
	closed       atomic.Bool
}

func (r *shellRun) ID() int64                 { return r.id }
func (r *shellRun) Events() <-chan Event      { return r.events }
func (r *shellRun) Stop(reason string) error  { return r.stop(reason) }

// WriteInput pumps stdin bytes from dock into the pty master.
func (r *shellRun) WriteInput(data []byte) error {
	if r.closed.Load() {
		return errors.New("shell run closed")
	}
	r.lastActivity.Store(time.Now().UnixNano())
	_, err := r.pty.Write(data)
	return err
}

// Resize updates the TTY window size — `stty cols/rows` inside bash
// sees the change immediately.
func (r *shellRun) Resize(rows, cols uint16) error {
	if r.closed.Load() {
		return errors.New("shell run closed")
	}
	if rows == 0 || cols == 0 {
		return errors.New("rows/cols must be non-zero")
	}
	if rows > shellMaxRows {
		rows = shellMaxRows
	}
	if cols > shellMaxCols {
		cols = shellMaxCols
	}
	return pty.Setsize(r.pty, &pty.Winsize{Rows: rows, Cols: cols})
}

func (r *shellRun) readerLoop(ctx context.Context) {
	buf := make([]byte, shellStdoutChunkBytes)
	for {
		n, err := r.pty.Read(buf)
		if n > 0 {
			r.lastActivity.Store(time.Now().UnixNano())
			// Copy buf into a fresh slice so subsequent reads don't
			// race with downstream channel receivers.
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			r.send(Event{Kind: EventStdout, Data: map[string]any{
				"bytes_b64": base64.StdEncoding.EncodeToString(chunk),
			}})
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				r.stop("eof")
			} else if ctx.Err() != nil {
				// stop() already called via cancel; nothing to do.
			} else {
				r.stop("read_error:" + err.Error())
			}
			return
		}
	}
}

func (r *shellRun) idleWatchdog(ctx context.Context) {
	ticker := time.NewTicker(shellIdleCheckInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			if r.closed.Load() {
				return
			}
			last := time.Unix(0, r.lastActivity.Load())
			if now.Sub(last) >= r.idleTimeout {
				r.stop("idle")
				return
			}
			if now.Sub(r.startedAt) >= r.hardCapTimeout {
				r.stop("hard_cap")
				return
			}
		}
	}
}

// waiterLoop blocks on cmd.Wait() and triggers stop when bash exits
// on its own (user typed `exit`, killed by external signal, etc.).
func (r *shellRun) waiterLoop() {
	_ = r.cmd.Wait()
	if !r.closed.Load() {
		r.stop("eof")
	}
}

// stop is idempotent. The first caller wins on stopReason.
func (r *shellRun) stop(reason string) error {
	r.stopOnce.Do(func() {
		r.stopReason.Store(reason)
		r.closed.Store(true)
		// SIGTERM the whole process group so subshells / jobs die too.
		if r.cmd.Process != nil {
			pgid, perr := syscall.Getpgid(r.cmd.Process.Pid)
			if perr == nil {
				_ = syscall.Kill(-pgid, syscall.SIGTERM)
			} else {
				_ = r.cmd.Process.Signal(syscall.SIGTERM)
			}
		}
		// Give bash up to 1s to flush its output buffer cleanly. The
		// reader goroutine is still draining; close the pty after the
		// drain window so it sees EOF.
		drainDone := make(chan struct{})
		go func() {
			time.Sleep(shellStopDrainTimeout)
			close(drainDone)
		}()
		<-drainDone
		if r.cmd.Process != nil {
			// Force SIGKILL on the pgid if anything's still alive.
			pgid, perr := syscall.Getpgid(r.cmd.Process.Pid)
			if perr == nil {
				_ = syscall.Kill(-pgid, syscall.SIGKILL)
			}
		}
		_ = r.pty.Close()
		exitCode := -1
		if r.cmd.ProcessState != nil {
			exitCode = r.cmd.ProcessState.ExitCode()
		}
		r.send(Event{Kind: EventExit, Data: map[string]any{
			"ok":        exitCode == 0 || reason == "operator",
			"exit_code": exitCode,
			"reason":    reason,
		}})
		r.ctxCancel()
		close(r.events)
	})
	return nil
}

// send is non-blocking — if the consumer is gone (channel full), drop
// the event. Bytes-direction overflow is recoverable; lifecycle events
// (state/exit) are small and almost never racy in practice.
func (r *shellRun) send(ev Event) {
	select {
	case r.events <- ev:
	default:
	}
}
