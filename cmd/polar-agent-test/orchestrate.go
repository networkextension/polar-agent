package main

// `polar-agent-test orchestrate` — one-shot dev loop.
//
// What it does:
//   1) Pick a free loopback port.
//   2) Build polar-agent into a temp file (uses go build cache; ~150ms
//      after the first time).
//   3) Spawn it as `polar-agent test-serve --addr 127.0.0.1:<port>`.
//   4) Stream the subprocess stderr inline with [agent] prefix so the
//      operator sees what the agent is doing in real time.
//   5) Poll ping until the agent is ready (≤ 5s).
//   6) Run the scenario suite (default: all, minus --skip).
//   7) Tear down — signal the subprocess process group, wait briefly,
//      SIGKILL if it doesn't exit.
//   8) Exit code = scenario result.
//
// SIGINT (Ctrl-C) propagates: the orchestrator catches it, tears down
// the subprocess cleanly, then exits non-zero.

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

func runOrchestrate(args []string) int {
	fs := flag.NewFlagSet("orchestrate", flag.ContinueOnError)
	skip := fs.String("skip", "", "comma-separated scenario names to skip (passed through to `all`)")
	only := fs.String("only", "", "comma-separated scenario names to run (alternative to all)")
	agentBin := fs.String("agent-bin", "", "use a prebuilt polar-agent at this path instead of `go build`")
	repoDir := fs.String("repo", "", "path to the polar repo root (default: cwd, walking up to find go.mod)")
	readyTimeout := fs.Duration("ready-timeout", 8*time.Second, "max wait for test-serve to come up")
	keepRunning := fs.Bool("keep-running", false, "after scenarios, leave test-serve running until Ctrl-C — for manual exploration")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	// Resolve repo dir — needed for `go build ./cmd/polar-agent`.
	repo, err := resolveRepoDir(*repoDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "orchestrate: %v\n", err)
		return 1
	}

	// Step 1: pick a free port.
	addr, err := pickFreeLoopbackAddr()
	if err != nil {
		fmt.Fprintf(os.Stderr, "orchestrate: pick port: %v\n", err)
		return 1
	}

	// Step 2: locate or build polar-agent.
	binPath := *agentBin
	if binPath == "" {
		fmt.Fprintln(os.Stderr, "[orchestrate] building polar-agent...")
		built, err := buildPolarAgent(repo)
		if err != nil {
			fmt.Fprintf(os.Stderr, "orchestrate: build: %v\n", err)
			return 1
		}
		defer os.Remove(built)
		binPath = built
	}
	fmt.Fprintf(os.Stderr, "[orchestrate] agent bin: %s\n", binPath)

	// Step 3-4: spawn subprocess + stream stderr.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cmd := exec.CommandContext(ctx, binPath, "test-serve", "--addr", addr)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Env = append(os.Environ(), "POLAR_AGENT_TEST_ADDR="+addr)

	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		fmt.Fprintf(os.Stderr, "orchestrate: pipe stderr: %v\n", err)
		return 1
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		fmt.Fprintf(os.Stderr, "orchestrate: pipe stdout: %v\n", err)
		return 1
	}

	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "orchestrate: start: %v\n", err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "[orchestrate] test-serve spawned pid=%d addr=%s\n", cmd.Process.Pid, addr)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); streamPrefixed(stderrPipe, "[agent] ") }()
	go func() { defer wg.Done(); streamPrefixed(stdoutPipe, "[agent] ") }()

	// Step 5: wait for ready.
	if err := waitTestServeReady(ctx, addr, *readyTimeout); err != nil {
		fmt.Fprintf(os.Stderr, "[orchestrate] test-serve not ready: %v\n", err)
		teardown(cmd, cancel, &wg)
		return 1
	}
	fmt.Fprintf(os.Stderr, "[orchestrate] test-serve ready\n")

	// Step 6: handle Ctrl-C cleanly.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	exitCh := make(chan int, 1)
	go func() {
		args := []string{}
		if *skip != "" {
			args = append(args, "--skip", *skip)
		}
		if *only != "" {
			args = append(args, "--only", *only)
		}
		_ = only
		code := runScenarioSet(addr, *only, *skip)
		exitCh <- code
	}()

	var code int
	select {
	case code = <-exitCh:
	case sig := <-sigCh:
		fmt.Fprintf(os.Stderr, "\n[orchestrate] caught %v — tearing down\n", sig)
		code = 130
	}

	if *keepRunning {
		fmt.Fprintf(os.Stderr, "[orchestrate] --keep-running: test-serve PID %d still up on %s; Ctrl-C to stop\n", cmd.Process.Pid, addr)
		<-sigCh
	}

	// Step 7: tear down.
	teardown(cmd, cancel, &wg)
	fmt.Fprintf(os.Stderr, "[orchestrate] done — exit code %d\n", code)
	return code
}

// runScenarioSet picks the right scenario subset and runs them.
// only/skip are mutually exclusive in spirit but both honored (only
// wins if non-empty).
func runScenarioSet(addr, only, skip string) int {
	if only != "" {
		picks := map[string]bool{}
		for _, n := range strings.Split(only, ",") {
			if n = strings.TrimSpace(n); n != "" {
				picks[n] = true
			}
		}
		failed := 0
		for _, s := range scenarios {
			if !picks[s.name] {
				continue
			}
			if c := runOneScenario(addr, s); c != 0 {
				failed++
			}
		}
		if failed > 0 {
			return 1
		}
		return 0
	}
	args := []string{}
	if skip != "" {
		args = append(args, "--skip", skip)
	}
	return runAllScenarios(addr, args)
}

func pickFreeLoopbackAddr() (string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr, nil
}

// resolveRepoDir starts at the user-supplied dir (or cwd) and walks
// up until it finds a go.mod. Returns the absolute path.
func resolveRepoDir(hint string) (string, error) {
	start := hint
	if start == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		start = cwd
	}
	abs, err := filepath.Abs(start)
	if err != nil {
		return "", err
	}
	dir := abs
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no go.mod found from %s upward", abs)
		}
		dir = parent
	}
}

func buildPolarAgent(repoDir string) (string, error) {
	tmp, err := os.CreateTemp("", "polar-agent-orchestrate-*")
	if err != nil {
		return "", err
	}
	out := tmp.Name()
	tmp.Close()
	cmd := exec.Command("go", "build", "-o", out, "./cmd/polar-agent")
	cmd.Dir = repoDir
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		os.Remove(out)
		return "", err
	}
	return out, nil
}

// streamPrefixed reads lines from r and writes them to stderr with
// the given prefix. Used to interleave subprocess log lines with our
// own output so the operator sees agent-side events in real time.
func streamPrefixed(r io.Reader, prefix string) {
	br := bufio.NewReader(r)
	for {
		line, err := br.ReadString('\n')
		if line != "" {
			fmt.Fprintf(os.Stderr, "%s%s", prefix, line)
		}
		if err != nil {
			return
		}
	}
}

func waitTestServeReady(ctx context.Context, addr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		dialCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
		c, err := dial(dialCtx, addr)
		cancel()
		if err != nil {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		// Send a ping; agent responds with pong when fully up.
		_ = c.send(envelope{"kind": "ping"})
		_, err = c.waitFor(500*time.Millisecond, "pong")
		c.Close()
		if err == nil {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return errors.New("timeout")
}

func teardown(cmd *exec.Cmd, cancel context.CancelFunc, wg *sync.WaitGroup) {
	if cmd.Process == nil {
		return
	}
	// Send SIGTERM to the whole process group (Setpgid ensures pgid =
	// cmd.Process.Pid). The subprocess is a single binary so this is
	// equivalent to killing the child directly, but pgroup-style
	// cleanup is the right pattern in case test-serve later forks.
	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err == nil {
		_ = syscall.Kill(-pgid, syscall.SIGTERM)
	} else {
		_ = cmd.Process.Signal(syscall.SIGTERM)
	}
	// Give it 2s to flush + close, then SIGKILL.
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	select {
	case <-exited:
	case <-time.After(2 * time.Second):
		if pgid > 0 {
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
		} else {
			_ = cmd.Process.Kill()
		}
		<-exited
	}
	cancel()
	// Drain the log goroutines briefly so any trailing log lines land
	// before we return — but bounded so a stuck reader doesn't hang us.
	doneWg := make(chan struct{})
	go func() { wg.Wait(); close(doneWg) }()
	select {
	case <-doneWg:
	case <-time.After(500 * time.Millisecond):
	}
}
