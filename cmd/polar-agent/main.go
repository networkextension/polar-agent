// polar-agent is the local-side CLI that pairs a developer's
// machine with a Polar bot user, then receives tool-call messages
// over WebSocket and executes them in a pinned working directory.
//
// Three commands:
//
//   polar-agent login   [--server=https://polar.example.com] --token=<raw>
//       (--server defaults to https://zen.4950.store:2443 — override
//        at build time with -ldflags "-X main.defaultServer=<url>")
//   polar-agent status
//   polar-agent attach  --bot=<bot_user_id> --workdir=.
//
// Config lives at ~/.polar/agent.toml after login. attach is the
// long-running mode; everything else is one-shot.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/networkextension/polar-agent/cmd/polar-agent/skills"
)

// fetchBotPreferredTool asks the dock what the bot's stored
// preferred_tool is. Used by `attach --tool=auto` so the operator
// running the docker image doesn't have to set POLAR_BOT_TOOLS
// per bot — the wizard set the bot's preferred_tool already.
//
// Returns "" + nil error when the bot has no preferred_tool set
// (legitimate signal: tool-loop mode). Real network/auth errors
// bubble up.
func fetchBotPreferredTool(cfg AgentConfig, botID string) (string, error) {
	url := strings.TrimRight(cfg.Server, "/") + "/api/agent/bots/" + botID + "/config"
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("dock returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out struct {
		PreferredTool string `json:"preferred_tool"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("decode bot config: %w", err)
	}
	return strings.TrimSpace(out.PreferredTool), nil
}

// autoRegisteredBot is one row returned by /api/agent/auto-register.
type autoRegisteredBot struct {
	BotUserID     string `json:"bot_user_id"`
	Name          string `json:"name"`
	PreferredTool string `json:"preferred_tool"`
}

// callAutoRegister POSTs to /api/agent/auto-register declaring
// this agent's capabilities (coder list + research flag). Server
// creates / reuses bot_users rows owned by the agent token's user.
// Returns the bot ids paired with their preferred_tool so the
// entrypoint can spawn one `polar-agent attach --tool=<coder>` per
// coder bot, or tool-loop attach (no --tool) for the research bot.
func callAutoRegister(cfg AgentConfig, coders []string, research bool, agentName string) ([]autoRegisteredBot, error) {
	body, _ := json.Marshal(map[string]any{
		"coders":     coders,
		"research":   research,
		"agent_name": agentName,
	})
	url := strings.TrimRight(cfg.Server, "/") + "/api/agent/auto-register"
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("auto-register status %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	var out struct {
		Bots []autoRegisteredBot `json:"bots"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return nil, err
	}
	return out.Bots, nil
}

const (
	exitOK         = 0
	exitUsage      = 2
	exitConfig     = 3
	exitNet        = 4
	exitAuth       = 5
	exitToolFailed = 6
)

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(exitUsage)
	}
	cmd := os.Args[1]
	args := os.Args[2:]
	switch cmd {
	case "login":
		os.Exit(runLogin(args))
	case "status":
		os.Exit(runStatus(args))
	case "attach":
		os.Exit(runAttach(args))
	case "auto-register":
		os.Exit(runAutoRegister(args))
	case "register":
		// Host module register — one-shot consume-enroll-token flow.
		os.Exit(runRegister(args))
	case "self-test":
		os.Exit(runSelfTest(args))
	case "submit-build":
		os.Exit(runSubmitBuild(args))
	case "research":
		if err := runResearchREPL(args); err != nil {
			fmt.Fprintf(os.Stderr, "research: %v\n", err)
			os.Exit(exitToolFailed)
		}
	case "test-serve":
		// Standalone skill testbed. NO dock connection. NO auth.
		// Loopback TCP socket speaking the same envelope JSON as
		// the dock↔agent WS. See doc/agent/agent-test-harness.md.
		os.Exit(runTestServer(args))
	case "help", "-h", "--help":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", cmd)
		printUsage()
		os.Exit(exitUsage)
	}
}

func printUsage() {
	fmt.Fprintln(os.Stderr, `polar-agent — local executor for Polar bot users

Usage:
  polar-agent login    [--server=<url>] --token=<raw>     # write ~/.polar/agent.toml from an existing agent_token
                                                          # --server defaults to https://zen.4950.store:2443
  polar-agent register [--server=<url>] --token=<enroll>  # consume one-time enroll token from /hosts.html; auto-saves agent.toml
                                                          # --server defaults to https://zen.4950.store:2443
                                                          # add --start to immediately exec attach
  polar-agent self-test                                 # smoke: WS handshake + skill.advertise round-trip against the registered host
  polar-agent status                                    # show config + last verify
  polar-agent attach  --bot=<bot_id> --workdir=<path>             # tool-call loop mode
  polar-agent attach  --bot=<bot_id> --workdir=<path> --tool=auto  # ask dock for bot's preferred_tool
  polar-agent attach  --bot=<bot_id> --workdir=<path> --tool=kimi  # passthrough to local kimi-cli
  polar-agent attach  --bot=<bot_id> --workdir=<path> --tool=claude
  polar-agent attach  --bot=<bot_id> --workdir=<path> --tool=codex
  polar-agent submit-build <ipa> [--project=<id>] [--sign-method=auto|zsign|codesign]
                                                  # iOS: sign + upload IPA to TestFlight
  polar-agent research --workdir=<path> --llm-base-url=<url> --llm-api-key=<key> --llm-model=<id>
                       [--llm-name=<name>] [--git-remote=<url>] [--prompt=<text>] [--verbose]
                                                  # local stdin REPL: run the same Aider-style
                                                  # tool-loop without dock involvement

Tool whitelist (executed in workdir, default tool-call mode):
  read_file   — args: { path }
  write_file  — args: { path, content }
  list_dir    — args: { path }
  run_cmd     — args: { cmd, args[], stdin }   # cmd in {git, swift, xcodebuild, xcrun, ls, cat, find, grep}

--tool=<name> passthrough mode: chat content forwarded to the named local tool.
              Built-ins: kimi (kimi-cli), claude, codex. Override or add via
              ~/.polar/tools.json (template at cmd/polar-agent/tools.example.json).
              Platform LLM is bypassed; the local tool IS the agent.

--kimi (legacy alias for --tool=kimi).

Config file: ~/.polar/agent.toml
  server = "https://polar.example.com"
  token  = "polar_agent_..."`)
}

func runLogin(args []string) int {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	server := fs.String("server", defaultServer, "Polar server base URL (default: "+defaultServer+")")
	tokenF := fs.String("token", "", "raw agent token from /api/agent/tokens")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	// server has a default now (see defaults.go) — only token is truly
	// required. Keep the empty-server guard as belt-and-suspenders in
	// case a downstream build overrides defaultServer to "".
	if *server == "" || *tokenF == "" {
		fmt.Fprintln(os.Stderr, "--token is required (--server defaults to "+defaultServer+")")
		return exitUsage
	}
	cfg := AgentConfig{Server: *server, Token: *tokenF}
	if err := cfg.Save(); err != nil {
		fmt.Fprintf(os.Stderr, "failed to save config: %v\n", err)
		return exitConfig
	}
	if err := verifyToken(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "warning: token verify failed (config saved anyway): %v\n", err)
		return exitAuth
	}
	fmt.Printf("logged in. config saved to %s\n", configPath())
	return exitOK
}

func runStatus(args []string) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	cfg, err := LoadAgentConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "no config (run `polar-agent login` first): %v\n", err)
		return exitConfig
	}
	fmt.Printf("server : %s\n", cfg.Server)
	fmt.Printf("config : %s\n", configPath())
	if err := verifyToken(cfg); err != nil {
		fmt.Printf("status : invalid (%v)\n", err)
		return exitAuth
	}
	fmt.Println("status : ok")
	return exitOK
}

func runAttach(args []string) int {
	fs := flag.NewFlagSet("attach", flag.ContinueOnError)
	bot := fs.String("bot", "", "bot_user_id to attach to")
	workdir := fs.String("workdir", ".", "local working directory the bot may operate on")
	verbose := fs.Bool("verbose", false, "log each tool call")
	tool := fs.String("tool", "", "passthrough tool name (kimi/claude/codex/<custom>); empty = tool-call loop")
	kimi := fs.Bool("kimi", false, "legacy alias for --tool=kimi")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *bot == "" {
		fmt.Fprintln(os.Stderr, "--bot=<bot_user_id> is required")
		return exitUsage
	}
	if *kimi && *tool == "" {
		*tool = "kimi"
	}

	cfg, err := LoadAgentConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "no config (run `polar-agent login` first): %v\n", err)
		return exitConfig
	}

	// --tool=auto: ask the dock what this bot's preferred_tool is.
	// Empty preferred_tool → fall back to tool-loop mode (no --tool).
	// Non-empty → use that as the passthrough tool name.
	if strings.EqualFold(strings.TrimSpace(*tool), "auto") {
		resolvedTool, ferr := fetchBotPreferredTool(cfg, *bot)
		if ferr != nil {
			fmt.Fprintf(os.Stderr, "warning: --tool=auto fetch failed (%v); falling back to tool-loop mode\n", ferr)
			*tool = ""
		} else {
			log.Printf("tool=auto resolved to %q for bot=%s", resolvedTool, *bot)
			*tool = resolvedTool
		}
	}

	resolved, err := resolveWorkdir(*workdir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "workdir invalid: %v\n", err)
		return exitConfig
	}

	// Resolve tool spec up front so we fail fast on bad name.
	var spec *toolSpec
	if *tool != "" {
		specs, _, lerr := loadToolsConfig()
		if lerr != nil {
			fmt.Fprintf(os.Stderr, "tools config invalid: %v\n", lerr)
			return exitConfig
		}
		s, ferr := findToolSpec(specs, *tool)
		if ferr != nil {
			fmt.Fprintf(os.Stderr, "%v\n", ferr)
			return exitConfig
		}
		spec = s
	}

	log.SetFlags(log.Ldate | log.Ltime)
	mode := "tool-loop"
	if spec != nil {
		mode = "passthrough(" + spec.Name + ")"
	}
	log.Printf("attach bot=%s workdir=%s server=%s mode=%s", *bot, resolved, cfg.Server, mode)

	// Register the coder skill so the WS handshake's skill.advertise
	// payload reports the real tool list, AND so skill.start envelopes
	// (P1c) can be dispatched into invokeTool. The runner closure
	// captures the resolved tool specs so the skills package stays
	// free of toolSpec / invokeTool dependencies.
	specs, _, _ := loadToolsConfig()
	toolNames := make([]string, 0, len(specs))
	for _, s := range specs {
		toolNames = append(toolNames, s.Name)
	}
	coderCfg := skills.CoderConfig{
		Mode:    "tool-loop",
		Tools:   toolNames,
		Workdir: resolved,
	}
	if spec != nil {
		coderCfg.Mode = "passthrough"
		coderCfg.Tool = spec.Name
	}
	runner := makeCoderRunner(specs, *verbose)
	skills.Register(skills.NewCoderSkill(coderCfg, runner))

	// P2: Shell skill. Unix-only (NewShellSkill returns nil on
	// Windows). Operator can disable per-host via
	// POLAR_AGENT_SHELL_DISABLED=true even if the agent is unix.
	if os.Getenv("POLAR_AGENT_SHELL_DISABLED") != "true" {
		if shellSk := skills.NewShellSkill(resolved); shellSk != nil {
			skills.Register(shellSk)
		}
	}

	// P3: Proxy skill (sing-box). Unix-only. Operator can disable
	// via POLAR_AGENT_PROXY_DISABLED=true. The capability advertise
	// includes installed:true/false depending on whether sing-box is
	// on PATH; dock-side UI uses that to render an install hint if
	// the binary is missing.
	if os.Getenv("POLAR_AGENT_PROXY_DISABLED") != "true" {
		if proxySk := skills.NewProxySkill(); proxySk != nil {
			skills.Register(proxySk)
		}
	}

	// P4: WireGuard skill (wg-quick). Unix-only. Needs root to
	// manage the kernel interface — see comments in
	// cmd/polar-agent/skills/wireguard.go for privilege model.
	// Disable via POLAR_AGENT_WIREGUARD_DISABLED=true.
	if os.Getenv("POLAR_AGENT_WIREGUARD_DISABLED") != "true" {
		if wgSk := skills.NewWireGuardSkill(); wgSk != nil {
			skills.Register(wgSk)
		}
	}

	// P5: KDP skill (serial console with ? command routing).
	// Unix-only. Needs read/write on a serial device, typically
	// /dev/tty.usbserial-* or /dev/ttyUSB0. Disable via
	// POLAR_AGENT_KDP_DISABLED=true.
	if os.Getenv("POLAR_AGENT_KDP_DISABLED") != "true" {
		if nkSk := skills.NewKDPSkill(); nkSk != nil {
			skills.Register(nkSk)
		}
	}

	// P-recovery-0a: background watcher for iOS devices in DFU /
	// recovery mode. Polls `irecovery -q`; on detect → POST to
	// /api/agent/library/devices/upsert. Zero operator config: silent
	// no-op if irecovery isn't on PATH. sync.Once guards re-entry
	// when multiple `polar-agent attach` processes run.
	// Disable via POLAR_AGENT_RECOVERY_DISABLED=true.
	if os.Getenv("POLAR_AGENT_RECOVERY_DISABLED") != "true" {
		startRecoveryWatcher(cfg)
	}

	// P-library-0b: tell the skills package about the agent's dock
	// (server, token). The `library` MCP adapter reads from this at
	// Start time. Done unconditionally; if the adapter isn't loaded
	// in the operator's skill config it doesn't matter.
	skills.SetLibraryAdapterConfig(cfg.Server, cfg.Token)

	// VNC skill (B.1 plain TCP relay). Unix-only — primarily targets
	// macOS Screen Sharing on the agent host (loopback to :5900) but
	// any LAN-reachable VNC server works. Authentication is end-to-end
	// in the browser via noVNC; the agent is a transparent byte pipe.
	// Disable via POLAR_AGENT_VNC_DISABLED=true.
	if os.Getenv("POLAR_AGENT_VNC_DISABLED") != "true" {
		if vncSk := skills.NewVncSkill(); vncSk != nil {
			skills.Register(vncSk)
		}
	}

	// P3 (MCP-server): in-process JSON-RPC dispatcher over the skill's
	// stdin/stdout. P0a ships scaffold + built-in echo tool only;
	// gdb-mi / lldb-dap / ida-rpc adapters land in P0b+. Disable via
	// POLAR_AGENT_MCP_DISABLED=true.
	if os.Getenv("POLAR_AGENT_MCP_DISABLED") != "true" {
		skills.Register(skills.NewMCPServerSkill())
	}

	// Bundle skill: dispatcher for dock-managed declarative skill
	// bundles (.skill ZIPs under ~/.polar/bundles/). Unlike the other
	// kinds, one registration handles many bundles — name+version are
	// in the skill.start config. POLAR_AGENT_BUNDLES_DIR overrides the
	// install root (tests + multi-tenant setups). Unix-only; the stub
	// returns nil on Windows.
	if os.Getenv("POLAR_AGENT_BUNDLES_DISABLED") != "true" {
		if bundleSk := skills.NewBundleSkill(os.Getenv("POLAR_AGENT_BUNDLES_DIR")); bundleSk != nil {
			skills.Register(bundleSk)
		}
	}

	if err := runAgentLoop(cfg, *bot, resolved, *verbose, spec); err != nil {
		log.Printf("agent loop ended: %v", err)
		return exitNet
	}
	return exitOK
}

// runAutoRegister implements `polar-agent auto-register`. Output
// format: one line per bot, "bot_id<TAB>preferred_tool", with empty
// preferred_tool meaning the row is a research bot (tool-loop mode,
// no --tool= when attaching).
//
// Flags:
//
//	--coders=kimi,claude,codex  comma-separated coder names (default all 3)
//	--research                  also register a research bot (default on)
//	--no-research               skip research bot
//	--agent-name=<str>          display name (default = hostname)
func runAutoRegister(args []string) int {
	fs := flag.NewFlagSet("auto-register", flag.ContinueOnError)
	coders := fs.String("coders", "kimi,claude,codex", "comma-separated coder names")
	research := fs.Bool("research", true, "also register a research bot (uses workspace default LLM + agent tools)")
	noResearch := fs.Bool("no-research", false, "skip research bot (overrides --research)")
	agentName := fs.String("agent-name", "", "display name (optional; from container hostname)")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *noResearch {
		*research = false
	}
	cfg, err := LoadAgentConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "no config (run `polar-agent login` first): %v\n", err)
		return exitConfig
	}
	list := []string{}
	for _, c := range strings.Split(*coders, ",") {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		list = append(list, c)
	}
	if len(list) == 0 && !*research {
		fmt.Fprintln(os.Stderr, "nothing to register: no coders + research disabled")
		return exitUsage
	}
	if *agentName == "" {
		if h, err := os.Hostname(); err == nil {
			*agentName = h
		}
	}
	bots, err := callAutoRegister(cfg, list, *research, *agentName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "auto-register failed: %v\n", err)
		return exitNet
	}
	for _, b := range bots {
		fmt.Printf("%s\t%s\n", b.BotUserID, b.PreferredTool)
	}
	return exitOK
}
