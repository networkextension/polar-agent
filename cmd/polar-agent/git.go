package main

// Phase 1 of project-pipeline Stage 5 (push code). When the
// platform sends `git_remote_url` along with a chat_message, we
// (1) ensure the workdir is a git repo with that remote configured,
// (2) capture pre-task HEAD, (3) let the coder tool run, then
// (4) post-task: stage everything, commit anything still unstaged
// (the "tail commit"), enumerate commits made during the task,
// (5) push to the remote.
//
// Auth: this iteration relies entirely on whatever git auth the
// host already has — SSH keys for ssh:// remotes, ~/.netrc /
// credential helper for https://. Per-project deploy keys are
// Phase 2 (server stores credential, agent fetches just-in-time).
//
// Failure model: NEVER fail the chat_reply because git failed.
// Coder output is the user's value; git is best-effort. Surface
// errors as part of reply.Content so the user sees them.

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

const (
	gitOpTimeout      = 90 * time.Second
	gitCloneTimeout   = 5 * time.Minute
	gitPushTimeout    = 5 * time.Minute
	gitTailCommitMsg  = "[polar-agent] task tail"
	gitTailCommitUser = "polar-agent"
	// gitTailCommitMail is what shows up in `git log` author for
	// the post-task tail commit. Operators can override via the
	// usual GIT_AUTHOR_* env on the polar-agent process.
	gitTailCommitMail = "polar-agent@polar.local"
)

type gitCommit struct {
	SHA     string
	Subject string
}

// gitPrepare ensures workdir is a git working tree with `origin`
// pointing at remoteURL, and returns the current HEAD SHA (empty
// when the repo has no commits yet — first task on a fresh repo).
func gitPrepare(ctx context.Context, workdir, remoteURL string) (preHEAD string, err error) {
	switch {
	case isGitRepo(workdir):
		// Existing repo: just make sure origin matches and fetch.
		if err := ensureRemote(ctx, workdir, "origin", remoteURL); err != nil {
			return "", err
		}
		// Best-effort fetch — failure isn't fatal (network down,
		// auth missing, etc.); coder can still work with local state.
		_, _ = runGit(ctx, workdir, gitCloneTimeout, "fetch", "--quiet", "origin")

	case isDirEmpty(workdir):
		// Empty workdir: clone into it. Cleanest path — we get the
		// remote's history, default branch, and HEAD positioned for
		// a normal first commit + push without rejection.
		if _, err := runGit(ctx, workdir, gitCloneTimeout, "clone", "--quiet", remoteURL, "."); err != nil {
			// Clone failed (auth / unreachable / empty remote /
			// permission). Fall back to init + remote add so the
			// coder still has a working repo locally; first push
			// may collide with the remote, surfaced in finalize.
			if _, ierr := runGit(ctx, workdir, gitOpTimeout, "init", "-q"); ierr != nil {
				return "", fmt.Errorf("git clone (%v) and git init (%v) both failed", err, ierr)
			}
			if _, rerr := runGit(ctx, workdir, gitOpTimeout, "remote", "add", "origin", remoteURL); rerr != nil {
				return "", fmt.Errorf("git remote add: %w", rerr)
			}
		}

	default:
		// Non-git workdir with files in it (e.g. earlier task ran
		// without git_remote_url and left output behind). init +
		// add remote. First push may need to be a fast-forward if
		// the remote has no commits, otherwise it'll collide and
		// the user has to resolve by reseting / re-creating the dir.
		if _, err := runGit(ctx, workdir, gitOpTimeout, "init", "-q"); err != nil {
			return "", fmt.Errorf("git init: %w", err)
		}
		if _, err := runGit(ctx, workdir, gitOpTimeout, "remote", "add", "origin", remoteURL); err != nil {
			return "", fmt.Errorf("git remote add: %w", err)
		}
	}

	preHEAD, _ = headSHA(ctx, workdir) // empty on a no-commits repo
	return preHEAD, nil
}

func shortSHA(sha string) string {
	if len(sha) < 7 {
		return sha
	}
	return sha[:7]
}

func isDirEmpty(dir string) bool {
	f, err := os.Open(dir)
	if err != nil {
		return false
	}
	defer f.Close()
	names, err := f.Readdirnames(1)
	if err != nil {
		return true // directory readable but Readdirnames returned EOF → empty
	}
	return len(names) == 0
}

// gitFinalize stages anything still untracked/modified, commits
// it (the "tail" commit), then enumerates commits made between
// preHEAD and the new HEAD, and pushes the current branch.
func gitFinalize(ctx context.Context, workdir, preHEAD, taskTitle string) (commits []gitCommit, pushed bool, err error) {
	dirty, err := workingTreeDirty(ctx, workdir)
	if err != nil {
		return nil, false, fmt.Errorf("git status: %w", err)
	}
	if dirty {
		if _, err := runGit(ctx, workdir, gitOpTimeout, "add", "-A"); err != nil {
			return nil, false, fmt.Errorf("git add -A: %w", err)
		}
		msg := gitTailCommitMsg
		if t := strings.TrimSpace(taskTitle); t != "" {
			msg += ": " + t
		}
		args := []string{
			"-c", "user.name=" + gitTailCommitUser,
			"-c", "user.email=" + gitTailCommitMail,
			"commit", "--allow-empty-message", "-q", "-m", msg,
		}
		if _, err := runGit(ctx, workdir, gitOpTimeout, args...); err != nil {
			return nil, false, fmt.Errorf("git commit (tail): %w", err)
		}
	}

	postHEAD, err := headSHA(ctx, workdir)
	if err != nil {
		return nil, false, fmt.Errorf("git rev-parse HEAD: %w", err)
	}
	if postHEAD == "" {
		return nil, false, nil
	}
	commits, err = listCommitsBetween(ctx, workdir, preHEAD, postHEAD)
	if err != nil {
		return nil, false, fmt.Errorf("git log: %w", err)
	}
	if len(commits) == 0 {
		return nil, false, nil
	}

	branch, err := currentBranch(ctx, workdir)
	if err != nil || branch == "" {
		// detached HEAD or fresh init with no commit-on-branch — set
		// a default branch so the push has something to target.
		branch = "main"
		_, _ = runGit(ctx, workdir, gitOpTimeout, "branch", "-M", branch)
	}
	if _, err := runGit(ctx, workdir, gitPushTimeout, "push", "-u", "origin", branch); err != nil {
		return commits, false, fmt.Errorf("git push: %w", err)
	}
	return commits, true, nil
}

// renderGitSummary turns the result of gitFinalize into a markdown
// fragment to append to the chat reply. Designed to be readable
// inline so the user sees what got pushed without leaving the chat.
func renderGitSummary(commits []gitCommit, pushed bool, gitErr error, remoteURL string) string {
	if gitErr != nil && len(commits) == 0 {
		return "\n\n---\n⚠️ git 失败：" + gitErr.Error()
	}
	if len(commits) == 0 {
		return "\n\n---\n（任务没产生任何文件改动，git 无需推送）"
	}
	var b strings.Builder
	b.WriteString("\n\n---\n")
	if pushed {
		b.WriteString(fmt.Sprintf("✅ 推送 %d 个 commit 到 `%s`：\n", len(commits), remoteURL))
	} else {
		b.WriteString(fmt.Sprintf("⚠️ 本地 commit 了 %d 条但 push 失败（`%s`）\n", len(commits), remoteURL))
	}
	for _, c := range commits {
		short := c.SHA
		if len(short) > 7 {
			short = short[:7]
		}
		b.WriteString(fmt.Sprintf("- `%s` %s\n", short, c.Subject))
	}
	if gitErr != nil {
		b.WriteString("\n错误：" + gitErr.Error())
	}
	return b.String()
}

// ---- helpers ----

func runGit(ctx context.Context, workdir string, timeout time.Duration, args ...string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, "git", args...)
	cmd.Dir = workdir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("%s", msg)
	}
	return strings.TrimRight(stdout.String(), "\n"), nil
}

func isGitRepo(workdir string) bool {
	cmd := exec.Command("git", "rev-parse", "--is-inside-work-tree")
	cmd.Dir = workdir
	return cmd.Run() == nil
}

func ensureRemote(ctx context.Context, workdir, name, url string) error {
	have, _ := runGit(ctx, workdir, gitOpTimeout, "remote", "get-url", name)
	if have == url {
		return nil
	}
	if have == "" {
		_, err := runGit(ctx, workdir, gitOpTimeout, "remote", "add", name, url)
		return err
	}
	_, err := runGit(ctx, workdir, gitOpTimeout, "remote", "set-url", name, url)
	return err
}

func headSHA(ctx context.Context, workdir string) (string, error) {
	out, err := runGit(ctx, workdir, gitOpTimeout, "rev-parse", "HEAD")
	if err != nil {
		// Fresh repo with no commits — `git rev-parse HEAD` exits 128.
		// Treat as "no HEAD yet" and return empty.
		return "", nil
	}
	return strings.TrimSpace(out), nil
}

func currentBranch(ctx context.Context, workdir string) (string, error) {
	out, err := runGit(ctx, workdir, gitOpTimeout, "symbolic-ref", "--short", "HEAD")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

func workingTreeDirty(ctx context.Context, workdir string) (bool, error) {
	out, err := runGit(ctx, workdir, gitOpTimeout, "status", "--porcelain")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) != "", nil
}

func listCommitsBetween(ctx context.Context, workdir, from, to string) ([]gitCommit, error) {
	rangeArg := to
	if from != "" {
		rangeArg = from + ".." + to
	}
	// Use a sentinel separator unlikely to appear in subjects.
	const sep = "<<__POLAR__>>"
	out, err := runGit(ctx, workdir, gitOpTimeout, "log", rangeArg, "--reverse", "--pretty=format:%H"+sep+"%s")
	if err != nil {
		return nil, err
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return nil, nil
	}
	var commits []gitCommit
	for _, line := range strings.Split(out, "\n") {
		parts := strings.SplitN(line, sep, 2)
		if len(parts) != 2 {
			continue
		}
		commits = append(commits, gitCommit{
			SHA:     strings.TrimSpace(parts[0]),
			Subject: strings.TrimSpace(parts[1]),
		})
	}
	return commits, nil
}
