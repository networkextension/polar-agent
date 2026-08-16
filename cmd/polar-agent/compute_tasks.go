package main

// compute_tasks.go — polar-cloud P1-D: generic dock compute-tasks pull loop.
//
// dock queue = agent_tasks (doc/arch/task-processing-v2.md). Contract:
//   POST /api/agent/compute-tasks/claim        {skills:[...]}  → 204 (idle) | 200 AgentTask
//   POST /api/agent/compute-tasks/complete/:id {ok, output, error, artifacts[]}
// Claim is scoped to the agent-token owner's personal workspace and honours
// constraints_json {host_id|agent_id} pinning (dock claimerIdentity).
//
// Handlers register per skill (registerComputeHandler); the loop only runs
// when at least one handler exists. Wake-ups: periodic ticker
// (POLAR_COMPUTE_POLL_SEC, default 15s) + WS `task.wake` doorbell
// (wakeCompute). Single-flight drain; up to computeMaxParallel tasks in
// flight; every task ends with exactly one complete call.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type computeTask struct {
	ID          string          `json:"id"`
	WorkspaceID string          `json:"workspace_id"`
	Skill       string          `json:"skill"`
	Input       json.RawMessage `json:"input"`
	Constraints json.RawMessage `json:"constraints,omitempty"`
	Priority    int             `json:"priority"`
	Status      string          `json:"status"`
}

type computeArtifact struct {
	Kind    string `json:"kind"`
	AssetID int64  `json:"asset_id"`
	Mime    string `json:"mime,omitempty"`
	Bytes   int64  `json:"bytes,omitempty"`
}

// computeResult is the wire body of complete/:id. Output must marshal to a
// JSON object/array (jsonb column); nil → {}.
type computeResult struct {
	OK        bool              `json:"ok"`
	Output    any               `json:"output,omitempty"`
	Error     string            `json:"error,omitempty"`
	Artifacts []computeArtifact `json:"artifacts,omitempty"`
}

// computeHandler runs one claimed task and returns the output object (JSON
// serialisable) or an error (→ ok:false).
type computeHandler func(ctx context.Context, cfg AgentConfig, task computeTask) (any, error)

var (
	computeMu       sync.RWMutex
	computeHandlers = map[string]computeHandler{}
)

func registerComputeHandler(skill string, h computeHandler) {
	computeMu.Lock()
	defer computeMu.Unlock()
	if skill == "" || h == nil {
		panic("registerComputeHandler: empty skill or nil handler")
	}
	if _, dup := computeHandlers[skill]; dup {
		panic("registerComputeHandler: duplicate skill " + skill)
	}
	computeHandlers[skill] = h
}

func computeSkills() []string {
	computeMu.RLock()
	defer computeMu.RUnlock()
	out := make([]string, 0, len(computeHandlers))
	for k := range computeHandlers {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func lookupComputeHandler(skill string) computeHandler {
	computeMu.RLock()
	defer computeMu.RUnlock()
	return computeHandlers[skill]
}

// ── HTTP ─────────────────────────────────────────────────────────────────

var errComputeIdle = errors.New("compute: no task")

// claimComputeTask asks dock for one queued task matching skills. Returns
// (nil, nil) on 204.
func claimComputeTask(ctx context.Context, cfg AgentConfig, skills []string) (*computeTask, error) {
	endpoint, err := researchCallbackURL(cfg.Server, "/api/agent/compute-tasks/claim")
	if err != nil {
		return nil, err
	}
	body, _ := json.Marshal(map[string]any{"skills": skills})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {
		return nil, nil
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("claim: http %d: %s", resp.StatusCode, truncateForErr(string(raw)))
	}
	var t computeTask
	if err := json.Unmarshal(raw, &t); err != nil {
		return nil, fmt.Errorf("claim: decode: %w", err)
	}
	if t.ID == "" {
		return nil, nil
	}
	return &t, nil
}

func completeComputeTask(ctx context.Context, cfg AgentConfig, id string, res computeResult) error {
	endpoint, err := researchCallbackURL(cfg.Server, "/api/agent/compute-tasks/complete/"+id)
	if err != nil {
		return err
	}
	if res.Output == nil {
		res.Output = map[string]any{}
	}
	if len(res.Error) > 4000 {
		res.Error = res.Error[:4000]
	}
	return postJSON(ctx, cfg.Token, endpoint, res, nil)
}

// ── loop ─────────────────────────────────────────────────────────────────

const computeMaxParallel = 4

var (
	computeWakeCh   = make(chan struct{}, 1)
	computeDrainMu  sync.Mutex
	computeDraining bool
	computeSem      = make(chan struct{}, computeMaxParallel)
)

// wakeCompute nudges the loop (WS task.wake doorbell). Non-blocking.
func wakeCompute() {
	select {
	case computeWakeCh <- struct{}{}:
	default:
	}
}

func computePollInterval() time.Duration {
	if v := strings.TrimSpace(os.Getenv("POLAR_COMPUTE_POLL_SEC")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 3 {
			return time.Duration(n) * time.Second
		}
	}
	return 15 * time.Second
}

// computeTaskLoop polls until ctx is done. Started once per WS session.
func computeTaskLoop(ctx context.Context, cfg AgentConfig) {
	skills := computeSkills()
	if len(skills) == 0 {
		return
	}
	interval := computePollInterval()
	log.Printf("[compute] loop start skills=%v poll=%s", skills, interval)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	drainCompute(ctx, cfg, skills)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			drainCompute(ctx, cfg, skills)
		case <-computeWakeCh:
			drainCompute(ctx, cfg, skills)
		}
	}
}

// drainCompute claims until 204, running each task in its own goroutine
// (bounded by computeSem). Single-flight: a concurrent call returns at once.
func drainCompute(ctx context.Context, cfg AgentConfig, skills []string) {
	computeDrainMu.Lock()
	if computeDraining {
		computeDrainMu.Unlock()
		return
	}
	computeDraining = true
	computeDrainMu.Unlock()
	defer func() {
		computeDrainMu.Lock()
		computeDraining = false
		computeDrainMu.Unlock()
	}()
	for {
		if ctx.Err() != nil {
			return
		}
		// Don't claim more than we can run: wait for a slot first so a task
		// isn't marked running while it sits in our local backlog.
		select {
		case computeSem <- struct{}{}:
		case <-ctx.Done():
			return
		}
		task, err := claimComputeTask(ctx, cfg, skills)
		if err != nil {
			<-computeSem
			log.Printf("[compute] claim error: %v", err)
			return
		}
		if task == nil {
			<-computeSem
			return
		}
		go func(t computeTask) {
			defer func() { <-computeSem }()
			runComputeTask(ctx, cfg, t)
		}(*task)
	}
}

func runComputeTask(ctx context.Context, cfg AgentConfig, t computeTask) {
	h := lookupComputeHandler(t.Skill)
	log.Printf("[compute] claimed id=%s skill=%s", t.ID, t.Skill)
	var res computeResult
	if h == nil {
		res = computeResult{OK: false, Error: "no handler for skill " + t.Skill}
	} else {
		out, err := safeRunHandler(ctx, cfg, t, h)
		if err != nil {
			res = computeResult{OK: false, Error: err.Error(), Output: out}
		} else {
			res = computeResult{OK: true, Output: out}
		}
	}
	// Report with a fresh context so a dying session can still deliver.
	rctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := completeComputeTask(rctx, cfg, t.ID, res); err != nil {
		log.Printf("[compute] complete id=%s failed: %v", t.ID, err)
		return
	}
	if res.OK {
		log.Printf("[compute] done id=%s skill=%s", t.ID, t.Skill)
	} else {
		log.Printf("[compute] failed id=%s skill=%s: %s", t.ID, t.Skill, res.Error)
	}
}

func safeRunHandler(ctx context.Context, cfg AgentConfig, t computeTask, h computeHandler) (out any, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("handler panic: %v", r)
		}
	}()
	return h(ctx, cfg, t)
}
