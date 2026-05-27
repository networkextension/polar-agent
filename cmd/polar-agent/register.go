package main

// `polar-agent register` — host registration. Two modes:
//
//   polar-agent register --local
//       Co-located bootstrap. Hits POST /api/hosts/local-bootstrap on
//       http://127.0.0.1:8080 (direct to dock — bypasses nginx). The
//       dock-side handler verifies the request came from loopback +
//       has no proxy headers, then mints a host + permanent
//       agent_token in one shot. No token paste, no admin login. Only
//       enabled when dock was started with POLAR_ALLOW_LOCAL_BOOTSTRAP=true.
//
//   polar-agent register [--server=<url>] --token=<enroll-token>
//       Remote / multi-machine path. The admin mints a one-time
//       enroll token via the dashboard (/hosts.html → Add Host →
//       copy); this CLI consumes it via the existing
//       /api/hosts/register endpoint and writes the resulting
//       permanent agent_token to ~/.polar/agent.toml. Unchanged from
//       the original Host module P0 flow.
//       --server defaults to the canonical control plane
//       (see defaults.go); forks can override at build time via
//       -ldflags "-X main.defaultServer=<url>".
//
// Both modes save agent.toml with the URL the agent should use for
// its subsequent WS attach. Local mode saves http://127.0.0.1:8080
// (direct dock) so attach also skips nginx.

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/networkextension/polar-agent/cmd/polar-agent/hostinfo"
)

const localBootstrapDefaultURL = "http://127.0.0.1:8080"

type registerRequest struct {
	Name     string `json:"name,omitempty"`
	HostOS   string `json:"host_os"`
	HostArch string `json:"host_arch"`
	// MachineUUID is the stable per-machine fingerprint dock uses to
	// dedup duplicate `hosts` rows on re-register (token expired,
	// agent reinstalled, IP changed). See hostinfo.HostInfo.MachineUUID
	// — empty when the collector failed; dock skips dedup in that case.
	MachineUUID string `json:"machine_uuid,omitempty"`
}

type registerResponse struct {
	Host          map[string]any `json:"host"`
	AgentTokenID  string         `json:"agent_token_id"`
	AgentTokenRaw string         `json:"agent_token_raw"`
	WorkspaceID   string         `json:"workspace_id"`
	Error         string         `json:"error"`
}

func runRegister(args []string) int {
	fs := flag.NewFlagSet("register", flag.ContinueOnError)
	server := fs.String("server", defaultServer, "Polar dock base URL (remote flow only; --local ignores this; default: "+defaultServer+")")
	token := fs.String("token", "", "one-time enroll token from /hosts.html Add Host (remote flow)")
	local := fs.Bool("local", false, "co-located bootstrap: hit dock on 127.0.0.1 directly, no token needed")
	localURL := fs.String("local-url", localBootstrapDefaultURL, "dock URL when --local (default http://127.0.0.1:8080)")
	name := fs.String("name", "", "host display name (default: os.Hostname())")
	start := fs.Bool("start", false, "after successful register, exec polar-agent attach in the foreground")
	verbose := fs.Bool("verbose", false, "log the request/response bodies")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	if *local && strings.TrimSpace(*token) != "" {
		fmt.Fprintln(os.Stderr, "--local and --token are mutually exclusive")
		return exitUsage
	}
	// server has a default now (see defaults.go) — only token is truly
	// required in remote mode. Keep the empty-server guard as
	// belt-and-suspenders for downstream builds that override
	// defaultServer to "".
	if !*local && (strings.TrimSpace(*server) == "" || strings.TrimSpace(*token) == "") {
		fmt.Fprintln(os.Stderr, "register requires either --local OR --token=<enroll>")
		fmt.Fprintln(os.Stderr, "  --local: co-located bootstrap (dock + agent same machine)")
		fmt.Fprintln(os.Stderr, "  --token: remote flow, get the enroll token from /hosts.html → Add Host")
		fmt.Fprintln(os.Stderr, "  --server is optional (defaults to "+defaultServer+")")
		return exitUsage
	}

	hostName := strings.TrimSpace(*name)
	if hostName == "" {
		if n, err := os.Hostname(); err == nil {
			hostName = n
		} else {
			hostName = "polar-agent"
		}
	}

	var (
		endpoint string
		authBear string
		saveURL  string
	)
	if *local {
		endpoint = strings.TrimRight(*localURL, "/") + "/api/hosts/local-bootstrap"
		saveURL = strings.TrimRight(*localURL, "/")
		// No bearer — loopback IS the auth.
	} else {
		endpoint = strings.TrimRight(*server, "/") + "/api/hosts/register"
		authBear = strings.TrimSpace(*token)
		saveURL = strings.TrimRight(*server, "/")
	}

	// Collect the stable machine fingerprint so dock can dedup the
	// hosts row across re-registers. hostinfo.Collect() is sync.Once
	// cached so this is cheap-ish; on darwin the first call execs
	// system_profiler (~600 ms) and ioreg (~30 ms), which is fine for
	// a one-shot CLI. The 2s timeouts inside each collector cap the
	// worst case; if a sysctl/ioreg hangs we ship MachineUUID="" and
	// dock falls back to legacy create. Either way we never block
	// register past a few seconds.
	hi := hostinfo.Collect()

	body, _ := json.Marshal(registerRequest{
		Name:        hostName,
		HostOS:      runtime.GOOS,
		HostArch:    runtime.GOARCH,
		MachineUUID: hi.MachineUUID,
	})
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		fmt.Fprintf(os.Stderr, "build request: %v\n", err)
		return exitNet
	}
	if authBear != "" {
		req.Header.Set("Authorization", "Bearer "+authBear)
	}
	req.Header.Set("Content-Type", "application/json")

	if *verbose {
		fmt.Fprintf(os.Stderr, "→ POST %s\n  body: %s\n", req.URL, string(body))
	}

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "request failed: %v\n", err)
		if *local {
			fmt.Fprintf(os.Stderr, "  is polar-dock running on %s? (try: pgrep -fl polar-dock)\n", *localURL)
		}
		return exitNet
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	if *verbose {
		fmt.Fprintf(os.Stderr, "← %d %s\n  body: %s\n", resp.StatusCode, resp.Status, string(respBody))
	}

	var parsed registerResponse
	_ = json.Unmarshal(respBody, &parsed)

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		msg := strings.TrimSpace(parsed.Error)
		if msg == "" {
			msg = strings.TrimSpace(string(respBody))
		}
		fmt.Fprintf(os.Stderr, "register failed (HTTP %d): %s\n", resp.StatusCode, msg)
		switch resp.StatusCode {
		case http.StatusForbidden:
			if *local {
				fmt.Fprintln(os.Stderr, "  enable on dock side by setting POLAR_ALLOW_LOCAL_BOOTSTRAP=true and restarting polar-dock,")
				fmt.Fprintln(os.Stderr, "  or check that you're hitting 127.0.0.1 directly (not via nginx/external).")
			}
		case http.StatusUnauthorized:
			fmt.Fprintln(os.Stderr, "  the enroll token is unknown — was it already used or did you typo it?")
		case http.StatusGone:
			fmt.Fprintln(os.Stderr, "  the enroll token expired (1h TTL). Re-mint via /hosts.html → Add Host.")
		case http.StatusConflict:
			fmt.Fprintln(os.Stderr, "  the enroll token was already consumed. Mint a fresh one.")
		}
		return exitNet
	}

	if strings.TrimSpace(parsed.AgentTokenRaw) == "" {
		fmt.Fprintf(os.Stderr, "register succeeded but response missing agent_token_raw: %s\n", string(respBody))
		return exitNet
	}

	cfg := AgentConfig{
		Server: saveURL,
		Token:  parsed.AgentTokenRaw,
	}
	if err := cfg.Save(); err != nil {
		fmt.Fprintf(os.Stderr, "saved register response but failed to write agent.toml: %v\n", err)
		return exitConfig
	}

	hostNameOut := ""
	if n, ok := parsed.Host["name"].(string); ok {
		hostNameOut = n
	}
	hostID := ""
	if id, ok := parsed.Host["id"].(string); ok {
		hostID = id
	}
	mode := "remote enroll"
	if *local {
		mode = "local bootstrap"
	}
	fmt.Printf("✓ %s: host=%s id=%s\n", mode, hostNameOut, hostID)
	fmt.Printf("✓ saved server=%s + agent_token to %s\n", saveURL, configPath())

	if *start {
		fmt.Println("→ exec polar-agent attach (tool-loop mode)")
		self, err := os.Executable()
		if err != nil {
			fmt.Fprintf(os.Stderr, "resolve self: %v\n", err)
			return exitConfig
		}
		if err := syscall.Exec(self, []string{self, "attach", "--workdir=."}, os.Environ()); err != nil {
			fmt.Fprintf(os.Stderr, "exec attach: %v\n", err)
			return exitNet
		}
	} else {
		fmt.Println("  next: polar-agent attach --bot=<bot_id> --workdir=<path> [--tool=<name>]")
	}
	return exitOK
}

// detectRunningAgent is a tiny helper used by the self-test: exposes
// whether a polar-agent attach process is already running for this
// user, so the test can decide whether to start one.
func detectRunningAgent() (pid int, ok bool) {
	cmd := exec.Command("pgrep", "-f", "polar-agent attach")
	out, err := cmd.Output()
	if err != nil {
		return 0, false
	}
	first := strings.SplitN(strings.TrimSpace(string(out)), "\n", 2)[0]
	if first == "" {
		return 0, false
	}
	var p int
	fmt.Sscanf(first, "%d", &p)
	if p <= 0 {
		return 0, false
	}
	return p, true
}
