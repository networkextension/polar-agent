package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"time"
)

// Pull-based task claim (P3 of doc/arch/agent-proxy-and-task-dispatch.md).
//
// dock can enqueue research runs durably and ring a {"kind":"task.wake"}
// doorbell instead of pushing the envelope over WS. The agent responds by
// claiming runs via POST /api/agent/tasks/claim until the queue is empty
// (HTTP 204), and runs each claimed envelope exactly like a pushed
// research_task. It also drains once on every (re)connect to pick up any
// backlog that accumulated while it was offline.
//
// This is always safe to call: when dock's pull mode is off it pushes
// instead, so the queue is empty and claim just returns 204.

// claimedJob is the generic envelope the dock claim endpoint returns for an
// agent_jobs row: a kind discriminator, the job id, and an opaque payload the
// per-kind runner interprets. (Research runs use the push path, not claim.)
type claimedJob struct {
	Kind    string          `json:"kind"`
	JobID   int64           `json:"job_id"`
	Payload json.RawMessage `json:"payload"`
}

// claimTask claims the next queued fleet job this agent is capable of running.
// Returns (job, true, nil) on a claimed job, (nil, false, nil) on an empty queue
// (HTTP 204), or an error.
func claimTask(ctx context.Context, cfg AgentConfig) (*claimedJob, bool, error) {
	endpoint, err := researchCallbackURL(cfg.Server, "/api/agent/tasks/claim")
	if err != nil {
		return nil, false, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
	if err != nil {
		return nil, false, err
	}
	if cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.Token)
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {
		return nil, false, nil // queue empty
	}
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, false, fmt.Errorf("http %d: %s", resp.StatusCode, truncateForErr(string(body)))
	}
	var job claimedJob
	if err := json.Unmarshal(body, &job); err != nil {
		return nil, false, fmt.Errorf("claim decode: %w", err)
	}
	if job.JobID == 0 {
		return nil, false, nil
	}
	return &job, true, nil
}

// drainState single-flights drainClaims: only one drain runs at a time;
// a wake that arrives mid-drain sets a rerun flag so the running drainer
// loops once more (no lost task between a 204 and a fresh enqueue).
var (
	drainMu      sync.Mutex
	draining     bool
	drainPending bool
)

// drainClaims claims and runs queued tasks until the queue is empty.
// Each claimed envelope runs in its own goroutine, exactly like a pushed
// research_task. Safe to call concurrently — calls coalesce.
func drainClaims(ctx context.Context, cfg AgentConfig, workdir string, verbose bool) {
	drainMu.Lock()
	if draining {
		drainPending = true
		drainMu.Unlock()
		return
	}
	draining = true
	drainMu.Unlock()

	for {
		for {
			env, ok, err := claimTask(ctx, cfg)
			if err != nil {
				log.Printf("[claim] error: %v", err)
				break
			}
			if !ok {
				break // 204 — queue empty
			}
			go func(j claimedJob) {
				if rerr := runClaimedJob(ctx, cfg, workdir, j, verbose); rerr != nil {
					log.Printf("[job] id=%d kind=%s error: %v", j.JobID, j.Kind, rerr)
				}
			}(*env)
		}
		drainMu.Lock()
		if drainPending {
			drainPending = false
			drainMu.Unlock()
			continue // a wake arrived mid-drain — loop again
		}
		draining = false
		drainMu.Unlock()
		return
	}
}
