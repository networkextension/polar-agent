//go:build darwin || freebsd || linux

package main

// fw_collector 单测:三种行解析(真实样例)、flusher 批量/上限、base_url 门控。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestParsePFStates(t *testing.T) {
	// zen 真机 pfctl -ss 样例(含 NAT 三段与 ALL iface)。
	out := `ALL udp 10.88.0.5:51822 -> 192.168.11.57:39201 -> 192.168.11.197:1639       MULTIPLE:MULTIPLE
en0 tcp 192.168.11.65:22 <- 192.168.11.79:53122       ESTABLISHED:ESTABLISHED
ALL udp 192.168.11.57:1632 <- 58.37.118.81:1079       MULTIPLE:MULTIPLE
garbage`
	conns := parsePFStates(out, 500)
	if len(conns) != 3 {
		t.Fatalf("want 3 conns, got %d: %+v", len(conns), conns)
	}
	c0 := conns[0]
	if c0.Proto != "udp" || c0.Src != "10.88.0.5:51822" || c0.Dst != "192.168.11.197:1639" ||
		c0.State != "MULTIPLE:MULTIPLE" || c0.Iface != "" {
		t.Fatalf("NAT 3-hop parse wrong: %+v", c0)
	}
	if conns[1].Iface != "en0" || conns[1].State != "ESTABLISHED:ESTABLISHED" {
		t.Fatalf("iface/state wrong: %+v", conns[1])
	}
	if got := parsePFStates(out, 2); len(got) != 2 {
		t.Fatalf("cap not applied: %d", len(got))
	}
}

func TestParseConntrack(t *testing.T) {
	out := `tcp      6 431999 ESTABLISHED src=10.0.0.2 dst=1.2.3.4 sport=51000 dport=443 src=1.2.3.4 dst=10.0.0.2 sport=443 dport=51000 [ASSURED] mark=0 use=1
udp      17 29 src=10.0.0.2 dst=8.8.8.8 sport=40000 dport=53 src=8.8.8.8 dst=10.0.0.2 sport=53 dport=40000 mark=0 use=1`
	conns := parseConntrack(out, 500)
	if len(conns) != 2 {
		t.Fatalf("want 2, got %d: %+v", len(conns), conns)
	}
	if conns[0].Proto != "tcp" || conns[0].Src != "10.0.0.2:51000" || conns[0].Dst != "1.2.3.4:443" ||
		conns[0].State != "ESTABLISHED" {
		t.Fatalf("tcp parse wrong: %+v", conns[0])
	}
	if conns[1].Src != "10.0.0.2:40000" {
		t.Fatalf("udp orig-direction wrong: %+v", conns[1])
	}
}

func TestParsePFLogLine(t *testing.T) {
	row, ok := parsePFLogLine(`00:00:00.000000 rule 2/0(match): block in on en0: 10.0.0.22.53122 > 1.2.3.4.22: Flags [S], seq 1, win 65535, length 0`)
	if !ok {
		t.Fatal("tcp line must parse")
	}
	if row.Action != "drop" || row.Direction != "in" || row.Iface != "en0" ||
		row.Proto != "tcp" || row.Src != "10.0.0.22" || row.SrcPort != 53122 ||
		row.Dst != "1.2.3.4" || row.DstPort != 22 || row.RuleRef != "rule 2" {
		t.Fatalf("tcp parse wrong: %+v", row)
	}
	row, ok = parsePFLogLine(`00:00:01.5 rule 0/0(match): pass out on en1: 10.0.0.5.5353 > 224.0.0.251.5353: UDP, length 45`)
	if !ok || row.Action != "allow" || row.Proto != "udp" || row.DstPort != 5353 {
		t.Fatalf("udp parse wrong: ok=%v %+v", ok, row)
	}
	row, ok = parsePFLogLine(`00:00:02.0 rule 1/0(match): block in on en0: 9.9.9.9 > 10.0.0.5: ICMP echo request, id 1, seq 1, length 64`)
	if !ok || row.Proto != "icmp" || row.SrcPort != 0 || row.Src != "9.9.9.9" {
		t.Fatalf("icmp parse wrong: ok=%v %+v", ok, row)
	}
	if _, ok := parsePFLogLine("tcpdump: listening on pflog0"); ok {
		t.Fatal("noise line must not parse")
	}
}

func TestParseNFTLogLine(t *testing.T) {
	row, ok := parseNFTLogLine(`[12345.678] polarfw:input:ssh: IN=eth0 OUT= MAC=aa:bb SRC=1.2.3.4 DST=10.0.0.2 LEN=60 PROTO=TCP SPT=51000 DPT=22 WINDOW=64240`)
	if !ok {
		t.Fatal("nft line must parse")
	}
	if row.RuleRef != "input:ssh" || row.Iface != "eth0" || row.Direction != "in" ||
		row.Proto != "tcp" || row.Src != "1.2.3.4" || row.DstPort != 22 {
		t.Fatalf("nft parse wrong: %+v", row)
	}
	if _, ok := parseNFTLogLine("random kernel line without prefix"); ok {
		t.Fatal("non-polarfw line must not parse")
	}
}

func TestSplitHostPortDot(t *testing.T) {
	cases := []struct {
		in   string
		host string
		port int
	}{
		{"1.2.3.4.443", "1.2.3.4", 443},
		{"fe80::1.5353", "fe80::1", 5353},
		{"9.9.9.9", "9.9.9.9", 0}, // 裸 v4(ICMP)绝不拆
		{"224.0.0.251.5353", "224.0.0.251", 5353},
	}
	for _, tc := range cases {
		h, p := splitHostPortDot(tc.in)
		if h != tc.host || p != tc.port {
			t.Errorf("split(%q) = (%q,%d), want (%q,%d)", tc.in, h, p, tc.host, tc.port)
		}
	}
}

func newTestCollector(t *testing.T, base string) *fwCollector {
	t.Helper()
	dir := t.TempDir()
	c := &fwCollector{
		cfg:      AgentConfig{AgentID: "ag_test", Token: "tok"},
		stateDir: dir,
		goos:     "darwin",
		client:   &http.Client{Timeout: 2 * time.Second},
		events:   make(chan fwEventRow, fwEventBufferCap),
		warned:   map[string]bool{},
	}
	if base != "" {
		if err := os.WriteFile(filepath.Join(dir, "current.conf"), []byte("x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "base_url"), []byte(base+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return c
}

func TestBaseURLGating(t *testing.T) {
	c := newTestCollector(t, "")
	if got := c.baseURL(); got != "" {
		t.Fatalf("unmanaged must be empty, got %q", got)
	}
	c2 := newTestCollector(t, "https://x/api/firewall/")
	if got := c2.baseURL(); got != "https://x/api/firewall" {
		t.Fatalf("base = %q", got)
	}
}

func TestFlushLoopBatches(t *testing.T) {
	var batches int32
	var lastN int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/agent/events" {
			http.NotFound(w, r)
			return
		}
		var body struct {
			AgentID string       `json:"agent_id"`
			Events  []fwEventRow `json:"events"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.AgentID != "ag_test" {
			t.Errorf("agent_id = %q", body.AgentID)
		}
		if len(body.Events) > fwEventBatchMax {
			t.Errorf("batch too big: %d", len(body.Events))
		}
		atomic.AddInt32(&batches, 1)
		atomic.StoreInt32(&lastN, int32(len(body.Events)))
		w.WriteHeader(200)
	}))
	defer srv.Close()

	c := newTestCollector(t, srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	go c.flushLoop(ctx)
	// 150 条 → 立即冲一批 100,余 50 由 5s ticker 或 ctx 结束时冲。
	for i := 0; i < 150; i++ {
		c.events <- fwEventRow{TS: time.Now(), Src: "1.2.3.4"}
	}
	deadline := time.Now().Add(3 * time.Second)
	for atomic.LoadInt32(&batches) < 1 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if atomic.LoadInt32(&batches) < 1 {
		t.Fatal("no batch flushed on reaching 100")
	}
	cancel() // ctx 结束冲残余
	deadline = time.Now().Add(2 * time.Second)
	for atomic.LoadInt32(&batches) < 2 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if atomic.LoadInt32(&batches) < 2 {
		t.Fatalf("remainder not flushed, batches=%d", batches)
	}
}

func TestStateReportPayload(t *testing.T) {
	got := make(chan map[string]any, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/agent/state" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("bad auth %q", r.Header.Get("Authorization"))
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		select {
		case got <- body:
		default:
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()

	c := newTestCollector(t, srv.URL)
	c.run = func(_ context.Context, argv ...string) (string, error) {
		return "en0 tcp 1.2.3.4:1 <- 5.6.7.8:2       ESTABLISHED:ESTABLISHED", nil
	}
	if err := os.WriteFile(filepath.Join(c.stateDir, "current.hash"), []byte("sha256:abc\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c.reportState(context.Background())
	select {
	case body := <-got:
		if body["agent_id"] != "ag_test" || body["running_hash"] != "sha256:abc" {
			t.Fatalf("payload wrong: %v", body)
		}
		if conns, _ := body["connections"].([]any); len(conns) != 1 {
			t.Fatalf("connections wrong: %v", body["connections"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("state not posted")
	}
}

// 每机单例:同一 stateDir 第二个 collector 抢不到锁。
func TestCollectorLockSingleton(t *testing.T) {
	a := newTestCollector(t, "")
	b := &fwCollector{stateDir: a.stateDir, warned: map[string]bool{}}
	if !a.acquireLock() {
		t.Fatal("first must acquire")
	}
	if b.acquireLock() {
		t.Fatal("second must be refused")
	}
}
