//go:build darwin || freebsd || linux || openbsd

package main

// route_task.go — polar-routing executor. Claims compute-task skills
// `route_apply` / `route_rollback` and reconciles the kernel route table
// with the desired set compiled by routing-svc.
//
// Safety model (same shape as fw_task.go):
//
//	read managed set (current.json) → read kernel table → three-way diff →
//	execute ops → confirm handshake → commit (promote state files) / local rollback
//
// Only slots polar-routing previously wrote (current.json) are ever deleted;
// unmanaged kernel routes are never touched. Rollback re-applies the
// inverse ops recorded before execution, entirely locally — a wrong default
// route must be undone without the network.
//
// State dir (POLAR_AGENT_ROUTING_DIR, default ~/.polar/routing):
//
//	current.json  — last committed desired set (managed routes)
//	previous.json — managed set before the last apply (rollback target)
//	current.hash  — hash of current.json
//	base_url      — rt_base_url from the last task (collector dial-back)

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/networkextension/polar-agent/cmd/polar-agent/routecmd"
)

const (
	routeSkillApply    = "route_apply"
	routeSkillRollback = "route_rollback"

	routeConfirmRetryInterval = 3 * time.Second
	routeConfirmAttemptTO     = 10 * time.Second
)

var routeMu sync.Mutex

func registerRoutingHandlers() {
	registerComputeHandler(routeSkillApply, runRouteApplyTask)
	registerComputeHandler(routeSkillRollback, runRouteRollbackTask)
}

// routeTaskInput mirrors routing-svc's taskInput (key names are a contract,
// tested on both sides).
type routeTaskInput struct {
	TxnID              int64            `json:"txn_id"`
	RTBaseURL          string           `json:"rt_base_url"`
	Mode               string           `json:"mode"`
	Routes             []routecmd.Route `json:"routes"`
	CompiledHash       string           `json:"compiled_hash"`
	RollbackTimeoutSec int              `json:"rollback_timeout_sec"`
	ConfirmToken       string           `json:"confirm_token"`
	AllowDefault       bool             `json:"allow_default"`
}

// routeExec wraps the outside world (commands / HTTP / state dir) so tests
// inject stubs.
type routeExec struct {
	stateDir string
	goos     string
	run      func(ctx context.Context, argv ...string) (string, error)
	client   *http.Client
}

func newRouteExec() (*routeExec, error) {
	dir := strings.TrimSpace(os.Getenv("POLAR_AGENT_ROUTING_DIR"))
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		dir = filepath.Join(home, ".polar", "routing")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &routeExec{stateDir: dir, goos: runtime.GOOS, run: routeRunCmd,
		client: &http.Client{Timeout: routeConfirmAttemptTO}}, nil
}

// routeRunCmd executes argv (sudo -n prefixed when not root).
func routeRunCmd(ctx context.Context, argv ...string) (string, error) {
	argv = privPrefix(routecmd.ResolveBin(argv, nil)) // resolve first: sudo's secure_path also lacks /sbin on macOS
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	out := strings.TrimSpace(buf.String())
	if err != nil {
		return out, fmt.Errorf("%s: %w (%s)", strings.Join(argv, " "), err, truncateForErr(out))
	}
	return out, nil
}

func (e *routeExec) path(name string) string { return filepath.Join(e.stateDir, name) }

func (e *routeExec) readSet(name string) []routecmd.Route {
	var rs []routecmd.Route
	if b, err := os.ReadFile(e.path(name)); err == nil {
		_ = json.Unmarshal(b, &rs)
	}
	return rs
}

func (e *routeExec) writeSet(name string, rs []routecmd.Route) error {
	if rs == nil {
		rs = []routecmd.Route{}
	}
	b, _ := json.MarshalIndent(routecmd.Canonical(rs), "", "  ")
	return os.WriteFile(e.path(name), b, 0o600)
}

// kernelTableHook lets tests substitute the live table read.
var kernelTableHook func() ([]routecmd.Route, error)

// kernelTable reads the live table for both families. Listing runs without
// sudo (read-only) — use exec directly, not e.run.
func (e *routeExec) kernelTable(ctx context.Context) ([]routecmd.Route, error) {
	if kernelTableHook != nil {
		return kernelTableHook()
	}
	var all []routecmd.Route
	for _, fam := range []int{4, 6} {
		argv, err := routecmd.ListArgv(e.goos, fam)
		if err != nil {
			return nil, err
		}
		argv = routecmd.ResolveBin(argv, nil)
		out, err := exec.CommandContext(ctx, argv[0], argv[1:]...).Output()
		if err != nil {
			if fam == 6 {
				continue // v6 table optional
			}
			return nil, fmt.Errorf("route: list table: %w", err)
		}
		all = append(all, routecmd.ParseTable(e.goos, fam, string(out))...)
	}
	return all, nil
}

// inverse computes the ops that undo `ops` given the kernel state before
// execution: add→delete, delete→add(prev), change→change(prev).
func inverse(ops []routecmd.Op, before []routecmd.Route) []routecmd.Op {
	prev := map[string]routecmd.Route{}
	for _, r := range before {
		k := fmt.Sprintf("%d|%s", r.Family, r.Dst)
		if _, dup := prev[k]; !dup {
			prev[k] = r
		}
	}
	inv := make([]routecmd.Op, 0, len(ops))
	for i := len(ops) - 1; i >= 0; i-- {
		op := ops[i]
		k := fmt.Sprintf("%d|%s", op.Route.Family, op.Route.Dst)
		switch op.Action {
		case "add":
			inv = append(inv, routecmd.Op{Action: "delete", Route: op.Route})
		case "delete":
			if p, ok := prev[k]; ok {
				inv = append(inv, routecmd.Op{Action: "add", Route: p})
			}
		case "change":
			if p, ok := prev[k]; ok {
				inv = append(inv, routecmd.Op{Action: "change", Route: p})
			}
		}
	}
	return inv
}

// execOps runs each op; returns the ops that succeeded (for partial undo).
func (e *routeExec) execOps(ctx context.Context, ops []routecmd.Op) ([]routecmd.Op, error) {
	done := []routecmd.Op{}
	for _, op := range ops {
		argv, err := routecmd.Render(e.goos, op)
		if err != nil {
			return done, err
		}
		if _, err := e.run(ctx, argv...); err != nil {
			return done, fmt.Errorf("route: %s %s: %w", op.Action, op.Route.Dst, err)
		}
		done = append(done, op)
	}
	return done, nil
}

func (e *routeExec) persistBaseURL(in routeTaskInput) {
	if in.RTBaseURL == "" {
		return
	}
	if err := os.WriteFile(e.path("base_url"), []byte(in.RTBaseURL+"\n"), 0o600); err != nil {
		log.Printf("[route] persist base_url: %v", err)
	}
}

func (e *routeExec) promote(previous, current []routecmd.Route, hash string) error {
	if err := e.writeSet("previous.json", previous); err != nil {
		return err
	}
	if err := e.writeSet("current.json", current); err != nil {
		return err
	}
	return os.WriteFile(e.path("current.hash"), []byte(hash), 0o600)
}

// ── dial-back ───────────────────────────────────────────────────────

type routeConfirmResp struct {
	Commit bool   `json:"commit"`
	Status string `json:"status"`
	Error  string `json:"error"`
}

func (e *routeExec) postConfirm(ctx context.Context, in routeTaskInput, appliedHash string) (commit bool, definitive bool, err error) {
	body, _ := json.Marshal(map[string]any{
		"confirm_token": in.ConfirmToken,
		"applied_hash":  appliedHash,
		"health":        map[string]any{"goos": e.goos},
	})
	url := fmt.Sprintf("%s/agent/transactions/%d/confirm", strings.TrimRight(in.RTBaseURL, "/"), in.TxnID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return false, false, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.client.Do(req)
	if err != nil {
		return false, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 500 {
		return false, false, fmt.Errorf("confirm: http %d", resp.StatusCode)
	}
	if resp.StatusCode >= 400 {
		return false, true, fmt.Errorf("confirm rejected: http %d", resp.StatusCode)
	}
	var r routeConfirmResp
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return false, false, fmt.Errorf("confirm: decode: %w", err)
	}
	return r.Commit, true, nil
}

func (e *routeExec) confirmLoop(ctx context.Context, in routeTaskInput, appliedHash string, deadline time.Time) (bool, string) {
	for {
		commit, definitive, err := e.postConfirm(ctx, in, appliedHash)
		if definitive {
			if err != nil {
				return false, err.Error()
			}
			if !commit {
				return false, "controller refused commit"
			}
			return true, ""
		}
		if time.Now().After(deadline) {
			return false, fmt.Sprintf("controller unreachable within %ds window: %v", in.RollbackTimeoutSec, err)
		}
		log.Printf("[route] confirm txn=%d retry: %v", in.TxnID, err)
		select {
		case <-ctx.Done():
			return false, "cancelled: " + ctx.Err().Error()
		case <-time.After(routeConfirmRetryInterval):
		}
	}
}

func (e *routeExec) reportStatus(ctx context.Context, in routeTaskInput, status string) {
	body, _ := json.Marshal(map[string]any{"confirm_token": in.ConfirmToken, "status": status})
	url := fmt.Sprintf("%s/agent/transactions/%d/status", strings.TrimRight(in.RTBaseURL, "/"), in.TxnID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if resp, err := e.client.Do(req); err == nil {
		resp.Body.Close()
	} else {
		log.Printf("[route] status txn=%d: %v", in.TxnID, err)
	}
}

// ── handlers ────────────────────────────────────────────────────────

func parseRouteInput(t computeTask, wantMode string) (routeTaskInput, error) {
	var in routeTaskInput
	if err := json.Unmarshal(t.Input, &in); err != nil {
		return in, fmt.Errorf("route: bad input: %w", err)
	}
	if in.TxnID <= 0 || in.RTBaseURL == "" || in.ConfirmToken == "" {
		return in, fmt.Errorf("route: txn_id/rt_base_url/confirm_token required")
	}
	if in.Mode != wantMode {
		return in, fmt.Errorf("route: mode %q on skill for %q", in.Mode, wantMode)
	}
	if in.RollbackTimeoutSec <= 0 {
		in.RollbackTimeoutSec = 60
	}
	return in, nil
}

func runRouteApplyTask(ctx context.Context, cfg AgentConfig, t computeTask) (any, error) {
	in, err := parseRouteInput(t, "apply")
	if err != nil {
		return nil, err
	}
	e, err := newRouteExec()
	if err != nil {
		return nil, err
	}
	return routeApply(e, in)
}

// routeApply is the apply transaction. Deliberately detached from the claim
// loop's session ctx: once routes are in the kernel the confirm/rollback
// machine must run to completion even if the WS drops.
func routeApply(e *routeExec, in routeTaskInput) (any, error) {
	if got := routecmd.Hash(in.Routes); got != in.CompiledHash {
		return nil, fmt.Errorf("route: compiled hash mismatch (input %s, computed %s)", in.CompiledHash, got)
	}
	for _, r := range in.Routes {
		if r.IsDefault() && !in.AllowDefault {
			return nil, fmt.Errorf("route: default route %s without allow_default", r.Dst)
		}
	}
	routeMu.Lock()
	defer routeMu.Unlock()
	e.persistBaseURL(in)

	window := time.Duration(in.RollbackTimeoutSec) * time.Second
	opCtx, cancel := context.WithTimeout(context.Background(), window+2*time.Minute)
	defer cancel()
	deadline := time.Now().Add(window)

	e.reportStatus(opCtx, in, "applying")

	managed := e.readSet("current.json")
	kernel, err := e.kernelTable(opCtx)
	if err != nil {
		return nil, err
	}
	ops := routecmd.Diff(in.Routes, managed, kernel)
	undo := inverse(ops, kernel)

	done, err := e.execOps(opCtx, ops)
	if err != nil {
		// Partial apply: undo what succeeded, then fail without rollback flag
		// (nothing was committed; the controller sees failed).
		if len(done) > 0 {
			if _, uerr := e.execOps(opCtx, inverse(done, kernel)); uerr != nil {
				return map[string]any{"rolled_back": false}, fmt.Errorf("%v; UNDO FAILED: %v", err, uerr)
			}
		}
		return nil, err
	}
	log.Printf("[route] txn=%d applied %d op(s), confirming within %s", in.TxnID, len(ops), window)
	e.reportStatus(opCtx, in, "awaiting_confirm")

	commit, reason := e.confirmLoop(opCtx, in, in.CompiledHash, deadline)
	if !commit {
		if _, rerr := e.execOps(opCtx, undo); rerr != nil {
			return map[string]any{"rolled_back": false}, fmt.Errorf("route: %s; ROLLBACK FAILED: %v", reason, rerr)
		}
		log.Printf("[route] txn=%d rolled back: %s", in.TxnID, reason)
		return map[string]any{"rolled_back": true}, fmt.Errorf("route: rolled back: %s", reason)
	}
	if err := e.promote(managed, in.Routes, in.CompiledHash); err != nil {
		log.Printf("[route] txn=%d promote state files: %v", in.TxnID, err)
	}
	log.Printf("[route] txn=%d committed hash=%s (%d ops)", in.TxnID, in.CompiledHash, len(ops))
	return map[string]any{"committed": true, "applied_hash": in.CompiledHash, "ops": len(ops)}, nil
}

func runRouteRollbackTask(ctx context.Context, cfg AgentConfig, t computeTask) (any, error) {
	in, err := parseRouteInput(t, "rollback")
	if err != nil {
		return nil, err
	}
	e, err := newRouteExec()
	if err != nil {
		return nil, err
	}
	return routeRollback(e, in)
}

// routeRollback: make the kernel match previous.json (the managed set before
// the last apply), then swap current/previous. No confirm handshake — the
// result reaches routing-svc via the dock callback.
func routeRollback(e *routeExec, in routeTaskInput) (any, error) {
	routeMu.Lock()
	defer routeMu.Unlock()
	e.persistBaseURL(in)
	opCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	if _, err := os.Stat(e.path("previous.json")); err != nil {
		return nil, fmt.Errorf("route: nothing to roll back (no previous state): %w", err)
	}
	prev := e.readSet("previous.json")
	cur := e.readSet("current.json")
	e.reportStatus(opCtx, in, "applying")
	kernel, err := e.kernelTable(opCtx)
	if err != nil {
		return nil, err
	}
	ops := routecmd.Diff(prev, cur, kernel)
	if _, err := e.execOps(opCtx, ops); err != nil {
		return nil, err
	}
	if err := e.writeSet("current.json", prev); err != nil {
		log.Printf("[route] rollback txn=%d state swap: %v", in.TxnID, err)
	}
	if err := e.writeSet("previous.json", cur); err != nil {
		log.Printf("[route] rollback txn=%d state swap: %v", in.TxnID, err)
	}
	_ = os.WriteFile(e.path("current.hash"), []byte(routecmd.Hash(prev)), 0o600)
	log.Printf("[route] txn=%d rolled back to previous set (%d ops)", in.TxnID, len(ops))
	return map[string]any{"rolled_back": true, "ops": len(ops)}, nil
}
