package main

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRenderGitSummary(t *testing.T) {
	cases := []struct {
		name     string
		commits  []gitCommit
		pushed   bool
		gitErr   error
		remote   string
		contains []string
	}{
		{
			name:     "no commits no error",
			commits:  nil,
			pushed:   false,
			remote:   "git@github.com:foo/bar.git",
			contains: []string{"任务没产生任何文件改动"},
		},
		{
			name: "two commits pushed",
			commits: []gitCommit{
				{SHA: "abcdef0123456789", Subject: "first"},
				{SHA: "fedcba9876543210", Subject: "second"},
			},
			pushed:   true,
			remote:   "git@github.com:foo/bar.git",
			contains: []string{"推送 2 个 commit", "abcdef0", "first", "fedcba9", "second"},
		},
		{
			name: "commits but push failed",
			commits: []gitCommit{
				{SHA: "abcdef0123456789", Subject: "only one"},
			},
			pushed:   false,
			remote:   "git@github.com:foo/bar.git",
			contains: []string{"本地 commit 了 1 条但 push 失败"},
		},
		{
			name:     "git failed before any commit",
			commits:  nil,
			pushed:   false,
			gitErr:   errStub{"auth failed"},
			remote:   "git@github.com:foo/bar.git",
			contains: []string{"git 失败", "auth failed"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := renderGitSummary(tc.commits, tc.pushed, tc.gitErr, tc.remote)
			for _, want := range tc.contains {
				if !strings.Contains(out, want) {
					t.Fatalf("expected output to contain %q, got: %s", want, out)
				}
			}
		})
	}
}

type errStub struct{ msg string }

func (e errStub) Error() string { return e.msg }

// TestGitPrepareFinalizeRoundtrip covers the real flow: a local
// bare repo as the "remote", an empty workdir, gitPrepare clones
// it, a coder writes a file, gitFinalize commits + pushes, the
// commits show up in the bare repo's log.
//
// Skipped when `git` is not in PATH so a sandboxed CI doesn't fail.
func TestGitPrepareFinalizeRoundtrip(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not in PATH")
	}
	ctx := context.Background()
	root := t.TempDir()

	bareRepo := filepath.Join(root, "remote.git")
	if err := exec.Command("git", "init", "--quiet", "--bare", bareRepo).Run(); err != nil {
		t.Fatalf("init bare: %v", err)
	}

	work := filepath.Join(root, "work")
	if err := exec.Command("mkdir", "-p", work).Run(); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	pre, err := gitPrepare(ctx, work, bareRepo)
	if err != nil {
		t.Fatalf("gitPrepare: %v", err)
	}
	// Pre-HEAD on a freshly init'd-after-failed-clone (bare repo has
	// no commits, so clone makes a no-commits repo) should be empty.
	if pre != "" {
		t.Fatalf("expected empty preHEAD for fresh repo, got %q", pre)
	}

	// Simulate coder output: write a file inside work.
	if err := exec.Command("sh", "-c", "echo hello > "+filepath.Join(work, "out.txt")).Run(); err != nil {
		t.Fatalf("write file: %v", err)
	}

	// gitFinalize should add+commit+push. Use a unique title so the
	// commit message includes it.
	commits, pushed, err := gitFinalize(ctx, work, pre, "test task title")
	if err != nil {
		t.Fatalf("gitFinalize: %v", err)
	}
	if !pushed {
		t.Fatalf("expected pushed=true, got false")
	}
	if len(commits) != 1 {
		t.Fatalf("expected 1 commit, got %d", len(commits))
	}
	if !strings.Contains(commits[0].Subject, "test task title") {
		t.Fatalf("expected commit subject to mention task title, got %q", commits[0].Subject)
	}

	// Verify the commit landed on the bare remote.
	out, err := exec.Command("git", "--git-dir="+bareRepo, "log", "--oneline").Output()
	if err != nil {
		t.Fatalf("git log on bare: %v", err)
	}
	if !strings.Contains(string(out), "test task title") {
		t.Fatalf("expected bare repo log to contain task title, got: %s", string(out))
	}
}
