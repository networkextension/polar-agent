package skills

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestCoderSkillKindAndVersion(t *testing.T) {
	s := NewCoderSkill(CoderConfig{Tools: []string{"kimi"}}, nil)
	if got := s.Kind(); got != KindCoder {
		t.Fatalf("Kind: got %q, want %q", got, KindCoder)
	}
	if s.Version() == "" {
		t.Fatal("Version: empty")
	}
}

func TestCoderSkillCapabilitiesToolLoopMode(t *testing.T) {
	s := NewCoderSkill(CoderConfig{
		Mode:    "tool-loop",
		Tools:   []string{"kimi", "claude", "codex"},
		Workdir: "/home/local/repo",
	}, nil)
	caps := s.Capabilities()
	if caps["mode"] != "tool-loop" {
		t.Errorf("mode: got %v, want tool-loop", caps["mode"])
	}
	tools, ok := caps["tools"].([]string)
	if !ok || len(tools) != 3 {
		t.Errorf("tools: got %v, want [kimi claude codex]", caps["tools"])
	}
	if caps["workdir"] != "/home/local/repo" {
		t.Errorf("workdir: got %v", caps["workdir"])
	}
	if _, present := caps["tool"]; present {
		t.Errorf("tool key should be absent in tool-loop mode, got %v", caps["tool"])
	}
}

func TestCoderSkillCapabilitiesPassthroughMode(t *testing.T) {
	s := NewCoderSkill(CoderConfig{
		Mode:  "passthrough",
		Tool:  "kimi",
		Tools: []string{"kimi", "claude", "codex"},
	}, nil)
	caps := s.Capabilities()
	if caps["mode"] != "passthrough" {
		t.Errorf("mode: got %v, want passthrough", caps["mode"])
	}
	if caps["tool"] != "kimi" {
		t.Errorf("tool: got %v, want kimi", caps["tool"])
	}
}

func TestCoderSkillDefaultsMode(t *testing.T) {
	s := NewCoderSkill(CoderConfig{Tools: []string{"kimi"}}, nil)
	if mode := s.Capabilities()["mode"]; mode != "tool-loop" {
		t.Errorf("default mode: got %v, want tool-loop", mode)
	}
}

func TestCoderSkillValidateAcceptsEmpty(t *testing.T) {
	s := NewCoderSkill(CoderConfig{Tools: []string{"kimi"}}, nil)
	for _, raw := range []json.RawMessage{nil, json.RawMessage("null"), json.RawMessage("{}")} {
		if err := s.Validate(raw); err != nil {
			t.Errorf("Validate(%q): %v, want nil", string(raw), err)
		}
	}
}

func TestCoderSkillValidateRejectsBadJSON(t *testing.T) {
	s := NewCoderSkill(CoderConfig{Tools: []string{"kimi"}}, nil)
	if err := s.Validate(json.RawMessage("{not-json")); err == nil {
		t.Fatal("Validate: want error on malformed JSON, got nil")
	}
}

func TestCoderSkillValidateRejectsUnknownTool(t *testing.T) {
	s := NewCoderSkill(CoderConfig{Tools: []string{"kimi", "claude"}}, nil)
	err := s.Validate(json.RawMessage(`{"tool":"nope"}`))
	if !errors.Is(err, ErrCoderToolMissing) {
		t.Errorf("Validate(nope): got %v, want ErrCoderToolMissing", err)
	}
}

func TestCoderSkillStartWithoutRunner(t *testing.T) {
	s := NewCoderSkill(CoderConfig{Tools: []string{"kimi"}}, nil)
	run, err := s.Start(context.Background(), 1, json.RawMessage(`{"tool":"kimi","message":"hi"}`))
	if run != nil {
		t.Errorf("Start: got Run %v, want nil", run)
	}
	if !errors.Is(err, ErrCoderNoRunner) {
		t.Errorf("Start: got err %v, want ErrCoderNoRunner", err)
	}
}

func TestCoderSkillStartRejectsUnknownTool(t *testing.T) {
	runner := func(ctx context.Context, tool, wd, msg, sub, git string) (bool, string, string, error) {
		t.Errorf("runner shouldn't be called for unknown tool")
		return false, "", "", nil
	}
	s := NewCoderSkill(CoderConfig{Tools: []string{"kimi"}}, runner)
	_, err := s.Start(context.Background(), 1, json.RawMessage(`{"tool":"claude","message":"x"}`))
	if !errors.Is(err, ErrCoderToolMissing) {
		t.Errorf("Start: got %v, want ErrCoderToolMissing", err)
	}
}

func TestCoderSkillStartFillsPassthroughTool(t *testing.T) {
	// In passthrough mode, omitting "tool" in start config should
	// fall back to the host's pinned Tool.
	var called string
	runner := func(ctx context.Context, tool, wd, msg, sub, git string) (bool, string, string, error) {
		called = tool
		return true, "ok-from-" + tool, "", nil
	}
	s := NewCoderSkill(CoderConfig{
		Mode:  "passthrough",
		Tool:  "kimi",
		Tools: []string{"kimi"},
	}, runner)
	run, err := s.Start(context.Background(), 42, json.RawMessage(`{"message":"hi"}`))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	events := drainEvents(t, run, 2*time.Second)
	if len(events) < 2 {
		t.Fatalf("expected at least state + exit, got %d events", len(events))
	}
	if events[0].Kind != EventState || events[0].Data["tool"] != "kimi" {
		t.Errorf("first event: got %+v, want state tool=kimi", events[0])
	}
	last := events[len(events)-1]
	if last.Kind != EventExit || last.Data["ok"] != true {
		t.Errorf("last event: got %+v, want exit ok=true", last)
	}
	if called != "kimi" {
		t.Errorf("runner called with tool=%q, want kimi", called)
	}
}

func TestCoderSkillStartHappyPath(t *testing.T) {
	runner := func(ctx context.Context, tool, wd, msg, sub, git string) (bool, string, string, error) {
		if tool != "kimi" || msg != "hello" || wd != "/repo" || sub != "proj-x" || git != "git@github.com:o/r.git" {
			t.Errorf("runner args wrong: tool=%q msg=%q wd=%q sub=%q git=%q", tool, msg, wd, sub, git)
		}
		return true, "tool said hi", "some stderr", nil
	}
	s := NewCoderSkill(CoderConfig{Mode: "tool-loop", Workdir: "/repo", Tools: []string{"kimi"}}, runner)
	run, err := s.Start(context.Background(), 7, json.RawMessage(`{"tool":"kimi","message":"hello","workdir_subpath":"proj-x","git_remote_url":"git@github.com:o/r.git"}`))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	events := drainEvents(t, run, 2*time.Second)
	if len(events) < 2 {
		t.Fatalf("want >=2 events, got %d", len(events))
	}
	exit := events[len(events)-1]
	if exit.Kind != EventExit {
		t.Fatalf("last event not exit: %+v", exit)
	}
	if exit.Data["ok"] != true || exit.Data["content"] != "tool said hi" || exit.Data["stderr"] != "some stderr" {
		t.Errorf("exit payload: %+v", exit.Data)
	}
}

func TestCoderSkillStartRunnerError(t *testing.T) {
	runner := func(ctx context.Context, tool, wd, msg, sub, git string) (bool, string, string, error) {
		return false, "", "", errors.New("binary not found")
	}
	s := NewCoderSkill(CoderConfig{Tools: []string{"kimi"}}, runner)
	run, err := s.Start(context.Background(), 9, json.RawMessage(`{"tool":"kimi","message":"x"}`))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	events := drainEvents(t, run, 2*time.Second)
	exit := events[len(events)-1]
	if exit.Kind != EventExit {
		t.Fatalf("last not exit: %+v", exit)
	}
	if exit.Data["ok"] != false || exit.Data["error"] != "binary not found" {
		t.Errorf("exit payload: %+v", exit.Data)
	}
}

func TestCoderSkillStartStopCancels(t *testing.T) {
	// Runner sleeps until ctx cancel — Stop() should unblock it.
	runner := func(ctx context.Context, tool, wd, msg, sub, git string) (bool, string, string, error) {
		select {
		case <-ctx.Done():
			return false, "", "", ctx.Err()
		case <-time.After(5 * time.Second):
			return true, "should-not-finish", "", nil
		}
	}
	s := NewCoderSkill(CoderConfig{Tools: []string{"kimi"}}, runner)
	run, err := s.Start(context.Background(), 11, json.RawMessage(`{"tool":"kimi","message":"x"}`))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	time.Sleep(20 * time.Millisecond) // let the runner start
	_ = run.Stop("operator")
	events := drainEvents(t, run, 2*time.Second)
	exit := events[len(events)-1]
	if exit.Kind != EventExit {
		t.Fatalf("last not exit: %+v", exit)
	}
	if exit.Data["ok"] != false {
		t.Errorf("expected ok=false after cancel, got %+v", exit.Data)
	}
	errStr, _ := exit.Data["error"].(string)
	if errStr == "" {
		t.Errorf("expected error message on cancel, got %+v", exit.Data)
	}
}

func TestCoderSkillRegisterAdvertised(t *testing.T) {
	r := NewRegistry()
	skill := NewCoderSkill(CoderConfig{
		Mode:  "passthrough",
		Tool:  "kimi",
		Tools: []string{"kimi"},
	}, nil)
	r.Register(skill)

	got := r.Advertised()
	if len(got) != 1 {
		t.Fatalf("Advertised: got %d entries, want 1", len(got))
	}
	if got[0].Kind != string(KindCoder) {
		t.Errorf("Advertised[0].Kind: got %q, want %q", got[0].Kind, KindCoder)
	}
	if got[0].Capabilities["mode"] != "passthrough" {
		t.Errorf("Advertised[0].Capabilities.mode: got %v", got[0].Capabilities["mode"])
	}
	if got[0].Capabilities["tool"] != "kimi" {
		t.Errorf("Advertised[0].Capabilities.tool: got %v", got[0].Capabilities["tool"])
	}
}

func drainEvents(t *testing.T, run Run, timeout time.Duration) []Event {
	t.Helper()
	var out []Event
	deadline := time.After(timeout)
	for {
		select {
		case ev, ok := <-run.Events():
			if !ok {
				return out
			}
			out = append(out, ev)
		case <-deadline:
			t.Fatalf("drainEvents: timed out after %v with %d events", timeout, len(out))
			return out
		}
	}
}
