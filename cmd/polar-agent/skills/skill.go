// Package skills defines the contract for polar-agent's
// dynamically-enabled capabilities (coder, shell, proxy, wireguard,
// kdp, …). See github.com/networkextension/polar-hosts doc/host-module-design.md for the design
// rationale; github.com/networkextension/polar-hosts doc/host-module-dev.md for the phase plan.
//
// Phase 0 (this file) ships only the Skill interface + an in-memory
// Registry. No real skill implementations register here yet. The
// agent's main loop calls Advertised() at WS handshake time to
// produce the skill.advertise payload — in P0 this returns []; from
// P1 onward (coder.go, shell.go, etc.) each skill init() calls
// Register so the advertise envelope auto-populates.
package skills

import (
	"context"
	"encoding/json"
	"sync"
)

// SkillKind is the well-known string identifier for a skill type.
// Must match the dock-side string in host_skills.kind, so dock can
// route skill.start messages to the right Skill implementation.
type SkillKind string

const (
	KindCoder     SkillKind = "coder"
	KindShell     SkillKind = "shell"
	KindProxy     SkillKind = "proxy"
	KindWireGuard SkillKind = "wireguard"
	KindKDP   SkillKind = "kdp"
	KindMCPServer SkillKind = "mcp-server"
	KindVNC       SkillKind = "vnc"
)

// Skill is the per-kind contract. Implementations live in their own
// file under cmd/polar-agent/skills/<kind>.go (coder.go, shell.go, …).
// Phase 0 has no implementations — the interface is locked here so
// Phase 1+ can land one skill at a time without touching the contract.
type Skill interface {
	Kind() SkillKind

	// Version is a semver-ish string the agent advertises so dock can
	// gate on minimum versions when message protocols evolve.
	Version() string

	// Capabilities describes per-skill flexibility (e.g. coder
	// reports {"tools": ["kimi","claude","codex"]} so the UI can
	// populate a tool dropdown). Returned verbatim in the
	// skill.advertise payload.
	Capabilities() map[string]any

	// Validate is called by the agent before Start to check the
	// operator-supplied config blob. Returning a non-nil error rejects
	// the start request without spawning anything.
	Validate(config json.RawMessage) error

	// Start launches the skill with the given runID + config. The
	// returned Run lets the runner observe state + tell the skill to
	// stop. Start should be non-blocking (kick off goroutines if
	// needed); ctx cancellation aborts any in-flight setup.
	Start(ctx context.Context, runID int64, config json.RawMessage) (Run, error)
}

// Run is the handle to a started skill instance. Used by the agent
// runner (cmd/polar-agent/skills/runner.go, lands in P1) to receive
// events and trigger stop. Lifecycle is owned by the skill; Run.Stop
// must be idempotent and cleanly terminate any subprocess.
type Run interface {
	ID() int64

	// Events streams state changes + log lines + metric snapshots to
	// the agent runner, which forwards them as skill.event WS messages
	// to dock. Channel closes when the run exits for any reason.
	Events() <-chan Event

	// Stop initiates graceful shutdown. Implementations should send a
	// final Event with EventExit before closing the events channel.
	// reason is a free-form short string surfaced in UI ("operator",
	// "host_disabled", "restart").
	Stop(reason string) error
}

// Event is the structured message a Run pushes to the runner. Mapped
// 1:1 to the skill.event WS frame.
type Event struct {
	Kind EventKind      `json:"kind"`
	Data map[string]any `json:"data,omitempty"`
}

// EventKind enumerates the recognized event types. New kinds may be
// added later without breaking older agents — dock treats unknown
// kinds as log-equivalent.
type EventKind string

const (
	EventState  EventKind = "state"  // status, listen_addr, error_message
	EventLog    EventKind = "log"    // free-form text line
	EventMetric EventKind = "metric" // bytes/s, active_connections, etc.
	EventStdout EventKind = "stdout" // P2 (Shell): pty byte chunks, data.bytes_b64
	EventExit   EventKind = "exit"   // final event before Run terminates
)

// RunInput is satisfied by Runs that accept stdin byte chunks
// (Shell skill). The agent's loop.go type-asserts to this before
// honoring a skill.stdin envelope; non-stdin Runs (Coder) silently
// don't implement it and skill.stdin to them is a no-op.
type RunInput interface {
	WriteInput(data []byte) error
}

// RunResizer is satisfied by Runs that respond to TTY size changes
// (Shell skill). Same type-assert pattern as RunInput.
type RunResizer interface {
	Resize(rows, cols uint16) error
}

// Registry is the agent-wide map of registered skills, keyed by Kind.
// Populated at init time by each skill file's init() function.
type Registry struct {
	mu     sync.RWMutex
	skills map[SkillKind]Skill
}

var defaultRegistry = &Registry{skills: map[SkillKind]Skill{}}

// Register adds skill to the default registry. Intended for use in
// per-skill file init(); panics on duplicate Kind to surface
// programmer errors loudly. Test helpers in tests use NewRegistry
// instead.
func Register(skill Skill) {
	defaultRegistry.Register(skill)
}

// Default returns the package-level registry. The agent main loop
// uses Default().Advertised() to build the skill.advertise payload.
func Default() *Registry {
	return defaultRegistry
}

// NewRegistry returns an isolated registry — for unit tests that
// want to control which skills are visible without touching the
// package-level singleton.
func NewRegistry() *Registry {
	return &Registry{skills: map[SkillKind]Skill{}}
}

// Register adds skill to the registry. Panics on duplicate Kind.
func (r *Registry) Register(skill Skill) {
	if skill == nil {
		panic("skills.Register: nil Skill")
	}
	kind := skill.Kind()
	if kind == "" {
		panic("skills.Register: empty Kind")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.skills[kind]; exists {
		panic("skills.Register: duplicate Kind " + string(kind))
	}
	r.skills[kind] = skill
}

// Get returns the registered skill for kind, or (nil, false) if not
// registered. Used by the runner when dispatching skill.start.
func (r *Registry) Get(kind SkillKind) (Skill, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	skill, ok := r.skills[kind]
	return skill, ok
}

// AdvertisedSkill mirrors the dock-side AdvertisedSkill struct; the
// agent marshals this slice into the skill.advertise WS payload.
type AdvertisedSkill struct {
	Kind         string         `json:"kind"`
	Version      string         `json:"version,omitempty"`
	Capabilities map[string]any `json:"capabilities,omitempty"`
}

// Advertised returns a snapshot of every registered skill in a form
// ready to ship over WS. Sorted by Kind for deterministic output
// (makes the advertise payload diff-friendly in logs).
func (r *Registry) Advertised() []AdvertisedSkill {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]AdvertisedSkill, 0, len(r.skills))
	for _, s := range r.skills {
		out = append(out, AdvertisedSkill{
			Kind:         string(s.Kind()),
			Version:      s.Version(),
			Capabilities: s.Capabilities(),
		})
	}
	// Stable sort by kind for predictable serialization.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1].Kind > out[j].Kind; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}
