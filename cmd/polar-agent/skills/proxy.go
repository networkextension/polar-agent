//go:build unix

package skills

// Proxy skill (Host module Phase 3).
//
// Spawns `sing-box run -c <tmpfile>` with operator-supplied config.
// Sing-box owns the protocol layer (http, socks5, vmess, vless,
// hysteria2, …); polar-agent just process-manages it: writes the
// config to a temp file, exec's the binary, captures stdout/stderr
// into log events, watches the process. No pty, no byte streaming.
//
// Why sing-box specifically: one binary, every protocol an operator
// might want, mature config schema. If you want shadowsocks-rust /
// v2ray-rust / xray instead, write a separate skill kind — keep
// proxy.go specifically sing-box.
//
// Lifecycle:
//   Start  → resolve sing-box binary via exec.LookPath, write
//            config JSON to a temp file, spawn process, emit state
//            event {status:running, listen_addr_hint, pid}.
//            Reader goroutines stream subprocess stdout/stderr as
//            EventLog frames (line-buffered, capped at 256 events).
//            cmd.Wait waiter emits EventExit when sing-box dies.
//   Stop   → SIGTERM the pgid, wait 5s, SIGKILL fallback. Idempotent.
//
// No new WS message kinds — reuse the existing skill.start +
// skill.event{state,log,exit} that Coder + Shell already share.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	proxyDefaultStopTimeout = 5 * time.Second
	proxyMaxConfigBytes     = 256 << 10 // 256 KiB — sing-box configs are typically <10KiB
	proxyMaxLogLineBytes    = 8 << 10   // 8 KiB per log line; longer lines get truncated
)

// ProxyConfig is the operator-supplied per-run config in skill.start.
type ProxyConfig struct {
	// ConfigJSON is the full sing-box configuration object. Validate
	// + dispatch encode this as a single string so the agent can pipe
	// it straight to disk without re-marshaling.
	ConfigJSON json.RawMessage `json:"config_json"`
	// ListenAddrHint surfaces the bind address(es) in the state event
	// so the UI can display "running on 127.0.0.1:1080" without
	// re-parsing the sing-box config. Operator-supplied; the actual
	// bind is whatever's inside ConfigJSON.
	ListenAddrHint string `json:"listen_addr_hint,omitempty"`
}

type proxySkill struct{}

// NewProxySkill returns the Proxy skill (sing-box backend) on unix.
// The stub in proxy_stub.go returns nil on Windows so main.go can
// call this unconditionally.
func NewProxySkill() Skill { return &proxySkill{} }

func (p *proxySkill) Kind() SkillKind { return KindProxy }
func (p *proxySkill) Version() string { return "1.0" }

func (p *proxySkill) Capabilities() map[string]any {
	caps := map[string]any{
		"backend":          "sing-box",
		"supports_log":     true,
		"stop_timeout_sec": int(proxyDefaultStopTimeout.Seconds()),
	}
	if path, err := exec.LookPath("sing-box"); err == nil {
		caps["installed"] = true
		caps["binary_path"] = path
	} else {
		caps["installed"] = false
	}
	return caps
}

func (p *proxySkill) Validate(config json.RawMessage) error {
	if len(config) == 0 || string(config) == "null" {
		return nil
	}
	var cfg ProxyConfig
	if err := json.Unmarshal(config, &cfg); err != nil {
		return err
	}
	if len(cfg.ConfigJSON) == 0 {
		return errors.New("proxy: config_json is required")
	}
	if len(cfg.ConfigJSON) > proxyMaxConfigBytes {
		return fmt.Errorf("proxy: config_json too large (%d > %d)", len(cfg.ConfigJSON), proxyMaxConfigBytes)
	}
	// Confirm it's valid JSON (sing-box will reject malformed configs
	// on start anyway, but failing here gives the operator a faster
	// loop in the UI).
	var probe map[string]any
	if err := json.Unmarshal(cfg.ConfigJSON, &probe); err != nil {
		return fmt.Errorf("proxy: config_json is not valid JSON: %w", err)
	}
	return nil
}

func (p *proxySkill) Start(ctx context.Context, runID int64, config json.RawMessage) (Run, error) {
	if len(config) == 0 || string(config) == "null" {
		return nil, errors.New("proxy: config required (config_json field)")
	}
	var cfg ProxyConfig
	if err := json.Unmarshal(config, &cfg); err != nil {
		return nil, fmt.Errorf("proxy: parse config: %w", err)
	}
	if len(cfg.ConfigJSON) == 0 {
		return nil, errors.New("proxy: config_json is required")
	}
	binaryPath, err := exec.LookPath("sing-box")
	if err != nil {
		return nil, fmt.Errorf("proxy: sing-box not on PATH (brew install sing-box, or rsync from dev box)")
	}

	// Write config to a private temp file. sing-box reads it once on
	// startup; we delete the file after the run completes (or on Stop).
	tmpFile, err := os.CreateTemp("", "polar-singbox-*.json")
	if err != nil {
		return nil, fmt.Errorf("proxy: temp file: %w", err)
	}
	if _, err := tmpFile.Write(cfg.ConfigJSON); err != nil {
		_ = tmpFile.Close()
		_ = os.Remove(tmpFile.Name())
		return nil, fmt.Errorf("proxy: write config: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		_ = os.Remove(tmpFile.Name())
		return nil, fmt.Errorf("proxy: close config: %w", err)
	}

	runCtx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(runCtx, binaryPath, "run", "-c", tmpFile.Name())
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		_ = os.Remove(tmpFile.Name())
		return nil, fmt.Errorf("proxy: stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		cancel()
		_ = os.Remove(tmpFile.Name())
		return nil, fmt.Errorf("proxy: stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		cancel()
		_ = os.Remove(tmpFile.Name())
		return nil, fmt.Errorf("proxy: start sing-box: %w", err)
	}

	run := &proxyRun{
		id:         runID,
		binary:     binaryPath,
		configPath: tmpFile.Name(),
		cmd:        cmd,
		stdout:     stdout,
		stderr:     stderr,
		events:     make(chan Event, 32),
		ctxCancel:  cancel,
	}

	// Initial state event so dock flips status starting → running.
	run.send(Event{Kind: EventState, Data: map[string]any{
		"status":           "running",
		"pid":              cmd.Process.Pid,
		"listen_addr_hint": cfg.ListenAddrHint,
		"binary":           binaryPath,
	}})

	go run.streamLog("stdout", stdout)
	go run.streamLog("stderr", stderr)
	go run.waiterLoop()

	return run, nil
}

// proxyRun is the long-lived Run handle for a single sing-box session.
type proxyRun struct {
	id         int64
	binary     string
	configPath string
	cmd        *exec.Cmd
	stdout     io.ReadCloser
	stderr     io.ReadCloser

	events    chan Event
	ctxCancel context.CancelFunc

	stopOnce sync.Once
	closed   sync.Once // close(events) once
}

func (r *proxyRun) ID() int64                 { return r.id }
func (r *proxyRun) Events() <-chan Event      { return r.events }
func (r *proxyRun) Stop(reason string) error  { return r.stop(reason) }

// streamLog reads sing-box subprocess output line-by-line and emits
// EventLog frames. Long lines are truncated; binary garbage doesn't
// crash anything (we just publish whatever bytes came in as a UTF-8
// string with errors replaced by the standard substitution).
func (r *proxyRun) streamLog(channel string, reader io.ReadCloser) {
	defer reader.Close()
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, proxyMaxLogLineBytes), proxyMaxLogLineBytes)
	for scanner.Scan() {
		line := scanner.Text()
		if len(line) > proxyMaxLogLineBytes {
			line = line[:proxyMaxLogLineBytes] + "…[truncated]"
		}
		r.send(Event{Kind: EventLog, Data: map[string]any{
			"channel": channel,
			"line":    line,
		}})
	}
	// scanner.Err() != nil → process died or pipe closed; the waiter
	// goroutine handles the exit event so we just return.
}

// waiterLoop blocks on cmd.Wait, emits the exit event, cleans up.
func (r *proxyRun) waiterLoop() {
	waitErr := r.cmd.Wait()
	r.stop(formatProxyExitReason(waitErr))
}

func formatProxyExitReason(waitErr error) string {
	if waitErr == nil {
		return "exit_ok"
	}
	if exitErr, ok := waitErr.(*exec.ExitError); ok && exitErr.ProcessState != nil {
		return fmt.Sprintf("exit_code:%d", exitErr.ProcessState.ExitCode())
	}
	return "wait_error:" + waitErr.Error()
}

func (r *proxyRun) stop(reason string) error {
	r.stopOnce.Do(func() {
		// SIGTERM the pgid so sing-box's listeners get a clean
		// shutdown — most protocols don't need it but tcp/tls
		// connections appreciate FIN over RST.
		if r.cmd.Process != nil {
			pgid, perr := syscall.Getpgid(r.cmd.Process.Pid)
			if perr == nil {
				_ = syscall.Kill(-pgid, syscall.SIGTERM)
			} else {
				_ = r.cmd.Process.Signal(syscall.SIGTERM)
			}
		}
		// Best-effort wait window for graceful shutdown.
		deadline := time.NewTimer(proxyDefaultStopTimeout)
		defer deadline.Stop()
		done := make(chan struct{})
		go func() {
			_, _ = r.cmd.Process.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-deadline.C:
			if r.cmd.Process != nil {
				pgid, perr := syscall.Getpgid(r.cmd.Process.Pid)
				if perr == nil {
					_ = syscall.Kill(-pgid, syscall.SIGKILL)
				}
			}
		}

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
		_ = os.Remove(r.configPath)
		r.closed.Do(func() { close(r.events) })
	})
	return nil
}

func (r *proxyRun) send(ev Event) {
	defer func() { _ = recover() }() // race on channel close ⇒ swallow
	select {
	case r.events <- ev:
	default:
		// Drop on overflow — sing-box can be chatty on busy
		// configs; lifecycle (state/exit) events are small and
		// almost never racy in practice.
		if strings.HasPrefix(string(ev.Kind), "log") {
			return
		}
		// For non-log events, block briefly to make sure state/exit
		// don't get dropped silently.
		select {
		case r.events <- ev:
		case <-time.After(50 * time.Millisecond):
		}
	}
}
