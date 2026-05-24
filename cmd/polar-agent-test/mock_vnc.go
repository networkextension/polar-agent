package main

// Minimal in-memory RFB-shaped server for VNC scenarios. NOT a real
// VNC server — speaks just enough wire shape for our byte-relay
// scenarios to assert on:
//
//   - On accept, immediately writes "RFB 003.008\n" (12 bytes) — the
//     standard RFB version banner that any compliant client (or our
//     pass-through agent) sees first.
//   - After the banner, echoes every byte the client sends back to it.
//   - Optionally closes the conn after N bytes received (for testing
//     unexpected-server-close paths).
//
// Listens on a free loopback port (port 0 → kernel-assigned), so
// concurrent scenarios don't fight for a fixed port. Per accepted
// connection one goroutine runs the echo loop.

import (
	"errors"
	"io"
	"net"
	"sync/atomic"
	"time"
)

const mockVNCBanner = "RFB 003.008\n"

type mockVNCServer struct {
	ln         net.Listener
	Addr       string         // "127.0.0.1:<port>"
	closed     atomic.Bool
	conns      atomic.Int64   // count of accepted (lifetime)
	bytesEcho  atomic.Int64   // total bytes echoed across all conns
	closeAfter int            // 0 = never; N = close after receiving N bytes
}

func startMockVNCServer() (*mockVNCServer, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	m := &mockVNCServer{
		ln:   ln,
		Addr: ln.Addr().String(),
	}
	go m.acceptLoop()
	return m, nil
}

// WithCloseAfter returns a new server configured to close each conn
// after receiving N bytes from the client. Use it to test what
// happens when macOS Screen Sharing hangs up mid-handshake (e.g.
// invalid security response).
func (m *mockVNCServer) WithCloseAfter(n int) *mockVNCServer {
	m.closeAfter = n
	return m
}

func (m *mockVNCServer) Close() {
	if m.closed.Swap(true) {
		return
	}
	_ = m.ln.Close()
}

func (m *mockVNCServer) acceptLoop() {
	for {
		conn, err := m.ln.Accept()
		if err != nil {
			if m.closed.Load() || errors.Is(err, net.ErrClosed) {
				return
			}
			time.Sleep(50 * time.Millisecond)
			continue
		}
		m.conns.Add(1)
		go m.handleConn(conn)
	}
}

func (m *mockVNCServer) handleConn(conn net.Conn) {
	defer conn.Close()
	// Send the banner first — the agent's readerLoop should read this
	// and emit it as EventStdout.
	if _, err := conn.Write([]byte(mockVNCBanner)); err != nil {
		return
	}
	// Echo loop until client closes or we hit closeAfter.
	buf := make([]byte, 4096)
	var received int
	for {
		_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))
		n, err := conn.Read(buf)
		if n > 0 {
			received += n
			if _, werr := conn.Write(buf[:n]); werr != nil {
				return
			}
			m.bytesEcho.Add(int64(n))
			if m.closeAfter > 0 && received >= m.closeAfter {
				return
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return
			}
			return
		}
	}
}
