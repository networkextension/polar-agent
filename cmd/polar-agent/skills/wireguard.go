//go:build unix

package skills

// WireGuard skill (Host module Phase 4).
//
// Spawns `wg-quick up <iface>` with operator-supplied .conf text.
// wg-quick reads /etc/wireguard/<iface>.conf by default — we write
// the operator's config to that path before invoking, and delete it
// on Stop (after `wg-quick down`). Lifecycle parallels the Proxy
// skill (sing-box):
//
//   Start  → resolve wg-quick on PATH, write config to
//            /etc/wireguard/<iface>.conf, exec `wg-quick up <iface>`,
//            emit state{running,iface,binary}. stdout/stderr
//            streamed as EventLog. cmd.Wait waiter handles exit
//            (wg-quick up itself returns once the interface is up
//            — then the tunnel is alive in-kernel; we keep the run
//            alive by waiting for explicit Stop or context cancel).
//   Stop   → exec `wg-quick down <iface>`, clean up config file.
//            Idempotent.
//
// Privilege model: wg-quick needs root for `ip link` + route
// manipulation + /etc/wireguard write. The skill doesn't bake sudo
// in — operator's deployment decides:
//   - run polar-agent as root (simple, broad)
//   - configure passwordless sudoers for `wg-quick` (recommended)
//   - use userspace wireguard-go (no privilege but more setup)
// Failures from missing privilege surface as EventLog{stderr} lines
// + a non-zero EventExit so the operator sees the cause in the UI.
//
// Reuses the existing skill.start + skill.event wire — no new WS
// message kinds.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	wgConfigDir         = "/etc/wireguard"
	wgMaxConfigBytes    = 64 << 10 // 64 KiB — wireguard configs are tiny (~1KiB typical)
	wgMaxLogLineBytes   = 8 << 10
	wgDefaultStopWindow = 5 * time.Second
)

// wgInterfaceNameRe restricts the operator's interface name to
// safe characters. wg-quick uses this as the basename of the
// config file and the kernel interface; allowing slashes / spaces
// would let an operator escape /etc/wireguard.
var wgInterfaceNameRe = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_-]{0,14}$`)

// WireGuardConfig is the per-run config inside skill.start.
type WireGuardConfig struct {
	// InterfaceName: the wg interface to create (max 15 chars per
	// Linux IFNAMSIZ; macOS allows longer but wg-quick clamps). If
	// empty, defaults to "polar-wg<run_id>". Must match
	// wgInterfaceNameRe.
	InterfaceName string `json:"interface_name,omitempty"`
	// ConfigText: the full .conf file body (the same shape you'd
	// drop in /etc/wireguard/wg0.conf — [Interface] + [Peer] blocks).
	ConfigText string `json:"config_text"`
	// PollIntervalSec: cadence for `wg show <iface> dump` peer-status
	// sampling, emitted as EventMetric frames. 0 = use default (30s),
	// clamped to [5, 300]. Set to <0 to disable polling entirely.
	PollIntervalSec int `json:"poll_interval_sec,omitempty"`
}

type wireguardSkill struct {
	// priv is detected once at construction and captures the exec
	// prefix (direct vs sudo -n) used for every wg-quick / wg
	// invocation. Detection result is stable for the process
	// lifetime; restart the agent if the operator changes sudoers.
	priv detectedPrivilege
}

// NewWireGuardSkill returns the WireGuard skill on unix. The stub
// in wireguard_stub.go returns nil on Windows so main.go can call
// this unconditionally.
//
// Privilege detection happens here (~one or two cheap `--help` execs).
// We always return a non-nil skill so the diagnostic Capabilities
// payload reaches dock even when the agent can't actually bring an
// iface up — that's how the UI tells "WireGuard works" from
// "WireGuard needs sudoers".
func NewWireGuardSkill() Skill {
	return &wireguardSkill{priv: detectWGPrivilege(defaultProber)}
}

// newWireGuardSkillForTest is the test-only constructor that lets a
// stub prober drive the detection branches. Lowercased so it doesn't
// leak into the public API.
func newWireGuardSkillForTest(probe cmdProber) *wireguardSkill {
	return &wireguardSkill{priv: detectWGPrivilege(probe)}
}

func (w *wireguardSkill) Kind() SkillKind { return KindWireGuard }
func (w *wireguardSkill) Version() string { return "1.0" }

func (w *wireguardSkill) Capabilities() map[string]any {
	caps := map[string]any{
		"backend":          "wg-quick",
		"config_dir":       wgConfigDir,
		"supports_log":     true,
		"stop_timeout_sec": int(wgDefaultStopWindow.Seconds()),
		// Peer-status polling is built into this skill. dock reads
		// EventMetric frames with data.kind == "wg_peer_status".
		"supports_peer_monitor":     true,
		"default_poll_interval_sec": wgDefaultPollIntervalSec,
		"min_poll_interval_sec":     wgMinPollIntervalSec,
		"max_poll_interval_sec":     wgMaxPollIntervalSec,
		// Privilege state — surfaced so dock UI can render "agent has
		// root" vs "agent needs sudoers" vs "wg-quick not installed"
		// without re-probing from dock side.
		"privilege_mode": string(w.priv.Mode),
		"exec_user":      w.priv.ExecUser,
	}
	if w.priv.WgQuickPath != "" {
		caps["installed"] = true
		caps["binary_path"] = w.priv.WgQuickPath
	} else {
		caps["installed"] = false
	}
	if w.priv.WgPath != "" {
		caps["wg_path"] = w.priv.WgPath
	} else {
		// Without `wg` we can still bring up the iface via wg-quick
		// (it uses the kernel module directly on Linux 5.6+) but the
		// peer-status poll won't work — flag it so dock UI can show
		// "install wireguard-tools to enable monitoring".
		caps["supports_peer_monitor"] = false
	}
	if w.priv.Mode == privilegeNone && w.priv.SudoersHint != "" {
		caps["sudoers_hint"] = w.priv.SudoersHint
	}
	return caps
}

func (w *wireguardSkill) Validate(config json.RawMessage) error {
	if w.priv.NotInstalled {
		return errors.New("wireguard: wg-quick not on PATH (install wireguard-tools: `apt install wireguard` / `pkg install wireguard-tools` / `brew install wireguard-tools`)")
	}
	if w.priv.Mode == privilegeNone {
		return fmt.Errorf("wireguard: agent has no root path. Add a sudoers entry and restart polar-agent — %s", w.priv.SudoersHint)
	}
	if len(config) == 0 || string(config) == "null" {
		return nil
	}
	var cfg WireGuardConfig
	if err := json.Unmarshal(config, &cfg); err != nil {
		return err
	}
	if strings.TrimSpace(cfg.ConfigText) == "" {
		return errors.New("wireguard: config_text is required")
	}
	if len(cfg.ConfigText) > wgMaxConfigBytes {
		return fmt.Errorf("wireguard: config_text too large (%d > %d)", len(cfg.ConfigText), wgMaxConfigBytes)
	}
	if iface := strings.TrimSpace(cfg.InterfaceName); iface != "" {
		if !wgInterfaceNameRe.MatchString(iface) {
			return fmt.Errorf("wireguard: invalid interface_name %q (allowed: [a-zA-Z][a-zA-Z0-9_-]{0,14})", iface)
		}
	}
	if cfg.PollIntervalSec > 0 {
		if cfg.PollIntervalSec < wgMinPollIntervalSec || cfg.PollIntervalSec > wgMaxPollIntervalSec {
			return fmt.Errorf("wireguard: poll_interval_sec %d out of range [%d, %d]", cfg.PollIntervalSec, wgMinPollIntervalSec, wgMaxPollIntervalSec)
		}
	}
	// Minimal sanity check on the .conf — must contain an [Interface] block
	// with PrivateKey. wg-quick will reject malformed configs, but failing
	// here gives the UI a faster loop.
	if !strings.Contains(cfg.ConfigText, "[Interface]") {
		return errors.New("wireguard: config missing [Interface] section")
	}
	if !strings.Contains(cfg.ConfigText, "PrivateKey") {
		return errors.New("wireguard: config missing PrivateKey")
	}
	return nil
}

func (w *wireguardSkill) Start(ctx context.Context, runID int64, config json.RawMessage) (Run, error) {
	if len(config) == 0 || string(config) == "null" {
		return nil, errors.New("wireguard: config required (config_text + interface_name)")
	}
	var cfg WireGuardConfig
	if err := json.Unmarshal(config, &cfg); err != nil {
		return nil, fmt.Errorf("wireguard: parse config: %w", err)
	}
	if strings.TrimSpace(cfg.ConfigText) == "" {
		return nil, errors.New("wireguard: config_text is required")
	}

	if w.priv.NotInstalled {
		return nil, errors.New("wireguard: wg-quick not on PATH (install wireguard-tools)")
	}
	if w.priv.Mode == privilegeNone {
		return nil, fmt.Errorf("wireguard: no root path — %s", w.priv.SudoersHint)
	}
	binaryPath := w.priv.WgQuickPath

	iface := strings.TrimSpace(cfg.InterfaceName)
	if iface == "" {
		iface = fmt.Sprintf("polarwg%d", runID)
	}
	if !wgInterfaceNameRe.MatchString(iface) {
		return nil, fmt.Errorf("wireguard: invalid interface_name %q", iface)
	}

	// Make sure /etc/wireguard exists + write config. wg-quick reads
	// from /etc/wireguard/<iface>.conf; we own that file for the run.
	if err := os.MkdirAll(wgConfigDir, 0o700); err != nil {
		return nil, fmt.Errorf("wireguard: mkdir %s: %w (need root?)", wgConfigDir, err)
	}
	confPath := filepath.Join(wgConfigDir, iface+".conf")
	// Don't clobber an existing config — operator might be running a
	// wg manually with the same name.
	if _, err := os.Stat(confPath); err == nil {
		return nil, fmt.Errorf("wireguard: %s already exists; pick a different interface_name or remove the existing config first", confPath)
	}
	if err := os.WriteFile(confPath, []byte(cfg.ConfigText), 0o600); err != nil {
		return nil, fmt.Errorf("wireguard: write %s: %w (need root?)", confPath, err)
	}

	runCtx, cancel := context.WithCancel(ctx)
	upName, upArgs := w.priv.wgCmdArgs(binaryPath, "up", iface)
	cmd := exec.CommandContext(runCtx, upName, upArgs...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = os.Remove(confPath)
		cancel()
		return nil, fmt.Errorf("wireguard: stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = os.Remove(confPath)
		cancel()
		return nil, fmt.Errorf("wireguard: stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		_ = os.Remove(confPath)
		cancel()
		return nil, fmt.Errorf("wireguard: start wg-quick: %w", err)
	}

	pollSec := cfg.PollIntervalSec
	pollEnabled := true
	switch {
	case pollSec < 0:
		// Operator explicitly disabled polling.
		pollEnabled = false
		pollSec = 0
	case pollSec == 0:
		pollSec = wgDefaultPollIntervalSec
	}

	run := &wireguardRun{
		id:              runID,
		binary:          binaryPath,
		iface:           iface,
		confPath:        confPath,
		cmd:             cmd,
		stdout:          stdout,
		stderr:          stderr,
		events:          make(chan Event, 32),
		ctxCancel:       cancel,
		stopCh:          make(chan struct{}),
		runCtx:          runCtx,
		pollIntervalSec: pollSec,
		pollEnabled:     pollEnabled,
		priv:            w.priv,
	}

	run.send(Event{Kind: EventState, Data: map[string]any{
		"status":    "running",
		"pid":       cmd.Process.Pid,
		"interface": iface,
		"binary":    binaryPath,
	}})

	go run.streamLog("stdout", stdout)
	go run.streamLog("stderr", stderr)
	go run.waiterLoop()

	return run, nil
}

type wireguardRun struct {
	id       int64
	binary   string
	iface    string
	confPath string
	cmd      *exec.Cmd
	stdout   io.ReadCloser
	stderr   io.ReadCloser

	events    chan Event
	ctxCancel context.CancelFunc
	stopCh    chan struct{} // closed when stop() runs

	// runCtx is the parent context whose cancel both kills `wg-quick up`
	// (already attached via exec.CommandContext) AND tears down the
	// peer-monitor poll goroutine. Captured here so waiterLoop can pass
	// it to pollPeers without re-deriving from cmd.
	runCtx context.Context

	pollIntervalSec int  // seconds; clamped at Validate
	pollEnabled     bool // false when operator passed PollIntervalSec < 0

	// priv carries the detected privilege prefix into stop() and the
	// peer-monitor poller. Copied from the skill at Start time so a
	// hypothetical future re-detect doesn't race an in-flight run.
	priv detectedPrivilege

	stopOnce sync.Once
	upDone   sync.Once // wg-quick up returns once iface is up; track that for clarity
}

func (r *wireguardRun) ID() int64                 { return r.id }
func (r *wireguardRun) Events() <-chan Event      { return r.events }
func (r *wireguardRun) Stop(reason string) error  { return r.stop(reason) }

func (r *wireguardRun) streamLog(channel string, reader io.ReadCloser) {
	defer reader.Close()
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, wgMaxLogLineBytes), wgMaxLogLineBytes)
	for scanner.Scan() {
		line := scanner.Text()
		if len(line) > wgMaxLogLineBytes {
			line = line[:wgMaxLogLineBytes] + "…[truncated]"
		}
		r.send(Event{Kind: EventLog, Data: map[string]any{
			"channel": channel,
			"line":    line,
		}})
	}
}

// waiterLoop blocks on cmd.Wait. Unlike sing-box (long-lived), wg-quick
// up exits AS SOON AS the interface is up — the tunnel itself is a
// kernel module. So a clean cmd.Wait return doesn't mean the run is
// over; it means we successfully brought up the iface. We emit a
// state event for that, then hold the run open until explicit Stop /
// context cancel, then `wg-quick down`.
func (r *wireguardRun) waiterLoop() {
	waitErr := r.cmd.Wait()
	if waitErr != nil {
		// wg-quick up failed. Report exit with the failure reason.
		r.stop(formatWGExitReason(waitErr))
		return
	}
	// up succeeded; flip status to up.
	r.upDone.Do(func() {
		r.send(Event{Kind: EventState, Data: map[string]any{
			"status":    "up",
			"interface": r.iface,
		}})
	})
	// Kick off peer monitoring now that the iface is in the kernel.
	// The goroutine ends when r.runCtx is cancelled (by stop()).
	if r.pollEnabled {
		go r.pollPeers(r.runCtx)
	}
	// Now block until Stop is called (or ctx cancels). The kernel
	// owns the actual tunnel; we just need to bring it down on exit.
	<-r.stopCh
}

func formatWGExitReason(waitErr error) string {
	if waitErr == nil {
		return "exit_ok"
	}
	if exitErr, ok := waitErr.(*exec.ExitError); ok && exitErr.ProcessState != nil {
		return fmt.Sprintf("exit_code:%d", exitErr.ProcessState.ExitCode())
	}
	return "wait_error:" + waitErr.Error()
}

func (r *wireguardRun) stop(reason string) error {
	r.stopOnce.Do(func() {
		// Best-effort `wg-quick down <iface>`. If wg-quick up failed
		// earlier, down will likely also fail — that's fine, we
		// still need to clean up the config file + emit the exit
		// event.
		downCtx, downCancel := context.WithTimeout(context.Background(), wgDefaultStopWindow)
		defer downCancel()
		downName, downArgs := r.priv.wgCmdArgs(r.binary, "down", r.iface)
		downCmd := exec.CommandContext(downCtx, downName, downArgs...)
		downOut, _ := downCmd.CombinedOutput()
		if len(downOut) > 0 {
			r.send(Event{Kind: EventLog, Data: map[string]any{
				"channel": "stop",
				"line":    strings.TrimRight(string(downOut), "\n"),
			}})
		}

		exitCode := -1
		if r.cmd.ProcessState != nil {
			exitCode = r.cmd.ProcessState.ExitCode()
		}
		r.send(Event{Kind: EventExit, Data: map[string]any{
			"ok":        exitCode == 0 || reason == "operator",
			"exit_code": exitCode,
			"reason":    reason,
			"interface": r.iface,
		}})

		r.ctxCancel()
		_ = os.Remove(r.confPath)
		// Unblock the waiterLoop (if wg-quick up succeeded and it's
		// holding on stopCh) so it returns + the goroutine exits.
		close(r.stopCh)
		close(r.events)
	})
	return nil
}

func (r *wireguardRun) send(ev Event) {
	defer func() { _ = recover() }() // race on channel close ⇒ swallow
	select {
	case r.events <- ev:
	default:
		// Lifecycle events block briefly so they don't get dropped;
		// log events drop on overflow (wg-quick is chatty on bring-up).
		if ev.Kind == EventLog {
			return
		}
		select {
		case r.events <- ev:
		case <-time.After(50 * time.Millisecond):
		}
	}
}
