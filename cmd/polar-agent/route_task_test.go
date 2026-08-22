//go:build darwin || freebsd || linux || openbsd

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/networkextension/polar-agent/cmd/polar-agent/routecmd"
)

// fakeKernel records executed argv and answers route listings from a
// mutable table so the three-way diff + undo paths are exercised end to end.
type fakeKernel struct {
	mu    sync.Mutex
	table []routecmd.Route
	cmds  []string
	fail  string // substring of an argv that should fail
}

func (k *fakeKernel) run(ctx context.Context, argv ...string) (string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	line := strings.Join(argv, " ")
	k.cmds = append(k.cmds, line)
	if k.fail != "" && strings.Contains(line, k.fail) {
		return "", fmt.Errorf("boom: %s", line)
	}
	// argv shape: route -n add|change|delete -inet DST [gw]  (darwin renderer)
	action, dst := argv[2], argv[4]
	if dst == "default" {
		dst = routecmd.DefaultV4
	}
	switch action {
	case "add", "change":
		r := routecmd.Route{Family: 4, Dst: dst, Kind: routecmd.KindVia}
		if len(argv) > 5 {
			r.Gateway = argv[5]
		}
		k.table = append(k.table, r)
	case "delete":
		var keep []routecmd.Route
		for _, r := range k.table {
			if r.Dst != dst {
				keep = append(keep, r)
			}
		}
		k.table = keep
	}
	return "", nil
}

func (k *fakeKernel) netstat() string {
	k.mu.Lock()
	defer k.mu.Unlock()
	var b strings.Builder
	b.WriteString("Routing tables\n\nInternet:\nDestination Gateway Flags Netif Expire\n")
	for _, r := range k.table {
		dst := r.Dst
		if r.IsDefault() {
			dst = "default"
		}
		fmt.Fprintf(&b, "%s %s UGSc en0\n", dst, r.Gateway)
	}
	return b.String()
}

type confirmScript struct {
	commit   bool
	status   int
	hits     int
	mu       sync.Mutex
	statuses []string
}

func rtConfirmServer(t *testing.T, cs *confirmScript) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/agent/transactions/", func(w http.ResponseWriter, r *http.Request) {
		cs.mu.Lock()
		defer cs.mu.Unlock()
		if strings.HasSuffix(r.URL.Path, "/status") {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			cs.statuses = append(cs.statuses, fmt.Sprint(body["status"]))
			w.WriteHeader(200)
			return
		}
		cs.hits++
		if cs.status != 0 {
			w.WriteHeader(cs.status)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"commit": cs.commit, "status": "x"})
	})
	return httptest.NewServer(mux)
}

func newRouteTestExec(t *testing.T, k *fakeKernel) *routeExec {
	t.Helper()
	dir := t.TempDir()
	e := &routeExec{stateDir: dir, goos: "darwin", run: k.run, client: &http.Client{Timeout: 2 * time.Second}}
	return e
}

// kernelTable shells to netstat; in tests we override via a package-level hook.
func withFakeTable(e *routeExec, k *fakeKernel) {
	kernelTableHook = func() ([]routecmd.Route, error) {
		return routecmd.ParseTable("darwin", 4, k.netstat()), nil
	}
}

func via(t *testing.T, dst, gw string) routecmd.Route {
	t.Helper()
	r, err := routecmd.Normalize(routecmd.Route{Dst: dst, Kind: "via", Gateway: gw})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func input(base string, routes []routecmd.Route) routeTaskInput {
	return routeTaskInput{TxnID: 7, RTBaseURL: base, Mode: "apply", Routes: routes,
		CompiledHash: routecmd.Hash(routes), RollbackTimeoutSec: 1, ConfirmToken: "tok"}
}

func TestRouteTaskInputShape(t *testing.T) {
	b, _ := json.Marshal(routeTaskInput{})
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	want := "allow_default,compiled_hash,confirm_token,mode,rollback_timeout_sec,routes,rt_base_url,txn_id"
	if got := strings.Join(keys, ","); got != want {
		t.Fatalf("keys %s, want %s", got, want)
	}
}

func TestRouteApplyCommit(t *testing.T) {
	k := &fakeKernel{table: []routecmd.Route{via(t, "0.0.0.0/0", "192.168.11.1"), via(t, "10.30.0.0/16", "192.168.11.9")}}
	cs := &confirmScript{commit: true}
	srv := rtConfirmServer(t, cs)
	defer srv.Close()
	e := newRouteTestExec(t, k)
	withFakeTable(e, k)
	defer func() { kernelTableHook = nil }()
	// previously managed 10.30/16; now desired 10.10.10/24 only → add + delete
	_ = e.writeSet("current.json", []routecmd.Route{via(t, "10.30.0.0/16", "192.168.11.9")})

	desired := []routecmd.Route{via(t, "10.10.10.0/24", "192.168.11.64")}
	out, err := routeApply(e, input(srv.URL, desired))
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if out.(map[string]any)["committed"] != true {
		t.Fatalf("not committed: %v", out)
	}
	joined := strings.Join(k.cmds, "\n")
	if !strings.Contains(joined, "route -n add -inet 10.10.10.0/24 192.168.11.64") ||
		!strings.Contains(joined, "route -n delete -inet 10.30.0.0/16") {
		t.Fatalf("unexpected cmds:\n%s", joined)
	}
	if strings.Contains(joined, "default") {
		t.Fatalf("unmanaged default route must not be touched:\n%s", joined)
	}
	cur := e.readSet("current.json")
	if len(cur) != 1 || cur[0].Dst != "10.10.10.0/24" {
		t.Fatalf("current.json not promoted: %+v", cur)
	}
	if h, _ := os.ReadFile(e.path("current.hash")); string(h) != routecmd.Hash(desired) {
		t.Fatal("current.hash not written")
	}
	if len(cs.statuses) < 2 || cs.statuses[0] != "applying" || cs.statuses[1] != "awaiting_confirm" {
		t.Fatalf("status heartbeats: %v", cs.statuses)
	}
}

func TestRouteApplyRefusedRollsBack(t *testing.T) {
	k := &fakeKernel{table: []routecmd.Route{via(t, "10.20.0.0/16", "192.168.11.77")}}
	cs := &confirmScript{commit: false}
	srv := rtConfirmServer(t, cs)
	defer srv.Close()
	e := newRouteTestExec(t, k)
	withFakeTable(e, k)
	defer func() { kernelTableHook = nil }()
	_ = e.writeSet("current.json", []routecmd.Route{via(t, "10.20.0.0/16", "192.168.11.77")})

	desired := []routecmd.Route{via(t, "10.20.0.0/16", "192.168.11.9"), via(t, "10.10.10.0/24", "192.168.11.64")}
	out, err := routeApply(e, input(srv.URL, desired))
	if err == nil || out.(map[string]any)["rolled_back"] != true {
		t.Fatalf("expected rolled_back error, got %v / %v", out, err)
	}
	joined := strings.Join(k.cmds, "\n")
	// forward: change 10.20 → .9, add 10.10.10 ; undo: delete 10.10.10, change 10.20 → .77
	if !strings.Contains(joined, "route -n change -inet 10.20.0.0/16 192.168.11.9") ||
		!strings.Contains(joined, "route -n delete -inet 10.10.10.0/24") ||
		!strings.Contains(joined, "route -n change -inet 10.20.0.0/16 192.168.11.77") {
		t.Fatalf("undo sequence wrong:\n%s", joined)
	}
	if cur := e.readSet("current.json"); len(cur) != 1 || cur[0].Gateway != "192.168.11.77" {
		t.Fatalf("current.json must be untouched after rollback: %+v", cur)
	}
}

func TestRouteApplyUnreachableRollsBack(t *testing.T) {
	k := &fakeKernel{}
	e := newRouteTestExec(t, k)
	withFakeTable(e, k)
	defer func() { kernelTableHook = nil }()
	desired := []routecmd.Route{via(t, "10.10.10.0/24", "192.168.11.64")}
	out, err := routeApply(e, input("http://127.0.0.1:1", desired)) // nothing listens
	if err == nil || out.(map[string]any)["rolled_back"] != true {
		t.Fatalf("expected rolled_back, got %v / %v", out, err)
	}
	if !strings.Contains(strings.Join(k.cmds, "\n"), "route -n delete -inet 10.10.10.0/24") {
		t.Fatalf("added route must be deleted on unreachable controller: %v", k.cmds)
	}
}

func TestRouteApplyPartialFailureUndoes(t *testing.T) {
	k := &fakeKernel{fail: "10.99.0.0/16"}
	cs := &confirmScript{commit: true}
	srv := rtConfirmServer(t, cs)
	defer srv.Close()
	e := newRouteTestExec(t, k)
	withFakeTable(e, k)
	defer func() { kernelTableHook = nil }()
	desired := []routecmd.Route{via(t, "10.10.10.0/24", "192.168.11.64"), via(t, "10.99.0.0/16", "192.168.11.64")}
	_, err := routeApply(e, input(srv.URL, desired))
	if err == nil {
		t.Fatal("expected failure")
	}
	if cs.hits != 0 {
		t.Fatal("must not confirm after a failed op")
	}
	if !strings.Contains(strings.Join(k.cmds, "\n"), "route -n delete -inet 10.10.10.0/24") {
		t.Fatalf("successful op must be undone: %v", k.cmds)
	}
}

func TestRouteApplyGuards(t *testing.T) {
	e := newRouteTestExec(t, &fakeKernel{})
	def := via(t, "default", "192.168.11.1")
	in := input("http://x", []routecmd.Route{def})
	if _, err := routeApply(e, in); err == nil || !strings.Contains(err.Error(), "allow_default") {
		t.Fatalf("default without allow_default must be refused: %v", err)
	}
	in.CompiledHash = "sha256:nope"
	if _, err := routeApply(e, in); err == nil || !strings.Contains(err.Error(), "hash mismatch") {
		t.Fatalf("hash mismatch must be refused: %v", err)
	}
}

func TestRouteRollback(t *testing.T) {
	k := &fakeKernel{table: []routecmd.Route{via(t, "10.10.10.0/24", "192.168.11.64")}}
	e := newRouteTestExec(t, k)
	withFakeTable(e, k)
	defer func() { kernelTableHook = nil }()
	_ = e.writeSet("current.json", []routecmd.Route{via(t, "10.10.10.0/24", "192.168.11.64")})
	_ = e.writeSet("previous.json", []routecmd.Route{via(t, "10.30.0.0/16", "192.168.11.9")})
	out, err := routeRollback(e, routeTaskInput{TxnID: 9, RTBaseURL: "http://127.0.0.1:1", Mode: "rollback", ConfirmToken: "t"})
	if err != nil || out.(map[string]any)["rolled_back"] != true {
		t.Fatalf("rollback: %v %v", out, err)
	}
	joined := strings.Join(k.cmds, "\n")
	if !strings.Contains(joined, "route -n add -inet 10.30.0.0/16 192.168.11.9") || !strings.Contains(joined, "route -n delete -inet 10.10.10.0/24") {
		t.Fatalf("rollback cmds:\n%s", joined)
	}
	if cur := e.readSet("current.json"); len(cur) != 1 || cur[0].Dst != "10.30.0.0/16" {
		t.Fatalf("state swap: %+v", cur)
	}
}

func TestInverse(t *testing.T) {
	before := []routecmd.Route{via(t, "10.1.0.0/16", "10.0.0.1"), via(t, "10.2.0.0/16", "10.0.0.1")}
	ops := []routecmd.Op{
		{Action: "add", Route: via(t, "10.3.0.0/16", "10.0.0.1")},
		{Action: "change", Route: via(t, "10.1.0.0/16", "10.0.0.9")},
		{Action: "delete", Route: via(t, "10.2.0.0/16", "10.0.0.1")},
	}
	inv := inverse(ops, before)
	want := []string{"add 10.2.0.0/16 10.0.0.1", "change 10.1.0.0/16 10.0.0.1", "delete 10.3.0.0/16 10.0.0.1"}
	if len(inv) != len(want) {
		t.Fatalf("%+v", inv)
	}
	for i, op := range inv {
		got := strings.TrimSpace(op.Action + " " + op.Route.Dst + " " + op.Route.Gateway)
		if got != want[i] {
			t.Errorf("inv[%d] = %q, want %q", i, got, want[i])
		}
	}
}
