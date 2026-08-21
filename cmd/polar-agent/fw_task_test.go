//go:build darwin || freebsd || linux

package main

// fw executor 单测:exec/HTTP 全 stub,覆盖 apply 三条路径(commit、拒绝→
// 回滚、不可达→回滚)、备份取形、rollback 状态互换与输入合约。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeRun struct {
	calls   [][]string
	outputs map[string]string // 按命令前缀匹配的 canned 输出
	fail    map[string]string // 按命令前缀匹配 → 报错
}

func (f *fakeRun) run(_ context.Context, argv ...string) (string, error) {
	joined := strings.Join(argv, " ")
	f.calls = append(f.calls, argv)
	for prefix, msg := range f.fail {
		if strings.HasPrefix(joined, prefix) {
			return "", &execErr{msg}
		}
	}
	for prefix, out := range f.outputs {
		if strings.HasPrefix(joined, prefix) {
			return out, nil
		}
	}
	return "", nil
}

type execErr struct{ s string }

func (e *execErr) Error() string { return e.s }

func newTestExec(t *testing.T, backend, goos string, fr *fakeRun) *fwExec {
	t.Helper()
	return &fwExec{backend: backend, stateDir: t.TempDir(), goos: goos,
		run: fr.run, client: &http.Client{Timeout: 2 * time.Second}}
}

func testInput(baseURL string) fwTaskInput {
	compiled := "pass in quick all\n"
	return fwTaskInput{TxnID: 42, FWBaseURL: baseURL, Backend: fwBackendPF,
		Compiled: compiled, CompiledHash: fwHashOf(compiled),
		PolicyVersion: 3, RollbackTimeoutSec: 2, Mode: "apply", ConfirmToken: "tok"}
}

// confirmServer 起一个假 fw-svc:按脚本应答 confirm,记录 status 心跳。
func confirmServer(t *testing.T, commit bool, statusCode int) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/agent/transactions/42/confirm", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["confirm_token"] != "tok" {
			w.WriteHeader(401)
			return
		}
		if statusCode != 200 {
			w.WriteHeader(statusCode)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"commit": commit})
	})
	mux.HandleFunc("/agent/transactions/42/status", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})
	return httptest.NewServer(mux)
}

func writeState(t *testing.T, e *fwExec, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(e.stateDir, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readState(t *testing.T, e *fwExec, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(e.stateDir, name))
	if err != nil {
		return ""
	}
	return string(b)
}

func calledWithPrefix(fr *fakeRun, prefix string) bool {
	for _, c := range fr.calls {
		if strings.HasPrefix(strings.Join(c, " "), prefix) {
			return true
		}
	}
	return false
}

func TestFWApplyCommit(t *testing.T) {
	srv := confirmServer(t, true, 200)
	defer srv.Close()
	fr := &fakeRun{}
	e := newTestExec(t, fwBackendPF, "darwin", fr)
	writeState(t, e, "current.conf", "old ruleset\n")
	in := testInput(srv.URL)

	out, err := fwApply(e, in)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	m := out.(map[string]any)
	if m["committed"] != true {
		t.Fatalf("want committed, got %v", m)
	}
	if !calledWithPrefix(fr, "/sbin/pfctl -nf") || !calledWithPrefix(fr, "/sbin/pfctl -f") ||
		!calledWithPrefix(fr, "/sbin/pfctl -E") {
		t.Fatalf("missing pfctl steps: %v", fr.calls)
	}
	if got := readState(t, e, "current.conf"); got != in.Compiled {
		t.Fatalf("current.conf = %q", got)
	}
	if got := readState(t, e, "previous.conf"); got != "old ruleset\n" {
		t.Fatalf("previous.conf = %q (want old backup)", got)
	}
	if got := readState(t, e, "current.hash"); got != in.CompiledHash {
		t.Fatalf("current.hash = %q", got)
	}
}

func TestFWApplyRefusedRollsBack(t *testing.T) {
	srv := confirmServer(t, false, 200) // controller 明确拒绝
	defer srv.Close()
	fr := &fakeRun{}
	e := newTestExec(t, fwBackendPF, "darwin", fr)
	writeState(t, e, "current.conf", "old ruleset\n")

	out, err := fwApply(e, testInput(srv.URL))
	if err == nil || !strings.Contains(err.Error(), "rolled back") {
		t.Fatalf("want rolled-back error, got %v", err)
	}
	if out.(map[string]any)["rolled_back"] != true {
		t.Fatalf("output must flag rolled_back (fw-svc callbackOutcome 依赖): %v", out)
	}
	if got := readState(t, e, "rollback.conf"); got != "old ruleset\n" {
		t.Fatalf("rollback content = %q", got)
	}
	// 回滚后不得 promote。
	if got := readState(t, e, "current.conf"); got != "old ruleset\n" {
		t.Fatalf("current.conf must stay old, got %q", got)
	}
}

func TestFWApplyUnreachableRollsBack(t *testing.T) {
	fr := &fakeRun{}
	e := newTestExec(t, fwBackendPF, "darwin", fr)
	writeState(t, e, "current.conf", "old ruleset\n")
	in := testInput("http://127.0.0.1:1") // 连不上

	out, err := fwApply(e, in)
	if err == nil || !strings.Contains(err.Error(), "unreachable") {
		t.Fatalf("want unreachable error, got %v", err)
	}
	if out.(map[string]any)["rolled_back"] != true {
		t.Fatalf("want rolled_back output, got %v", out)
	}
}

func TestFWApplySyntaxFailNoRollbackNeeded(t *testing.T) {
	fr := &fakeRun{fail: map[string]string{"/sbin/pfctl -nf": "syntax error"}}
	e := newTestExec(t, fwBackendPF, "darwin", fr)
	writeState(t, e, "current.conf", "old ruleset\n")

	_, err := fwApply(e, testInput("http://127.0.0.1:1"))
	if err == nil || !strings.Contains(err.Error(), "syntax") {
		t.Fatalf("want syntax error, got %v", err)
	}
	if calledWithPrefix(fr, "/sbin/pfctl -f ") {
		t.Fatalf("must not load after failed check: %v", fr.calls)
	}
}

func TestFWHashMismatchRejected(t *testing.T) {
	in := testInput("http://x")
	in.CompiledHash = "sha256:deadbeef"
	raw, _ := json.Marshal(in)
	_, err := runFWApplyTask(context.Background(), AgentConfig{}, computeTask{Input: raw})
	if err == nil || !strings.Contains(err.Error(), "hash mismatch") {
		t.Fatalf("want hash mismatch, got %v", err)
	}
}

func TestFWBackendOSMismatch(t *testing.T) {
	e := newTestExec(t, fwBackendNFT, "darwin", &fakeRun{})
	if _, err := fwApply(e, testInput("http://x")); err == nil ||
		!strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("want unsupported, got %v", err)
	}
}

func TestFWNFTBackupShape(t *testing.T) {
	fr := &fakeRun{outputs: map[string]string{
		"/usr/sbin/nft list table inet polar_fw": "table inet polar_fw {\n\tchain input {\n\t}\n}",
	}}
	e := newTestExec(t, fwBackendNFT, "linux", fr)
	e.run = fr.run
	got, err := e.captureBackup(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got, "table inet polar_fw\ndelete table inet polar_fw\n") ||
		!strings.Contains(got, "chain input") {
		t.Fatalf("bad nft backup script:\n%s", got)
	}

	// 表不存在 → 纯删表脚本(还原到未管理状态)。
	fr2 := &fakeRun{fail: map[string]string{"/usr/sbin/nft list": "No such file or directory"}}
	e2 := newTestExec(t, fwBackendNFT, "linux", fr2)
	got2, err := e2.captureBackup(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got2 != "table inet polar_fw\ndelete table inet polar_fw\n" {
		t.Fatalf("bad empty backup script: %q", got2)
	}
}

func TestFWRollbackSwapsState(t *testing.T) {
	srv := confirmServer(t, true, 200)
	defer srv.Close()
	fr := &fakeRun{}
	e := newTestExec(t, fwBackendPF, "darwin", fr)
	writeState(t, e, "current.conf", "NEW\n")
	writeState(t, e, "previous.conf", "OLD\n")
	writeState(t, e, "current.hash", "sha256:x")
	in := testInput(srv.URL)
	in.Mode = "rollback"

	out, err := fwRollback(e, in)
	if err != nil {
		t.Fatal(err)
	}
	if out.(map[string]any)["rolled_back"] != true {
		t.Fatalf("want rolled_back, got %v", out)
	}
	if readState(t, e, "current.conf") != "OLD\n" || readState(t, e, "previous.conf") != "NEW\n" {
		t.Fatalf("state not swapped: cur=%q prev=%q",
			readState(t, e, "current.conf"), readState(t, e, "previous.conf"))
	}
	if readState(t, e, "current.hash") != "" {
		t.Fatal("current.hash must be cleared after rollback")
	}
}

func TestFWRollbackNothingToDo(t *testing.T) {
	e := newTestExec(t, fwBackendPF, "darwin", &fakeRun{})
	in := testInput("http://x")
	in.Mode = "rollback"
	if _, err := fwRollback(e, in); err == nil || !strings.Contains(err.Error(), "nothing to roll back") {
		t.Fatalf("want nothing-to-roll-back, got %v", err)
	}
}

// 输入键名合约(polar-firewall doc/api.md §5)。
func TestFWInputContract(t *testing.T) {
	raw := `{"txn_id":7,"fw_base_url":"https://x/api/firewall","backend":"pf",
		"compiled":"pass in quick all\n","compiled_hash":"h","policy_version":3,
		"rollback_timeout_sec":60,"mode":"apply","confirm_token":"t"}`
	var in fwTaskInput
	if err := json.Unmarshal([]byte(raw), &in); err != nil {
		t.Fatal(err)
	}
	if in.TxnID != 7 || in.Backend != "pf" || in.ConfirmToken != "t" ||
		in.RollbackTimeoutSec != 60 || in.PolicyVersion != 3 {
		t.Fatalf("bad parse: %+v", in)
	}
	if _, err := parseFWInput(computeTask{Input: []byte(raw)}, "rollback"); err == nil {
		t.Fatal("mode mismatch must be rejected")
	}
}
