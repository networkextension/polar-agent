//go:build unix

package skills

// Tests for the VNC skill (B.1 plain TCP relay path).
// Unix-only — the skill itself is //go:build unix.
//
// Strategy: spin up an in-memory net.Listener that echoes bytes back
// (the simplest possible "VNC server" — we're not testing RFB, only
// the relay plumbing). Tests cover:
//   - Kind/Version/Capabilities surface
//   - Validate target whitelist (loopback, private, mDNS allowed;
//     public destinations rejected)
//   - Start dials successfully, bytes round-trip both directions
//   - Stop is idempotent and emits an exit event

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"testing"
	"time"
)

func TestVncSkillKindAndCapabilities(t *testing.T) {
	sk := NewVncSkill()
	if sk == nil {
		t.Skip("NewVncSkill returned nil (non-unix build?)")
	}
	if sk.Kind() != KindVNC {
		t.Errorf("Kind: got %q, want %q", sk.Kind(), KindVNC)
	}
	if sk.Version() == "" {
		t.Error("Version: empty")
	}
	caps := sk.Capabilities()
	if caps["protocol"] != "raw-bytes" {
		t.Errorf("Capabilities.protocol: got %v, want raw-bytes", caps["protocol"])
	}
	if caps["auth_mode"] != "browser" {
		t.Errorf("Capabilities.auth_mode: got %v, want browser", caps["auth_mode"])
	}
}

func TestVncSkillValidate(t *testing.T) {
	sk := NewVncSkill()
	if sk == nil {
		t.Skip("non-unix")
	}
	// Empty / null configs are valid (defaults applied at Start).
	if err := sk.Validate(nil); err != nil {
		t.Errorf("empty config: %v", err)
	}
	if err := sk.Validate(json.RawMessage("null")); err != nil {
		t.Errorf("null: %v", err)
	}
	// Loopback variants allowed.
	for _, target := range []string{"127.0.0.1:5900", "localhost:5900", "[::1]:5900"} {
		cfg := json.RawMessage(fmt.Sprintf(`{"target":%q}`, target))
		if err := sk.Validate(cfg); err != nil {
			t.Errorf("loopback %q: %v", target, err)
		}
	}
	// Private LAN allowed.
	for _, target := range []string{"192.168.1.10:5900", "10.0.0.5:5901", "172.16.0.1:5900"} {
		cfg := json.RawMessage(fmt.Sprintf(`{"target":%q}`, target))
		if err := sk.Validate(cfg); err != nil {
			t.Errorf("private %q: %v", target, err)
		}
	}
	// mDNS .local allowed.
	if err := sk.Validate(json.RawMessage(`{"target":"some-mac.local:5900"}`)); err != nil {
		t.Errorf(".local: %v", err)
	}
	// Public IP rejected.
	if err := sk.Validate(json.RawMessage(`{"target":"8.8.8.8:5900"}`)); err == nil {
		t.Error("public IP: want error")
	}
	// Public hostname rejected.
	if err := sk.Validate(json.RawMessage(`{"target":"vnc.example.com:5900"}`)); err == nil {
		t.Error("public hostname: want error")
	}
	// Malformed target rejected.
	if err := sk.Validate(json.RawMessage(`{"target":"not-a-host-port"}`)); err == nil {
		t.Error("malformed: want error")
	}
	// Negative idle timeout rejected.
	if err := sk.Validate(json.RawMessage(`{"idle_timeout_sec":-1}`)); err == nil {
		t.Error("negative idle: want error")
	}
	// Excessive idle timeout rejected.
	if err := sk.Validate(json.RawMessage(`{"idle_timeout_sec":99999}`)); err == nil {
		t.Error("oversize idle: want error")
	}
}

// startEchoServer launches an in-memory TCP server that echoes each
// inbound byte back to the client. Returns "127.0.0.1:port" + a stop
// func.
func startEchoServer(t *testing.T) (string, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	done := make(chan struct{})
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}(conn)
		}
	}()
	return ln.Addr().String(), func() {
		_ = ln.Close()
		close(done)
	}
}

func TestVncSkillStartRoundtrip(t *testing.T) {
	sk := NewVncSkill()
	if sk == nil {
		t.Skip("non-unix")
	}
	target, stopServer := startEchoServer(t)
	defer stopServer()

	cfg := json.RawMessage(fmt.Sprintf(`{"target":%q,"idle_timeout_sec":60}`, target))
	run, err := sk.Start(context.Background(), 1, cfg)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer run.Stop("test_cleanup")

	// First event should be the running state.
	select {
	case ev := <-run.Events():
		if ev.Kind != EventState {
			t.Errorf("first event: kind %q, want state", ev.Kind)
		}
		if ev.Data["status"] != "running" {
			t.Errorf("first event status: %v, want running", ev.Data["status"])
		}
		if ev.Data["target"] != target {
			t.Errorf("first event target: %v, want %s", ev.Data["target"], target)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for state event")
	}

	// Send a payload; expect it back via EventStdout (echo server).
	input, ok := run.(RunInput)
	if !ok {
		t.Fatal("Run doesn't satisfy RunInput")
	}
	payload := []byte("RFB 003.008\n")
	if err := input.WriteInput(payload); err != nil {
		t.Fatalf("WriteInput: %v", err)
	}

	// Drain events until we see the echoed bytes.
	deadline := time.After(2 * time.Second)
	var got []byte
	for len(got) < len(payload) {
		select {
		case ev := <-run.Events():
			if ev.Kind != EventStdout {
				continue
			}
			b64, _ := ev.Data["bytes_b64"].(string)
			chunk, err := base64.StdEncoding.DecodeString(b64)
			if err != nil {
				t.Fatalf("decode bytes_b64: %v", err)
			}
			got = append(got, chunk...)
		case <-deadline:
			t.Fatalf("timed out waiting for echo; got %q", got)
		}
	}
	if string(got[:len(payload)]) != string(payload) {
		t.Errorf("echo: got %q, want %q", got[:len(payload)], payload)
	}
}

func TestVncSkillStopIdempotent(t *testing.T) {
	sk := NewVncSkill()
	if sk == nil {
		t.Skip("non-unix")
	}
	target, stopServer := startEchoServer(t)
	defer stopServer()

	cfg := json.RawMessage(fmt.Sprintf(`{"target":%q}`, target))
	run, err := sk.Start(context.Background(), 2, cfg)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Drain the initial state event so the channel doesn't fill.
	<-run.Events()

	// Two Stops shouldn't panic / hang / double-emit exit.
	if err := run.Stop("operator"); err != nil {
		t.Errorf("first Stop: %v", err)
	}
	if err := run.Stop("operator"); err != nil {
		t.Errorf("second Stop: %v", err)
	}

	// Expect at most one exit event before the channel closes.
	exits := 0
	timeout := time.After(2 * time.Second)
	for {
		select {
		case ev, ok := <-run.Events():
			if !ok {
				if exits != 1 {
					t.Errorf("exit events: got %d, want 1", exits)
				}
				return
			}
			if ev.Kind == EventExit {
				exits++
			}
		case <-timeout:
			t.Fatal("events channel never closed after Stop")
		}
	}
}

func TestVncSkillStartRejectsPublicTarget(t *testing.T) {
	sk := NewVncSkill()
	if sk == nil {
		t.Skip("non-unix")
	}
	cfg := json.RawMessage(`{"target":"8.8.8.8:5900"}`)
	if _, err := sk.Start(context.Background(), 3, cfg); err == nil {
		t.Error("public target: want error")
	}
}

func TestVncSkillStartDialFail(t *testing.T) {
	sk := NewVncSkill()
	if sk == nil {
		t.Skip("non-unix")
	}
	// Pick a likely-unused localhost port. Connect refuses fast.
	cfg := json.RawMessage(`{"target":"127.0.0.1:1"}`)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := sk.Start(ctx, 4, cfg); err == nil {
		t.Error("dial to unused port: want error")
	}
}
