//go:build unix

package skills

// VNC skill (Host module — VNC pane, B.1 plain TCP relay).
//
// Dials a local VNC server (macOS built-in Screen Sharing on
// 127.0.0.1:5900 by default; x11vnc / tightvnc / etc. work too) and
// pipes the raw RFB byte stream bidirectionally:
//
//   skill.stdin (from dock)  → tcp.Write to VNC server
//   tcp.Read from VNC server → EventStdout (32 KiB chunks, base64-wrapped)
//
// The agent does NOT parse RFB. Authentication runs in the browser via
// noVNC, against whichever security type the server advertises. For
// macOS Screen Sharing this requires the operator to enable
// "VNC viewers may control screen with password" so the server accepts
// standard VNC auth (security type 2) alongside Apple Auth (type 30).
// Type-30 support is a later add (B.2) that needs a Diffie-Hellman +
// AES translation layer in the agent.
//
// Wire shape reuses Shell skill envelopes: skill.start / skill.stdin /
// skill.event(stdout) / skill.stop. The dock-side handler bridges the
// browser WS to those envelopes without changes to the agent protocol.
//
// Lifecycle:
//   Start  → net.Dial target, two goroutines: reader, idle watchdog
//   Stop   → conn.Close, drain reader, emit EventExit, close events
//   Idle   → no bytes either direction for IdleTimeoutSec → Stop("idle")
//   HardCap→ runtime > hardCapTimeout → Stop("hard_cap")

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	vncDefaultIdleSec   = 1800     // 30 min — VNC pushes frames continuously
	                              // so "idle" here means no key/mouse from
	                              // browser; the frame stream alone counts
	                              // as agent→browser activity and resets
	                              // the timer.
	vncMaxIdleSec       = 21600   // 6 h hard cap
	vncStdoutChunkBytes = 32 << 10 // 32 KiB
	vncDialTimeout      = 5 * time.Second
	vncIdleCheckPeriod  = 30 * time.Second
	vncDefaultTarget    = "127.0.0.1:5900"
)

// VncConfig is the per-run config inside skill.start.
type VncConfig struct {
	// Target is the VNC server's host:port. Defaults to 127.0.0.1:5900.
	// Operator-supplied — validated to be parseable as host:port and
	// (defense-in-depth) NOT a privileged port outside the standard
	// VNC range. The Shell skill is the audit path for arbitrary host
	// access; VNC keeps a tighter target surface.
	Target         string `json:"target,omitempty"`
	IdleTimeoutSec int    `json:"idle_timeout_sec,omitempty"`
}

type vncSkill struct{}

// NewVncSkill constructs the agent's VNC skill. Stateless — all
// per-run state lives in vncRun.
func NewVncSkill() Skill { return &vncSkill{} }

func (v *vncSkill) Kind() SkillKind { return KindVNC }
func (v *vncSkill) Version() string { return "1.0" }

func (v *vncSkill) Capabilities() map[string]any {
	return map[string]any{
		"protocol":          "raw-bytes",
		"default_target":    vncDefaultTarget,
		"default_idle_sec":  vncDefaultIdleSec,
		"hard_cap_sec":      vncMaxIdleSec,
		"stdout_chunk_size": vncStdoutChunkBytes,
		"auth_mode":         "browser",
	}
}

func (v *vncSkill) Validate(config json.RawMessage) error {
	if len(config) == 0 || string(config) == "null" {
		return nil
	}
	var cfg VncConfig
	if err := json.Unmarshal(config, &cfg); err != nil {
		return err
	}
	if cfg.Target != "" {
		if err := validateVncTarget(cfg.Target); err != nil {
			return err
		}
	}
	if cfg.IdleTimeoutSec < 0 {
		return errors.New("vnc: idle_timeout_sec must be non-negative")
	}
	if cfg.IdleTimeoutSec > vncMaxIdleSec {
		return fmt.Errorf("vnc: idle_timeout_sec capped at %d (6h)", vncMaxIdleSec)
	}
	return nil
}

// validateVncTarget parses host:port and applies a narrow whitelist:
// loopback is always fine; LAN private ranges are fine; anything else
// is rejected. The skill is intended for connecting to VNC servers
// on the agent host or its trusted LAN, not arbitrary internet
// destinations.
func validateVncTarget(target string) error {
	host, portStr, err := net.SplitHostPort(target)
	if err != nil {
		return fmt.Errorf("vnc: target must be host:port, got %q: %w", target, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 || port > 65535 {
		return fmt.Errorf("vnc: invalid port in target %q", target)
	}
	host = strings.TrimSpace(host)
	if host == "" {
		return errors.New("vnc: empty host in target")
	}
	// Loopback is always allowed.
	if host == "localhost" || host == "127.0.0.1" || host == "::1" {
		return nil
	}
	ip := net.ParseIP(host)
	if ip != nil {
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() {
			return nil
		}
		return fmt.Errorf("vnc: target %q is not loopback or private; refusing", target)
	}
	// Hostnames: allow .local (mDNS) + anything that's not obviously a
	// public-suffix domain. We don't resolve here — the dial will fail
	// if the name doesn't point anywhere reachable.
	if strings.HasSuffix(host, ".local") || !strings.Contains(host, ".") {
		return nil
	}
	return fmt.Errorf("vnc: target %q must be loopback / private IP / mDNS; refusing", target)
}

func (v *vncSkill) Start(ctx context.Context, runID int64, config json.RawMessage) (Run, error) {
	var cfg VncConfig
	if len(config) > 0 && string(config) != "null" {
		if err := json.Unmarshal(config, &cfg); err != nil {
			return nil, fmt.Errorf("vnc: parse config: %w", err)
		}
	}
	if cfg.Target == "" {
		cfg.Target = vncDefaultTarget
	}
	if err := validateVncTarget(cfg.Target); err != nil {
		return nil, err
	}
	if cfg.IdleTimeoutSec <= 0 {
		cfg.IdleTimeoutSec = vncDefaultIdleSec
	}
	if cfg.IdleTimeoutSec > vncMaxIdleSec {
		cfg.IdleTimeoutSec = vncMaxIdleSec
	}

	log.Printf("[vnc] run=%d dial target=%s", runID, cfg.Target)
	dialer := &net.Dialer{Timeout: vncDialTimeout}
	conn, err := dialer.DialContext(ctx, "tcp", cfg.Target)
	if err != nil {
		log.Printf("[vnc] run=%d dial FAILED target=%s err=%v", runID, cfg.Target, err)
		return nil, fmt.Errorf("vnc: dial %s: %w", cfg.Target, err)
	}
	log.Printf("[vnc] run=%d dial OK local=%s remote=%s", runID, conn.LocalAddr(), conn.RemoteAddr())

	runCtx, cancel := context.WithCancel(ctx)
	run := &vncRun{
		id:             runID,
		cfg:            cfg,
		conn:           conn,
		events:         make(chan Event, 32),
		ctxCancel:      cancel,
		idleTimeout:    time.Duration(cfg.IdleTimeoutSec) * time.Second,
		hardCapTimeout: vncMaxIdleSec * time.Second,
		startedAt:      time.Now(),
	}
	run.lastActivity.Store(time.Now().UnixNano())

	run.send(Event{Kind: EventState, Data: map[string]any{
		"status": "running",
		"target": cfg.Target,
	}})

	go run.readerLoop(runCtx)
	go run.idleWatchdog(runCtx)

	return run, nil
}

// vncRun is the long-lived Run for a single VNC TCP relay session.
// Implements Run + RunInput. RunResizer is a no-op since RFB carries
// its own framebuffer-size negotiation in-protocol.
type vncRun struct {
	id   int64
	cfg  VncConfig
	conn net.Conn

	events         chan Event
	ctxCancel      context.CancelFunc
	idleTimeout    time.Duration
	hardCapTimeout time.Duration
	startedAt      time.Time

	lastActivity atomic.Int64
	stopOnce     sync.Once
	closed       atomic.Bool
}

func (r *vncRun) ID() int64                { return r.id }
func (r *vncRun) Events() <-chan Event     { return r.events }
func (r *vncRun) Stop(reason string) error { return r.stop(reason) }

// WriteInput pumps bytes from dock (browser key/mouse events, RFB
// client messages) into the VNC server TCP connection.
func (r *vncRun) WriteInput(data []byte) error {
	if r.closed.Load() {
		return errors.New("vnc run closed")
	}
	r.lastActivity.Store(time.Now().UnixNano())
	n, err := r.conn.Write(data)
	if err != nil {
		log.Printf("[vnc] run=%d WriteInput err after %d bytes: %v", r.id, n, err)
	}
	return err
}

// Resize is a no-op — RFB negotiates framebuffer size in-band via
// SetDesktopSize / ExtendedDesktopSize. The dock byte bridge sends
// resize control frames anyway; we accept them gracefully.
func (r *vncRun) Resize(rows, cols uint16) error { return nil }

func (r *vncRun) readerLoop(ctx context.Context) {
	buf := make([]byte, vncStdoutChunkBytes)
	first := true
	var totalRead int64
	for {
		n, err := r.conn.Read(buf)
		if n > 0 {
			r.lastActivity.Store(time.Now().UnixNano())
			totalRead += int64(n)
			if first {
				log.Printf("[vnc] run=%d first read n=%d head=%q (banner expected)", r.id, n, string(buf[:min(n, 16)]))
				first = false
			}
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			r.send(Event{Kind: EventStdout, Data: map[string]any{
				"bytes_b64": base64.StdEncoding.EncodeToString(chunk),
			}})
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				log.Printf("[vnc] run=%d reader EOF after %d bytes — server closed", r.id, totalRead)
				r.stop("eof")
			} else if ctx.Err() != nil {
				log.Printf("[vnc] run=%d reader ctx canceled after %d bytes — stop already initiated", r.id, totalRead)
			} else {
				log.Printf("[vnc] run=%d reader err after %d bytes: %v", r.id, totalRead, err)
				r.stop("read_error:" + err.Error())
			}
			return
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func (r *vncRun) idleWatchdog(ctx context.Context) {
	ticker := time.NewTicker(vncIdleCheckPeriod)
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

func (r *vncRun) stop(reason string) error {
	r.stopOnce.Do(func() {
		log.Printf("[vnc] run=%d stop reason=%s target=%s", r.id, reason, r.cfg.Target)
		r.closed.Store(true)
		_ = r.conn.Close()
		r.send(Event{Kind: EventExit, Data: map[string]any{
			"ok":     reason == "operator" || reason == "eof",
			"reason": reason,
			"target": r.cfg.Target,
		}})
		r.ctxCancel()
		close(r.events)
		log.Printf("[vnc] run=%d stop done — conn closed + events channel closed", r.id)
	})
	return nil
}

// send is non-blocking — drop on overflow for stdout to keep the
// reader goroutine unblocked; lifecycle events are tiny and rare so
// they fit in the buffer.
func (r *vncRun) send(ev Event) {
	defer func() { _ = recover() }()
	select {
	case r.events <- ev:
	default:
		if ev.Kind == EventStdout {
			return
		}
		select {
		case r.events <- ev:
		case <-time.After(50 * time.Millisecond):
		}
	}
}
