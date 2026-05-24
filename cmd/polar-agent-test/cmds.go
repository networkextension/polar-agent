package main

// Subcommand implementations: ping / list / run / scenario / all.

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"
)

func runPing(addr string, args []string) int {
	fs := flag.NewFlagSet("ping", flag.ContinueOnError)
	a := flagAddr(addr, fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := dial(ctx, *a)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ping: %v\n", err)
		return 1
	}
	defer c.Close()
	if err := c.send(envelope{"kind": "ping"}); err != nil {
		fmt.Fprintf(os.Stderr, "ping send: %v\n", err)
		return 1
	}
	pong, err := c.waitFor(3*time.Second, "pong")
	if err != nil {
		fmt.Fprintf(os.Stderr, "ping: %v\n", err)
		return 1
	}
	j, _ := json.MarshalIndent(pong, "", "  ")
	fmt.Println(string(j))
	return 0
}

func runList(addr string, args []string) int {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	a := flagAddr(addr, fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := dial(ctx, *a)
	if err != nil {
		fmt.Fprintf(os.Stderr, "list: %v\n", err)
		return 1
	}
	defer c.Close()
	if err := c.send(envelope{"kind": "list"}); err != nil {
		fmt.Fprintf(os.Stderr, "list send: %v\n", err)
		return 1
	}
	resp, err := c.waitFor(3*time.Second, "skills")
	if err != nil {
		fmt.Fprintf(os.Stderr, "list: %v\n", err)
		return 1
	}
	j, _ := json.MarshalIndent(resp, "", "  ")
	fmt.Println(string(j))
	return 0
}

func runSkill(addr string, args []string) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	a := flagAddr(addr, fs)
	config := fs.String("config", "{}", "skill config JSON")
	stdin := fs.String("stdin", "", "bytes to send as skill.stdin after start")
	waitSecs := fs.Int("wait-secs", 5, "wait for exit (seconds); -1 = stream until ctrl-c")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "usage: polar-agent-test run <skill_kind> [--config <json>] [--stdin <text>] [--wait-secs <n>]")
		return 2
	}
	kind := fs.Arg(0)
	var cfg json.RawMessage = []byte(*config)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	c, err := dial(ctx, *a)
	if err != nil {
		fmt.Fprintf(os.Stderr, "run: %v\n", err)
		return 1
	}
	defer c.Close()
	const runID = 1
	if err := c.send(envelope{
		"kind":       "skill.start",
		"run_id":     runID,
		"skill_kind": kind,
		"config":     cfg,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "run start: %v\n", err)
		return 1
	}
	if *stdin != "" {
		// Tiny wait so the run is registered before stdin lands.
		time.Sleep(100 * time.Millisecond)
		if err := c.send(envelope{
			"kind":      "skill.stdin",
			"run_id":    runID,
			"bytes_b64": encodeBytes([]byte(*stdin)),
		}); err != nil {
			fmt.Fprintf(os.Stderr, "run stdin: %v\n", err)
			return 1
		}
	}
	waitDur := time.Duration(*waitSecs) * time.Second
	if *waitSecs < 0 {
		waitDur = 24 * time.Hour
	}
	ev, stdoutBytes, err := c.collectSkillEvent(runID, waitDur, "exit")
	fmt.Println("--- stdout ---")
	fmt.Println(string(stdoutBytes))
	if err != nil {
		fmt.Fprintf(os.Stderr, "--- error ---\n%v\n", err)
		return 1
	}
	j, _ := json.MarshalIndent(ev, "", "  ")
	fmt.Println("--- exit event ---")
	fmt.Println(string(j))
	return 0
}

func runScenarioCmd(addr string, args []string) int {
	fs := flag.NewFlagSet("scenario", flag.ContinueOnError)
	a := flagAddr(addr, fs)
	listFlag := fs.Bool("list", false, "list available scenarios and exit")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *listFlag {
		for _, s := range scenarios {
			fmt.Printf("  %-20s — %s\n", s.name, s.desc)
		}
		return 0
	}
	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "usage: polar-agent-test scenario <name>  (or --list)")
		return 2
	}
	name := fs.Arg(0)
	for _, s := range scenarios {
		if s.name == name {
			return runOneScenario(*a, s)
		}
	}
	fmt.Fprintf(os.Stderr, "unknown scenario: %s\n", name)
	return 2
}

func runAllScenarios(addr string, args []string) int {
	fs := flag.NewFlagSet("all", flag.ContinueOnError)
	a := flagAddr(addr, fs)
	skip := fs.String("skip", "", "comma-separated scenario names to skip")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	skipSet := map[string]bool{}
	for _, s := range strings.Split(*skip, ",") {
		if s = strings.TrimSpace(s); s != "" {
			skipSet[s] = true
		}
	}
	failed := 0
	for _, s := range scenarios {
		if skipSet[s.name] {
			fmt.Printf("[SKIP] %s — %s\n", s.name, s.desc)
			continue
		}
		if code := runOneScenario(*a, s); code != 0 {
			failed++
		}
	}
	if failed > 0 {
		fmt.Fprintf(os.Stderr, "\n%d scenario(s) FAILED\n", failed)
		return 1
	}
	fmt.Println("\nALL PASS")
	return 0
}

func runOneScenario(addr string, s scenario) int {
	fmt.Printf("[ RUN] %s — %s\n", s.name, s.desc)
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	c, err := dial(ctx, addr)
	if err != nil {
		fmt.Printf("[FAIL] %s: dial: %v (%.1fs)\n", s.name, err, time.Since(start).Seconds())
		return 1
	}
	defer c.Close()
	if err := s.run(ctx, c); err != nil {
		fmt.Printf("[FAIL] %s: %v (%.1fs)\n", s.name, err, time.Since(start).Seconds())
		return 1
	}
	fmt.Printf("[PASS] %s (%.1fs)\n", s.name, time.Since(start).Seconds())
	return 0
}
