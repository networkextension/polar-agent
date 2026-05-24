//go:build unix

package skills

// KDP skill (Host module Phase 5).
//
// Interactive serial console with agent-side intercept for `?`-prefixed
// helper commands. Designed for iOS kernel-debug workflows where the
// box is wired to a USB serial converter exposing /dev/tty.usbserial-*.
//
// Wire-shape: identical to the Shell skill (kind=shell). The dock-side
// byte bridge at /ws/host/:id/shell/:run_id + EventStdout / RunInput /
// RunResizer all work unchanged — KDP just registers under a
// different SkillKind so the UI can label it "Console" instead of
// "Shell" and pre-fill the device-picker form.
//
// Byte flow:
//   serial.Read  → EventStdout chunks (32 KiB cap)
//   skill.stdin  → ? command interpreter OR pass-through to serial.Write
//
//                  Line discipline: WriteInput buffers stdin until we
//                  see a LF (\n). The completed line is checked for a
//                  leading "?": if present, the `?cmd args` is parsed
//                  and dispatched to a builtin handler; the line is
//                  NOT forwarded to the serial port. If not, the full
//                  line (including LF) is written verbatim.
//
//                  This means typing "?help" inside the xterm doesn't
//                  send "?help" to the KDP target — it triggers a
//                  local helper that prints command list back via
//                  EventStdout.
//
// `?` builtins shipped today:
//   ?help              — list commands
//   ?info              — agent-side run info (device, baud, uptime)
//
// Real KDP commands (?break, ?regs, ?bt, etc.) land in a follow-up
// once the user wires a target device and we know the actual KDP
// protocol surface. The dispatcher table makes adding them a 5-line
// change per command.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"go.bug.st/serial"
)

// kdpB64 is the encoding used for stdout payload base64-wrapping.
// Mirrors the convention in shell.go (stdlib base64.StdEncoding).
var kdpB64 = base64.StdEncoding

const (
	kdpDefaultBaud      = 115200
	kdpStdoutChunkBytes = 32 << 10
	kdpMaxStdinLineBytes = 64 << 10 // 64 KiB; bigger lines auto-flush
)

// KDPConfig is the per-run config inside skill.start.
type KDPConfig struct {
	// DevicePath: the serial device on the host. Required.
	// Typical values:
	//   macOS:  /dev/tty.usbserial-XYZ, /dev/cu.usbmodem1101
	//   Linux:  /dev/ttyUSB0, /dev/ttyACM0
	DevicePath string `json:"device_path"`
	// Baud: serial baud rate. Default 115200.
	Baud int `json:"baud,omitempty"`
}

type kdpSkill struct{}

// NewKDPSkill returns the KDP skill on unix. The stub in
// kdp_stub.go returns nil on Windows.
func NewKDPSkill() Skill { return &kdpSkill{} }

func (n *kdpSkill) Kind() SkillKind { return KindKDP }
func (n *kdpSkill) Version() string { return "1.0" }

func (n *kdpSkill) Capabilities() map[string]any {
	return map[string]any{
		"protocol":          "pty-bytes", // same wire shape as Shell
		"default_baud":      kdpDefaultBaud,
		"stdout_chunk_size": kdpStdoutChunkBytes,
		"command_prefix":    "?",
		"builtin_commands":  []string{"?help", "?info"},
	}
}

func (n *kdpSkill) Validate(config json.RawMessage) error {
	if len(config) == 0 || string(config) == "null" {
		return nil
	}
	var cfg KDPConfig
	if err := json.Unmarshal(config, &cfg); err != nil {
		return err
	}
	if cfg.DevicePath == "" {
		return errors.New("kdp: device_path is required")
	}
	if !filepath.IsAbs(cfg.DevicePath) {
		return fmt.Errorf("kdp: device_path must be absolute, got %q", cfg.DevicePath)
	}
	if cfg.Baud < 0 || cfg.Baud > 12_000_000 {
		return fmt.Errorf("kdp: baud out of range: %d", cfg.Baud)
	}
	return nil
}

func (n *kdpSkill) Start(ctx context.Context, runID int64, config json.RawMessage) (Run, error) {
	var cfg KDPConfig
	if len(config) > 0 && string(config) != "null" {
		if err := json.Unmarshal(config, &cfg); err != nil {
			return nil, fmt.Errorf("kdp: parse config: %w", err)
		}
	}
	if cfg.DevicePath == "" {
		return nil, errors.New("kdp: device_path is required")
	}
	if cfg.Baud == 0 {
		cfg.Baud = kdpDefaultBaud
	}

	mode := &serial.Mode{
		BaudRate: cfg.Baud,
		Parity:   serial.NoParity,
		DataBits: 8,
		StopBits: serial.OneStopBit,
	}
	port, err := serial.Open(cfg.DevicePath, mode)
	if err != nil {
		return nil, fmt.Errorf("kdp: open %s @ %d: %w", cfg.DevicePath, cfg.Baud, err)
	}
	// Sane read timeout so the reader goroutine wakes up periodically
	// (lets us notice ctx cancel without blocking forever on a quiet
	// device).
	_ = port.SetReadTimeout(500 * time.Millisecond)

	runCtx, cancel := context.WithCancel(ctx)
	run := &kdpRun{
		id:        runID,
		cfg:       cfg,
		port:      port,
		events:    make(chan Event, 32),
		ctxCancel: cancel,
		startedAt: time.Now(),
		stopCh:    make(chan struct{}),
	}
	run.send(Event{Kind: EventState, Data: map[string]any{
		"status":      "running",
		"device":      cfg.DevicePath,
		"baud":        cfg.Baud,
		"builtin_cmd": []string{"?help", "?info"},
	}})
	run.send(Event{Kind: EventStdout, Data: map[string]any{
		"bytes_b64": kdpEncode("[polar-kdp] connected to " + cfg.DevicePath +
			" @ " + fmt.Sprintf("%d", cfg.Baud) + " baud — type ?help for commands\r\n"),
	}})

	go run.readerLoop(runCtx)
	return run, nil
}

// kdpRun is the long-lived Run for a single serial session.
type kdpRun struct {
	id   int64
	cfg  KDPConfig
	port serial.Port

	events    chan Event
	ctxCancel context.CancelFunc
	startedAt time.Time
	stopCh    chan struct{}

	// stdinBuf accumulates bytes from WriteInput until we see a LF.
	// Then the completed line is checked for `?` and dispatched. The
	// buffer is owned solely by WriteInput callers — we serialize
	// with the mutex below since the loop could be called concurrently
	// from multiple skill.stdin envelopes.
	stdinMu  sync.Mutex
	stdinBuf bytes.Buffer

	stopOnce sync.Once
	closed   atomic.Bool
}

func (r *kdpRun) ID() int64                 { return r.id }
func (r *kdpRun) Events() <-chan Event      { return r.events }
func (r *kdpRun) Stop(reason string) error  { return r.stop(reason) }

// Resize is a no-op for serial — there's no TTY size on the wire.
// We implement the interface so the dock byte bridge can call it
// without type-asserting.
func (r *kdpRun) Resize(rows, cols uint16) error { return nil }

// WriteInput receives stdin bytes from the operator's xterm. We
// accumulate until a LF, then check for `?` prefix → dispatch to
// builtin command, OR pass through to the serial port verbatim.
func (r *kdpRun) WriteInput(data []byte) error {
	if r.closed.Load() {
		return errors.New("kdp run closed")
	}
	r.stdinMu.Lock()
	defer r.stdinMu.Unlock()

	if _, err := r.stdinBuf.Write(data); err != nil {
		return err
	}
	// Auto-flush oversized line (operator pasted a 64KiB blob — pass
	// through without trying to parse).
	if r.stdinBuf.Len() > kdpMaxStdinLineBytes {
		return r.flushPassthroughLocked(false)
	}
	// Drain complete lines.
	for {
		buf := r.stdinBuf.Bytes()
		idx := bytes.IndexByte(buf, '\n')
		if idx < 0 {
			break
		}
		// Consume line including LF.
		line := make([]byte, idx+1)
		copy(line, buf[:idx+1])
		r.stdinBuf.Next(idx + 1)
		r.dispatchLine(line)
	}
	return nil
}

func (r *kdpRun) flushPassthroughLocked(includeEOL bool) error {
	buf := r.stdinBuf.Bytes()
	if includeEOL && (len(buf) == 0 || buf[len(buf)-1] != '\n') {
		buf = append(buf, '\n')
	}
	if len(buf) == 0 {
		return nil
	}
	_, err := r.port.Write(buf)
	r.stdinBuf.Reset()
	return err
}

func (r *kdpRun) dispatchLine(line []byte) {
	// Echo to xterm for local readability (serial may not echo).
	r.send(Event{Kind: EventStdout, Data: map[string]any{
		"bytes_b64": kdpEncode(string(line)),
	}})
	// `?` prefix → builtin command. Allow optional leading whitespace.
	trimmed := bytes.TrimLeft(line, " \t")
	if len(trimmed) > 0 && trimmed[0] == '?' {
		r.runBuiltin(string(bytes.TrimRight(trimmed, "\r\n")))
		return
	}
	// Pass-through to serial.
	if _, err := r.port.Write(line); err != nil {
		r.send(Event{Kind: EventStdout, Data: map[string]any{
			"bytes_b64": kdpEncode("[polar-kdp] write error: " + err.Error() + "\r\n"),
		}})
	}
}

// runBuiltin dispatches a `?command [args]` line. Output is written
// back to the xterm as if it came from the serial device.
func (r *kdpRun) runBuiltin(line string) {
	// Strip leading `?` + split on first whitespace.
	body := line
	if len(body) > 0 && body[0] == '?' {
		body = body[1:]
	}
	parts := bufio.NewScanner(bytes.NewReader([]byte(body)))
	parts.Split(bufio.ScanWords)
	var cmd string
	if parts.Scan() {
		cmd = parts.Text()
	}
	var msg string
	switch cmd {
	case "help", "h", "?":
		msg = "[polar-kdp] builtin commands:\r\n" +
			"  ?help       — show this list\r\n" +
			"  ?info       — show device + uptime\r\n"
	case "info":
		uptime := time.Since(r.startedAt).Round(time.Second)
		msg = fmt.Sprintf("[polar-kdp] device=%s baud=%d uptime=%s\r\n",
			r.cfg.DevicePath, r.cfg.Baud, uptime)
	case "":
		msg = "[polar-kdp] empty command after ?, try ?help\r\n"
	default:
		msg = fmt.Sprintf("[polar-kdp] unknown command: ?%s (try ?help)\r\n", cmd)
	}
	r.send(Event{Kind: EventStdout, Data: map[string]any{
		"bytes_b64": kdpEncode(msg),
	}})
}

func (r *kdpRun) readerLoop(ctx context.Context) {
	buf := make([]byte, kdpStdoutChunkBytes)
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.stopCh:
			return
		default:
		}
		n, err := r.port.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			r.send(Event{Kind: EventStdout, Data: map[string]any{
				"bytes_b64": kdpEncode(string(chunk)),
			}})
		}
		if err != nil {
			// SetReadTimeout makes a quiet device return (0, nil) on
			// timeout; an actual error means the device went away.
			if ctx.Err() == nil && !r.closed.Load() {
				r.send(Event{Kind: EventStdout, Data: map[string]any{
					"bytes_b64": kdpEncode("[polar-kdp] read error: " + err.Error() + "\r\n"),
				}})
				r.stop("read_error:" + err.Error())
			}
			return
		}
	}
}

func (r *kdpRun) stop(reason string) error {
	r.stopOnce.Do(func() {
		r.closed.Store(true)
		_ = r.port.Close()
		r.send(Event{Kind: EventExit, Data: map[string]any{
			"ok":     reason == "operator" || reason == "browser_closed",
			"reason": reason,
			"device": r.cfg.DevicePath,
		}})
		r.ctxCancel()
		close(r.stopCh)
		close(r.events)
	})
	return nil
}

func (r *kdpRun) send(ev Event) {
	defer func() { _ = recover() }()
	select {
	case r.events <- ev:
	default:
		// Drop on overflow for stdout; lifecycle events block briefly.
		if ev.Kind == EventStdout {
			return
		}
		select {
		case r.events <- ev:
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// kdpEncode is a tiny helper — keeps the base64 import scoped to
// this file rather than threading it through every Event call site.
func kdpEncode(s string) string {
	return kdpB64.EncodeToString([]byte(s))
}
