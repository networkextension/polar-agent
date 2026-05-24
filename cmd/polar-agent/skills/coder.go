package skills

// Coder skill — wraps polar-agent's existing per-tool dispatch
// (kimi / claude / codex / operator-defined tools.json entries) as
// a Skill so dock-side UI can render which coding tools each host
// makes available without inspecting the agent's CLI flags.
//
// P1a: advertise metadata only; Start() returned ErrNotImplemented.
// P1c (this file): Start() actually runs the tool. The tool runner
// (which knows about toolSpec + invokeTool, both in the main package)
// is injected via NewCoderSkill so this file stays free of the
// circular import. main.go builds a closure over its loaded specs
// and passes it in.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
)

// CoderConfig is the operator-supplied blob for a host_skills row of
// kind=coder. Dock surfaces these via the skill config form in
// /hosts/:id; agent receives the same JSON inside skill.start at
// dispatch time.
type CoderConfig struct {
	// Mode: "passthrough" (one specific tool, agent was started with
	// --tool=X) or "tool-loop" (agent supports all configured tools,
	// dock picks per message).
	Mode string `json:"mode"`
	// Tool: the single tool name when Mode=="passthrough". Empty in
	// tool-loop mode.
	Tool string `json:"tool,omitempty"`
	// Tools: the full set of tool names this host can dispatch.
	// Always populated so the UI can show "this host has kimi +
	// claude + codex available" regardless of how the operator
	// started the agent.
	Tools []string `json:"tools"`
	// Workdir: absolute path the agent pinned at startup (the
	// directory tools cwd into for code edits). Surface-only for now.
	Workdir string `json:"workdir,omitempty"`
}

// CoderStartConfig is the per-call config dock sends inside the
// skill.start envelope's `config` blob. Smaller surface than
// CoderConfig (which describes the host_skills row) — this is the
// dispatch-time payload: which tool to invoke, the user's message,
// optional per-project workdir subpath, and optional git remote.
type CoderStartConfig struct {
	Tool           string `json:"tool"`
	Message        string `json:"message"`
	WorkdirSubpath string `json:"workdir_subpath,omitempty"`
	GitRemoteURL   string `json:"git_remote_url,omitempty"`
}

// CoderToolRunner is the injection-point for the actual subprocess
// dispatch. main.go provides a closure over its loaded `[]toolSpec`
// + `invokeTool`. Keeping the type narrow (strings in, strings out)
// means the skills package never has to know about toolSpec /
// chatMessage / chatReply — those stay in the main package.
//
// Returns (ok=true, content, stderr, nil) for a successful tool run
// (even one with non-empty stderr). Returns (ok=false, _, _, err)
// for invocation failures (binary missing, timeout, spawn error).
// A logical "tool ran but reported failure" maps to (ok=false,
// content_with_error_text, stderr, nil) — caller is free to put the
// reason in content or in err.
type CoderToolRunner func(
	ctx context.Context,
	toolName, workdir, message, workdirSubpath, gitRemoteURL string,
) (ok bool, content, stderr string, err error)

// Errors surfaced by Start().
var (
	ErrCoderNoRunner    = errors.New("coder skill: no tool runner injected (agent built without main bindings?)")
	ErrCoderToolMissing = errors.New("coder skill: requested tool not in advertised list")
	ErrCoderConfigEmpty = errors.New("coder skill: skill.start config missing")
)

type coderSkill struct {
	config CoderConfig
	runner CoderToolRunner
}

// NewCoderSkill constructs a Coder skill instance the agent will
// register at startup. `runner` is the injected dispatch closure;
// pass nil only in tests that don't exercise Start().
func NewCoderSkill(cfg CoderConfig, runner CoderToolRunner) Skill {
	if cfg.Mode == "" {
		cfg.Mode = "tool-loop"
	}
	if cfg.Tools == nil {
		cfg.Tools = []string{}
	}
	return &coderSkill{config: cfg, runner: runner}
}

func (c *coderSkill) Kind() SkillKind { return KindCoder }

func (c *coderSkill) Version() string { return "1.0" }

func (c *coderSkill) Capabilities() map[string]any {
	out := map[string]any{
		"mode":  c.config.Mode,
		"tools": c.config.Tools,
	}
	if c.config.Tool != "" {
		out["tool"] = c.config.Tool
	}
	if c.config.Workdir != "" {
		out["workdir"] = c.config.Workdir
	}
	return out
}

func (c *coderSkill) Validate(config json.RawMessage) error {
	if len(config) == 0 || string(config) == "null" {
		return nil
	}
	var probe CoderStartConfig
	if err := json.Unmarshal(config, &probe); err != nil {
		return err
	}
	if strings.TrimSpace(probe.Tool) != "" && !slices.Contains(c.config.Tools, probe.Tool) {
		return fmt.Errorf("%w: %q (advertised: %v)", ErrCoderToolMissing, probe.Tool, c.config.Tools)
	}
	return nil
}

func (c *coderSkill) Start(ctx context.Context, runID int64, config json.RawMessage) (Run, error) {
	if c.runner == nil {
		return nil, ErrCoderNoRunner
	}
	if len(config) == 0 {
		return nil, ErrCoderConfigEmpty
	}
	var sc CoderStartConfig
	if err := json.Unmarshal(config, &sc); err != nil {
		return nil, fmt.Errorf("coder skill: parse start config: %w", err)
	}
	// Resolve tool — passthrough mode auto-fills from advertised Tool,
	// tool-loop mode requires the caller to pick.
	tool := strings.TrimSpace(sc.Tool)
	if tool == "" {
		tool = c.config.Tool
	}
	if tool == "" {
		return nil, fmt.Errorf("coder skill: no tool in start config and host is tool-loop mode (Tools=%v)", c.config.Tools)
	}
	if !slices.Contains(c.config.Tools, tool) {
		return nil, fmt.Errorf("%w: %q (advertised: %v)", ErrCoderToolMissing, tool, c.config.Tools)
	}

	runCtx, cancel := context.WithCancel(ctx)
	run := &coderRun{
		id:     runID,
		events: make(chan Event, 8),
		cancel: cancel,
	}
	go run.execute(runCtx, c.runner, tool, c.config.Workdir, sc)
	return run, nil
}

// coderRun is the active execution handle. Lifecycle:
//
//	NewCoderSkill -> Start -> goroutine emits state/log/exit -> close
//
// The goroutine in execute() is the sole writer to events; Stop just
// cancels the ctx and waits for the goroutine to emit its final exit.
type coderRun struct {
	id       int64
	events   chan Event
	cancel   context.CancelFunc
	stopOnce sync.Once
}

func (r *coderRun) ID() int64                 { return r.id }
func (r *coderRun) Events() <-chan Event      { return r.events }
func (r *coderRun) Stop(reason string) error {
	r.stopOnce.Do(func() {
		// Cancel the goroutine's context; it will catch ctx.Err and
		// emit the final exit event with the reason, then close events.
		r.cancel()
	})
	return nil
}

func (r *coderRun) execute(ctx context.Context, runner CoderToolRunner, tool, workdir string, sc CoderStartConfig) {
	defer close(r.events)

	// Initial state event so dock-side can flip the run row from
	// 'starting' to 'running' as soon as the agent picks up.
	r.send(Event{Kind: EventState, Data: map[string]any{
		"status": "running",
		"tool":   tool,
	}})

	ok, content, stderr, err := runner(ctx, tool, workdir, sc.Message, sc.WorkdirSubpath, sc.GitRemoteURL)

	// Capture context-cancel as a distinct exit reason so the run row
	// can be marked 'cancelled' vs 'failed'.
	exitData := map[string]any{
		"ok":      ok,
		"content": content,
		"stderr":  stderr,
		"tool":    tool,
	}
	switch {
	case ctx.Err() != nil:
		exitData["ok"] = false
		exitData["error"] = "cancelled: " + ctx.Err().Error()
	case err != nil:
		exitData["ok"] = false
		exitData["error"] = err.Error()
	}
	r.send(Event{Kind: EventExit, Data: exitData})
}

// send is a non-blocking publish — if the consumer is gone (channel
// full + nobody draining), we log-then-drop rather than wedge the
// runner goroutine forever. The events channel has buffer 8 which is
// enough for typical Coder runs (3-5 events per task).
func (r *coderRun) send(ev Event) {
	select {
	case r.events <- ev:
	default:
		// Drop on full channel. coder runs only emit a handful of
		// events; if the buffer fills up the consumer is stuck and
		// blocking here would freeze the agent.
	}
}
