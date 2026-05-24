package main

// Multi-tool passthrough. polar-agent ships built-in specs for
// kimi-cli, claude, and codex, picked by --tool=<name> at attach
// time. Users override or add tools by dropping a tools.json into
// ~/.polar/.
//
// Same path as the original kimi-only flow:
//   chat_message → tools.spawn(spec, prompt, withResume) →
//     exec.Command(binary, args...) with stdout/stderr io.MultiWriter
//     to capBuffer + prefixWriter (so the operator's terminal sees
//     progress in real time) → wait → chat_reply.
//
// Resume strategy is per-tool (kimi-cli per-workdir --continue;
// claude --continue per cwd; codex exec resume --last). The spec
// declares args_first vs args_resume so the dispatcher doesn't
// need to know the per-tool semantics. First message in a fresh
// state always uses args_first; if args_resume errors with
// no_prior_session_marker in stderr, fall back to args_first
// (kimi's --continue case).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type toolSpec struct {
	Name                 string   `json:"name"`
	Binary               string   `json:"binary"`
	ArgsFirst            []string `json:"args_first"`
	ArgsResume           []string `json:"args_resume"`
	NoPriorSessionMarker string   `json:"no_prior_session_marker,omitempty"`
	// CWD: optional template (defaults to {{workdir}}). Tools like
	// claude resolve --continue against process cwd, so the agent
	// must chdir into the user's repo before exec; for tools that
	// take a workdir flag explicitly (kimi --work-dir, codex --cd)
	// the cwd field is cosmetic.
	CWD string `json:"cwd,omitempty"`
	// TimeoutSeconds: per-tool timeout. 0 means use the runtime
	// default (POLAR_KIMI_TIMEOUT env var or 30 minutes).
	TimeoutSeconds int `json:"timeout_seconds,omitempty"`
	// Env: per-tool environment variable overrides. Merged on top
	// of the parent process's env (parent inheritance preserved
	// for keys not listed here, override for keys that are).
	// Common use cases:
	//   - per-tool proxy: claude needs https_proxy but codex doesn't
	//   - per-tool API key: ANTHROPIC_API_KEY for claude only
	// Values support {{workdir}} substitution but no shell
	// expansion ($FOO is literal). If you need to indirect through
	// the operator's existing env, just don't list the key here
	// and let inheritance handle it.
	Env map[string]string `json:"env,omitempty"`
}

type toolsConfig struct {
	DefaultTool string     `json:"default_tool,omitempty"`
	Tools       []toolSpec `json:"tools"`
}

// builtinTools are the specs compiled in. Override by dropping
// ~/.polar/tools.json with the same shape; matching name in the
// override file replaces the built-in entry, new names are
// appended.
func builtinTools() []toolSpec {
	return []toolSpec{
		{
			Name:                 "kimi",
			Binary:               "kimi-cli",
			ArgsFirst:            []string{"--work-dir={{workdir}}", "--quiet", "--yolo", "--prompt={{prompt}}"},
			ArgsResume:           []string{"--work-dir={{workdir}}", "--quiet", "--yolo", "--continue", "--prompt={{prompt}}"},
			NoPriorSessionMarker: "No previous session",
		},
		{
			Name:   "claude",
			Binary: "claude",
			// claude's --add-dir is variadic (<directories...>) and
			// rejects the --add-dir=X form. Use space-separated args.
			ArgsFirst:  []string{"-p", "--add-dir", "{{workdir}}", "--permission-mode", "bypassPermissions", "{{prompt}}"},
			ArgsResume: []string{"-p", "--continue", "--add-dir", "{{workdir}}", "--permission-mode", "bypassPermissions", "{{prompt}}"},
			// claude resolves --continue against cwd; spec runs
			// itself in the workdir.
			CWD: "{{workdir}}",
		},
		{
			Name:   "codex",
			Binary: "codex",
			// codex (clap-based) rejects --cd=X; needs space-separated.
			// "unexpected argument '--cd' found" is the error you'll
			// hit otherwise.
			ArgsFirst:  []string{"exec", "--cd", "{{workdir}}", "--dangerously-bypass-approvals-and-sandbox", "{{prompt}}"},
			ArgsResume: []string{"exec", "resume", "--last", "--cd", "{{workdir}}", "--dangerously-bypass-approvals-and-sandbox", "{{prompt}}"},
		},
	}
}

func toolsConfigPath() string {
	if v := strings.TrimSpace(os.Getenv("POLAR_AGENT_TOOLS")); v != "" {
		return v
	}
	home, _ := os.UserHomeDir()
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".polar", "tools.json")
}

// loadToolsConfig merges built-ins with the optional user override
// file. User entries with a matching name replace the built-in
// (last write wins); new names are appended. Returns the merged
// list and the resolved default tool name.
func loadToolsConfig() ([]toolSpec, string, error) {
	merged := builtinTools()
	defaultTool := "kimi"

	path := toolsConfigPath()
	if path == "" {
		return merged, defaultTool, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return merged, defaultTool, nil
		}
		return nil, "", fmt.Errorf("read %s: %w", path, err)
	}
	var override toolsConfig
	if err := json.Unmarshal(raw, &override); err != nil {
		return nil, "", fmt.Errorf("parse %s: %w", path, err)
	}
	if strings.TrimSpace(override.DefaultTool) != "" {
		defaultTool = strings.TrimSpace(override.DefaultTool)
	}
	for _, override := range override.Tools {
		name := strings.TrimSpace(override.Name)
		if name == "" {
			continue
		}
		// Empty Binary is rejected — the override would silently wipe
		// the builtin's binary path and leave us calling exec.Command("")
		// at attach time, which surfaces as the cryptic
		// "start  : exec: no command" runtime error (issue #166).
		// Loader-side guard turns it into a clear, actionable warning
		// instead. Override authors must either keep the builtin (omit
		// the override entry) or copy the binary value through.
		if strings.TrimSpace(override.Binary) == "" {
			log.Printf("[tools] ignoring override %q in %s: \"binary\" field is empty; copy the builtin's binary path or remove the override entry", name, path)
			continue
		}
		replaced := false
		for i, existing := range merged {
			if existing.Name == name {
				merged[i] = override
				replaced = true
				break
			}
		}
		if !replaced {
			merged = append(merged, override)
		}
	}
	return merged, defaultTool, nil
}

func findToolSpec(specs []toolSpec, name string) (*toolSpec, error) {
	for i := range specs {
		if specs[i].Name == name {
			return &specs[i], nil
		}
	}
	return nil, fmt.Errorf("tool %q not found (available: %s)", name, joinToolNames(specs))
}

func joinToolNames(specs []toolSpec) string {
	names := make([]string, 0, len(specs))
	for _, s := range specs {
		names = append(names, s.Name)
	}
	return strings.Join(names, ", ")
}

// substituteArgs replaces {{workdir}} / {{prompt}} placeholders in
// each arg. Plain text replacement; the prompt is NOT shell-quoted
// (we exec.Command with separate argv, not a shell).
func substituteArgs(args []string, workdir, prompt string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = strings.NewReplacer("{{workdir}}", workdir, "{{prompt}}", prompt).Replace(a)
	}
	return out
}

func resolveCWD(spec *toolSpec, workdir string) string {
	if strings.TrimSpace(spec.CWD) == "" {
		return workdir
	}
	return strings.NewReplacer("{{workdir}}", workdir).Replace(spec.CWD)
}

// toolTimeout for a spec, falling back to the runtime
// POLAR_KIMI_TIMEOUT env var (legacy name kept for compat) or 30
// minutes default.
func toolTimeout(spec *toolSpec) time.Duration {
	if spec.TimeoutSeconds > 0 {
		return time.Duration(spec.TimeoutSeconds) * time.Second
	}
	return kimiTimeout()
}

// invokeTool runs the configured tool and returns a chatReply. The
// flow is exactly the same as the original kimi.go runKimi —
// streaming I/O via prefixWriter to operator stdout/stderr, bounded
// capBuffer for the chat reply, retry-without-resume on the
// "no_prior_session_marker" stderr substring.
func invokeTool(ctx context.Context, spec *toolSpec, workdir string, msg chatMessage, verbose bool) (*chatReply, error) {
	reply, retryFresh, err := runTool(ctx, spec, workdir, msg, true, verbose)
	if err != nil {
		return nil, err
	}
	if retryFresh {
		if verbose {
			log.Printf("[%s] no previous session, retrying with args_first", spec.Name)
		}
		reply, _, err = runTool(ctx, spec, workdir, msg, false, verbose)
		if err != nil {
			return nil, err
		}
	}
	return reply, nil
}

// runTool runs the tool once. retryFresh is true when the
// invocation failed with the configured "no prior session"
// stderr substring; the caller should retry with args_first.
func runTool(ctx context.Context, spec *toolSpec, workdir string, msg chatMessage, withResume, verbose bool) (*chatReply, bool, error) {
	timeout := toolTimeout(spec)
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	argsTemplate := spec.ArgsFirst
	if withResume && len(spec.ArgsResume) > 0 {
		argsTemplate = spec.ArgsResume
	}
	args := substituteArgs(argsTemplate, workdir, msg.Content)
	cwd := resolveCWD(spec, workdir)

	if verbose {
		log.Printf("[%s] spawn %s %s (cwd=%s, timeout=%s, %d byte prompt, thread=%d)",
			spec.Name, spec.Binary, redactPrompt(args, msg.Content), cwd, timeout, len(msg.Content), msg.ThreadID)
	}
	cmd := exec.CommandContext(cctx, spec.Binary, args...)
	cmd.Dir = cwd
	if len(spec.Env) > 0 {
		cmd.Env = mergeEnv(os.Environ(), spec.Env, workdir)
	}

	stdoutBuf := &capBuffer{cap: kimiMaxStdout}
	stderrBuf := &capBuffer{cap: kimiMaxStderr}
	cmd.Stdout = io.MultiWriter(prefixWriter{w: os.Stdout, prefix: "[" + spec.Name + "] "}, stdoutBuf)
	cmd.Stderr = io.MultiWriter(prefixWriter{w: os.Stderr, prefix: "[" + spec.Name + ":err] "}, stderrBuf)

	startedAt := time.Now()
	if err := cmd.Start(); err != nil {
		return nil, false, fmt.Errorf("start %s: %w", spec.Binary, err)
	}
	waitErr := cmd.Wait()
	if verbose {
		log.Printf("[%s] done in %s (exit_err=%v)", spec.Name, time.Since(startedAt).Truncate(time.Millisecond), waitErr)
	}

	stdoutBytes := stdoutBuf.Bytes()
	stderrBytes := stderrBuf.Bytes()
	reply := &chatReply{
		Kind:    "chat_reply",
		ID:      msg.ID,
		Content: string(stdoutBytes),
		Stderr:  string(stderrBytes),
	}
	if cctx.Err() == context.DeadlineExceeded {
		reply.OK = false
		reply.Error = fmt.Sprintf("%s timed out after %s (set POLAR_KIMI_TIMEOUT=<duration> on attach to extend, e.g. 1h)", spec.Name, timeout)
		return reply, false, nil
	}
	if waitErr != nil {
		// Detect "no prior session" so caller can retry with args_first.
		if withResume && spec.NoPriorSessionMarker != "" && strings.Contains(reply.Stderr, spec.NoPriorSessionMarker) {
			return nil, true, nil
		}
		reply.OK = false
		var ee *exec.ExitError
		if errors.As(waitErr, &ee) {
			reply.Error = fmt.Sprintf("%s exit %d", spec.Name, ee.ExitCode())
		} else {
			reply.Error = waitErr.Error()
		}
		return reply, false, nil
	}
	reply.OK = true
	if strings.TrimSpace(reply.Content) == "" && reply.Stderr != "" {
		reply.Content = fmt.Sprintf("(%s 没有 stdout，详见 stderr)", spec.Name)
	}
	return reply, false, nil
}

// mergeEnv produces the child process environment by merging
// per-tool overrides on top of the parent's. Override values
// support {{workdir}} substitution. Existing keys in parent are
// replaced; new keys are appended.
func mergeEnv(parent []string, overrides map[string]string, workdir string) []string {
	if len(overrides) == 0 {
		return parent
	}
	resolved := make(map[string]string, len(overrides))
	for k, v := range overrides {
		resolved[k] = strings.ReplaceAll(v, "{{workdir}}", workdir)
	}
	out := make([]string, 0, len(parent)+len(resolved))
	seen := map[string]bool{}
	for _, kv := range parent {
		i := strings.IndexByte(kv, '=')
		if i <= 0 {
			out = append(out, kv)
			continue
		}
		key := kv[:i]
		if newV, ok := resolved[key]; ok {
			out = append(out, key+"="+newV)
			seen[key] = true
		} else {
			out = append(out, kv)
		}
	}
	for k, v := range resolved {
		if !seen[k] {
			out = append(out, k+"="+v)
		}
	}
	return out
}

// redactPrompt elides the prompt portion of args for log lines
// (chat content can be huge / sensitive). We replace any arg that
// contains the literal prompt with "<…N bytes…>".
func redactPrompt(args []string, prompt string) string {
	if prompt == "" {
		return strings.Join(args, " ")
	}
	out := make([]string, len(args))
	marker := fmt.Sprintf("<%d bytes>", len(prompt))
	for i, a := range args {
		if strings.Contains(a, prompt) {
			out[i] = strings.ReplaceAll(a, prompt, marker)
		} else {
			out[i] = a
		}
	}
	return strings.Join(out, " ")
}
