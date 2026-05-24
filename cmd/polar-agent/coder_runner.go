package main

// Glue between the skills package's CoderToolRunner contract and the
// agent's existing invokeTool dispatch. Lives in the main package
// (not skills/) so the skills package doesn't have to import toolSpec
// / chatMessage / invokeTool. main.go calls makeCoderRunner at boot
// and hands the returned closure to skills.NewCoderSkill.

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/networkextension/polar-agent/cmd/polar-agent/skills"
)

// makeCoderRunner returns a CoderToolRunner closure over the agent's
// loaded toolSpecs. Mirrors the chat_message branch in loop.go: pre-task
// git plumbing, workdir subpath resolution + mkdir, invokeTool, post-task
// git commit/push. Errors come back as (ok=false, content, stderr, err);
// successful tool runs return (true, content, stderr, nil) even when
// stderr is non-empty.
func makeCoderRunner(specs []toolSpec, verbose bool) skills.CoderToolRunner {
	return func(ctx context.Context, toolName, workdir, message, workdirSubpath, gitRemoteURL string) (bool, string, string, error) {
		spec, err := findToolSpec(specs, toolName)
		if err != nil {
			return false, "", "", err
		}

		// Workdir subpath: mkdir + safety check (same logic as the
		// chat_message branch in loop.go).
		effectiveWorkdir := workdir
		if sp := strings.TrimSpace(workdirSubpath); sp != "" {
			if !isSafeSubpath(sp) {
				return false, "", "", fmt.Errorf("invalid workdir_subpath %q", sp)
			}
			sub := filepath.Join(workdir, sp)
			if err := os.MkdirAll(sub, 0o755); err != nil {
				return false, "", "", fmt.Errorf("mkdir %s: %w", sub, err)
			}
			effectiveWorkdir = sub
		}

		// Pre-task git plumbing. Same forgiving semantics as loop.go:
		// failures don't abort the tool run, they're surfaced in the
		// returned content tail.
		var preHEAD string
		var gitPrepErr error
		if remote := strings.TrimSpace(gitRemoteURL); remote != "" {
			log.Printf("[skill.start coder] git prepare workdir=%s remote=%s", effectiveWorkdir, remote)
			preHEAD, gitPrepErr = gitPrepare(ctx, effectiveWorkdir, remote)
			if gitPrepErr != nil {
				log.Printf("[skill.start coder] git prepare failed (continuing): %v", gitPrepErr)
			}
		}

		// Synthesize a chatMessage. The legacy fields (ID, ThreadID,
		// Kind) aren't used inside invokeTool; only Content +
		// WorkdirSubpath + GitRemoteURL matter for spawn-time logic, and
		// we've already resolved the subpath above so we pass it empty
		// — invokeTool wouldn't re-resolve it from the message struct
		// anyway since we pass effectiveWorkdir directly.
		msg := chatMessage{
			Content: message,
		}
		reply, runErr := invokeTool(ctx, spec, effectiveWorkdir, msg, verbose)
		if runErr != nil {
			return false, "", "", runErr
		}

		// Post-task git: stage + push if pre-prepare succeeded.
		// Append a summary line to the reply content the same way loop.go
		// does for the legacy path.
		if remote := strings.TrimSpace(gitRemoteURL); remote != "" && gitPrepErr == nil {
			commits, pushed, finErr := gitFinalize(ctx, effectiveWorkdir, preHEAD, "")
			log.Printf("[skill.start coder] git finalize commits=%d pushed=%v err=%v", len(commits), pushed, finErr)
			reply.Content += renderGitSummary(commits, pushed, finErr, remote)
		} else if gitPrepErr != nil {
			reply.Content += "\n\n---\n⚠️ git 准备失败：" + gitPrepErr.Error()
		}

		return reply.OK, reply.Content, reply.Stderr, nil
	}
}
