// polar-agent-test — CLI driver for the agent test harness.
//
// Connects to `polar-agent test-serve` over TCP loopback, drives skills
// via NDJSON envelopes (same shape as dock↔agent), asserts outcomes.
// See doc/agent/agent-test-harness.md for the protocol + scope.
//
// Subcommands:
//   ping                          — liveness probe
//   list                          — enumerate registered skills
//   run <kind> [--config <json>]  — start a skill, stream events
//   scenario <name>               — run a named integration scenario
//   all                           — run every scenario, non-zero on first fail
//
// Default server addr: 127.0.0.1:7077 (or $POLAR_AGENT_TEST_ADDR).
package main

import (
	"flag"
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(2)
	}
	addr := os.Getenv("POLAR_AGENT_TEST_ADDR")
	if addr == "" {
		addr = "127.0.0.1:7077"
	}
	cmd := os.Args[1]
	args := os.Args[2:]
	switch cmd {
	case "ping":
		os.Exit(runPing(addr, args))
	case "list":
		os.Exit(runList(addr, args))
	case "run":
		os.Exit(runSkill(addr, args))
	case "scenario":
		os.Exit(runScenarioCmd(addr, args))
	case "all":
		os.Exit(runAllScenarios(addr, args))
	case "orchestrate":
		os.Exit(runOrchestrate(args))
	case "-h", "--help", "help":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand: %s\n", cmd)
		printUsage()
		os.Exit(2)
	}
}

func printUsage() {
	fmt.Fprintln(os.Stderr, `polar-agent-test — drives polar-agent test-serve

Usage:
  polar-agent-test ping
  polar-agent-test list
  polar-agent-test run <skill_kind> [--config <json>] [--stdin <text>] [--wait-secs <n>]
  polar-agent-test scenario <name>
  polar-agent-test all
  polar-agent-test orchestrate     # one-shot: spawn test-serve + run all + tail logs

Subcommands:
  ping         send {kind:"ping"}, expect pong
  list         send {kind:"list"}, print skills JSON
  run          start a skill with given config, stream events until exit or --wait-secs
  scenario     run a named built-in scenario (see -list-scenarios)
  all          run every scenario; exit non-zero on first failure
  orchestrate  build polar-agent + spawn test-serve subprocess + run scenarios
               + stream test-serve log inline + clean teardown. One command,
               no manual setup. Default for go-run-style dev loops.

Env:
  POLAR_AGENT_TEST_ADDR    bind to non-default test-serve (default 127.0.0.1:7077)`)
}

// flagAddr lets each subcommand override the default addr — useful for
// running against a test-serve on a non-default port.
func flagAddr(addr string, fs *flag.FlagSet) *string {
	return fs.String("addr", addr, "test-serve address")
}
