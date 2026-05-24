package main

// TCP client + envelope reader/writer. Same NDJSON shape as the
// test-serve speaks; this file is the symmetric peer.

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// envelope is a typed-by-kind JSON object; we keep it generic and
// re-parse per kind in the consumer.
type envelope = map[string]any

// client wraps a TCP conn + a goroutine that decodes incoming
// envelopes into a channel. Send is serialized through one mutex so
// concurrent callers don't interleave bytes mid-frame.
type client struct {
	conn net.Conn

	writeMu sync.Mutex
	wbuf    *bufio.Writer

	rbuf  *bufio.Reader
	inbox chan envelope
	closed atomic.Bool
}

func dial(ctx context.Context, addr string) (*client, error) {
	d := &net.Dialer{Timeout: 5 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", addr, err)
	}
	c := &client{
		conn:  conn,
		wbuf:  bufio.NewWriter(conn),
		rbuf:  bufio.NewReader(conn),
		inbox: make(chan envelope, 256),
	}
	go c.readLoop()
	return c, nil
}

func (c *client) Close() {
	c.closed.Store(true)
	_ = c.conn.Close()
}

// send marshals env to JSON + appends \n, writes under the lock so
// no other writer interleaves a half-frame.
func (c *client) send(env envelope) error {
	b, err := json.Marshal(env)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.wbuf.Write(b); err != nil {
		return err
	}
	if err := c.wbuf.WriteByte('\n'); err != nil {
		return err
	}
	return c.wbuf.Flush()
}

func (c *client) readLoop() {
	defer close(c.inbox)
	for {
		line, err := c.rbuf.ReadBytes('\n')
		if err != nil {
			return
		}
		// Trim CR/LF.
		for len(line) > 0 && (line[len(line)-1] == '\n' || line[len(line)-1] == '\r') {
			line = line[:len(line)-1]
		}
		if len(line) == 0 {
			continue
		}
		var env envelope
		if err := json.Unmarshal(line, &env); err != nil {
			continue
		}
		select {
		case c.inbox <- env:
		default:
			// drop on overflow — test mode is bursty, the consumer
			// should drain quickly.
		}
	}
}

// waitFor pulls envelopes until one's "kind" matches one of the wanted
// strings, or timeout. Returns the matched envelope or an error.
func (c *client) waitFor(timeout time.Duration, wanted ...string) (envelope, error) {
	want := map[string]bool{}
	for _, k := range wanted {
		want[k] = true
	}
	deadline := time.After(timeout)
	for {
		select {
		case env, ok := <-c.inbox:
			if !ok {
				return nil, errors.New("connection closed")
			}
			kind, _ := env["kind"].(string)
			if want[kind] {
				return env, nil
			}
			// Drop unmatched — caller's not interested. For richer
			// flows (collecting stdout across many frames), see the
			// collectStdout helper below.
		case <-deadline:
			return nil, fmt.Errorf("timeout waiting for %v", wanted)
		}
	}
}

// collectSkillEvent pulls envelopes until the given runID's skill.event
// arrives with one of the wanted event_kinds. Stdout chunks along the
// way are accumulated and returned. Useful for scenarios that need to
// drive a session AND assert output.
func (c *client) collectSkillEvent(runID int64, timeout time.Duration, wantedKinds ...string) (envelope, []byte, error) {
	want := map[string]bool{}
	for _, k := range wantedKinds {
		want[k] = true
	}
	var stdout []byte
	deadline := time.After(timeout)
	for {
		select {
		case env, ok := <-c.inbox:
			if !ok {
				return nil, stdout, errors.New("connection closed")
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
			if evKind == "stdout" {
				if data, ok := env["data"].(map[string]any); ok {
					if b64, ok := data["bytes_b64"].(string); ok {
						chunk, err := base64.StdEncoding.DecodeString(b64)
						if err == nil {
							stdout = append(stdout, chunk...)
						}
					}
				}
			}
			if want[evKind] {
				return env, stdout, nil
			}
		case <-deadline:
			return nil, stdout, fmt.Errorf("timeout waiting for run=%d event_kind in %v (got %d stdout bytes so far)", runID, wantedKinds, len(stdout))
		}
	}
}

// encodeBytes wraps raw bytes as base64 for skill.stdin envelopes.
func encodeBytes(b []byte) string {
	return base64.StdEncoding.EncodeToString(b)
}

// runCollect aggregates stdout + exit per runID across a single drain
// pass. Useful for scenarios that started ≥2 concurrent runs in one
// connection — the shared inbox can't be drained N times because
// collectSkillEvent's runID filter discards other runs' events.
type runCollect struct {
	Stdout []byte
	Exit   envelope // nil if not yet exited
}

// collectMultipleRuns drains envelopes until every runID in `wantRunIDs`
// has its exit event (or until timeout). Returns a per-run summary.
func (c *client) collectMultipleRuns(wantRunIDs []int64, timeout time.Duration) (map[int64]*runCollect, error) {
	want := map[int64]bool{}
	out := map[int64]*runCollect{}
	for _, id := range wantRunIDs {
		want[id] = true
		out[id] = &runCollect{}
	}
	remaining := len(want)
	deadline := time.After(timeout)
	for remaining > 0 {
		select {
		case env, ok := <-c.inbox:
			if !ok {
				return out, errors.New("connection closed before all runs exited")
			}
			if kind, _ := env["kind"].(string); kind != "skill.event" {
				continue
			}
			runIDf, _ := env["run_id"].(float64)
			runID := int64(runIDf)
			rc, tracked := out[runID]
			if !tracked {
				continue
			}
			evKind, _ := env["event_kind"].(string)
			if evKind == "stdout" {
				if data, ok := env["data"].(map[string]any); ok {
					if b64, ok := data["bytes_b64"].(string); ok {
						chunk, err := base64.StdEncoding.DecodeString(b64)
						if err == nil {
							rc.Stdout = append(rc.Stdout, chunk...)
						}
					}
				}
			}
			if evKind == "exit" && rc.Exit == nil {
				rc.Exit = env
				remaining--
			}
		case <-deadline:
			pending := []int64{}
			for id, rc := range out {
				if rc.Exit == nil {
					pending = append(pending, id)
				}
			}
			return out, fmt.Errorf("timeout — runs without exit: %v", pending)
		}
	}
	return out, nil
}
