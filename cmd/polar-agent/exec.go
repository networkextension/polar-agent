package main

import (
	"context"
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

// toolCall is the wire shape received from the platform.
type toolCall struct {
	Kind string         `json:"kind"`
	ID   string         `json:"id"`
	Tool string         `json:"tool"`
	Args map[string]any `json:"args"`
}

// toolResult is the wire shape we send back. Always includes Kind +
// ID; OK / Output / Stdout / Stderr / Error vary by tool.
type toolResult struct {
	Kind   string         `json:"kind"`
	ID     string         `json:"id"`
	OK     bool           `json:"ok"`
	Stdout string         `json:"stdout,omitempty"`
	Stderr string         `json:"stderr,omitempty"`
	Output map[string]any `json:"output,omitempty"`
	Error  string         `json:"error,omitempty"`
}

// runCmdAllowList caps what `run_cmd` can spawn. Anything else =
// reject. The agent runs in the user's actual shell with full
// permissions (no sandbox per product decision), so this list
// exists to make accidental misuse hard, not to protect against
// a malicious LLM with full plan complicity.
var runCmdAllowList = map[string]bool{
	"git":         true,
	"swift":       true,
	"xcodebuild":  true,
	"xcrun":       true,
	"ls":          true,
	"cat":         true,
	"find":        true,
	"grep":        true,
	"swiftformat": true,
	"swiftlint":   true,
	"open":        true, // simulator screenshot helper
}

const runCmdTimeout = 5 * time.Minute

type executor struct {
	workdir string
	verbose bool
}

func newExecutor(workdir string, verbose bool) *executor {
	return &executor{workdir: workdir, verbose: verbose}
}

func (e *executor) dispatch(ctx context.Context, call toolCall) toolResult {
	if e.verbose {
		log.Printf("→ %s id=%s args=%v", call.Tool, call.ID, call.Args)
	}
	r := toolResult{Kind: "tool_result", ID: call.ID}
	switch call.Tool {
	case "read_file":
		e.toolReadFile(call, &r)
	case "write_file":
		e.toolWriteFile(call, &r)
	case "list_dir":
		e.toolListDir(call, &r)
	case "run_cmd":
		e.toolRunCmd(ctx, call, &r)
	default:
		r.Error = "unknown tool: " + call.Tool
	}
	if e.verbose {
		status := "ok"
		if !r.OK {
			status = "FAIL: " + r.Error
		}
		log.Printf("← %s id=%s %s", call.Tool, call.ID, status)
	}
	return r
}

// safeJoin resolves a relative path inside the workdir, refusing
// any path that escapes via "..".
func (e *executor) safeJoin(relPath string) (string, error) {
	relPath = strings.TrimSpace(relPath)
	if relPath == "" {
		return "", errors.New("path is empty")
	}
	full := filepath.Join(e.workdir, relPath)
	rel, err := filepath.Rel(e.workdir, full)
	if err != nil {
		return "", err
	}
	if strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("path %q escapes workdir", relPath)
	}
	return full, nil
}

func argString(args map[string]any, key string) string {
	v, ok := args[key]
	if !ok {
		return ""
	}
	s, _ := v.(string)
	return s
}

func argStringSlice(args map[string]any, key string) []string {
	v, ok := args[key]
	if !ok {
		return nil
	}
	raw, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, x := range raw {
		s, _ := x.(string)
		out = append(out, s)
	}
	return out
}

func (e *executor) toolReadFile(call toolCall, r *toolResult) {
	full, err := e.safeJoin(argString(call.Args, "path"))
	if err != nil {
		r.Error = err.Error()
		return
	}
	b, err := os.ReadFile(full)
	if err != nil {
		r.Error = err.Error()
		return
	}
	r.OK = true
	r.Output = map[string]any{
		"path":    argString(call.Args, "path"),
		"content": string(b),
		"size":    len(b),
	}
}

func (e *executor) toolWriteFile(call toolCall, r *toolResult) {
	rel := argString(call.Args, "path")
	full, err := e.safeJoin(rel)
	if err != nil {
		r.Error = err.Error()
		return
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		r.Error = err.Error()
		return
	}
	content := argString(call.Args, "content")
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		r.Error = err.Error()
		return
	}
	r.OK = true
	r.Output = map[string]any{
		"path":         rel,
		"bytes_written": len(content),
	}
}

func (e *executor) toolListDir(call toolCall, r *toolResult) {
	rel := argString(call.Args, "path")
	if rel == "" {
		rel = "."
	}
	full, err := e.safeJoin(rel)
	if err != nil {
		r.Error = err.Error()
		return
	}
	entries, err := os.ReadDir(full)
	if err != nil {
		r.Error = err.Error()
		return
	}
	out := make([]map[string]any, 0, len(entries))
	for _, ent := range entries {
		info, _ := ent.Info()
		size := int64(0)
		if info != nil && !ent.IsDir() {
			size = info.Size()
		}
		out = append(out, map[string]any{
			"name":  ent.Name(),
			"is_dir": ent.IsDir(),
			"size":  size,
		})
	}
	r.OK = true
	r.Output = map[string]any{"path": rel, "entries": out}
}

func (e *executor) toolRunCmd(ctx context.Context, call toolCall, r *toolResult) {
	cmdName := argString(call.Args, "cmd")
	if cmdName == "" {
		r.Error = "cmd required"
		return
	}
	if !runCmdAllowList[cmdName] {
		r.Error = "cmd not in allow list: " + cmdName
		return
	}
	args := argStringSlice(call.Args, "args")
	stdin := argString(call.Args, "stdin")

	cctx, cancel := context.WithTimeout(ctx, runCmdTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, cmdName, args...)
	cmd.Dir = e.workdir
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	} else {
		cmd.Stdin = io.NopCloser(strings.NewReader(""))
	}
	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()
	if err := cmd.Start(); err != nil {
		r.Error = err.Error()
		return
	}
	stdoutBytes, _ := io.ReadAll(stdout)
	stderrBytes, _ := io.ReadAll(stderr)
	err := cmd.Wait()
	exitCode := 0
	if ee, ok := err.(*exec.ExitError); ok {
		exitCode = ee.ExitCode()
	} else if err != nil {
		r.Error = err.Error()
		exitCode = -1
	}
	r.OK = err == nil
	r.Stdout = truncate(string(stdoutBytes), 200_000)
	r.Stderr = truncate(string(stderrBytes), 50_000)
	r.Output = map[string]any{
		"cmd":       cmdName,
		"args":      args,
		"exit_code": exitCode,
	}
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "\n...[truncated]"
}
