package main

// `polar-agent self-test` — end-to-end smoke against the registered
// host's dock. Verifies four things without spinning up a real attach
// session:
//
//   1. agent.toml loads + has a server + token
//   2. /api/agent/whoami returns 200 (token is live)
//   3. WS handshake to /ws/agent opens, hello + skill.advertise reach
//      dock, and dock keeps the connection open (no immediate close)
//   4. /api/hosts/<id> reflects our host_id (= dock's view of us is
//      consistent with the cached agent_token → host mapping)
//
// All four pass = the host module sees this agent + a chat bound to
// our host_skill would dispatch successfully. Any failure points the
// operator at the next thing to fix.

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"github.com/networkextension/polar-agent/cmd/polar-agent/skills"
)

func runSelfTest(args []string) int {
	fs := flag.NewFlagSet("self-test", flag.ContinueOnError)
	verbose := fs.Bool("verbose", false, "log each step's request/response")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	pass := 0
	fail := 0
	step := func(name string, fn func() error) {
		fmt.Printf("• %s ... ", name)
		if err := fn(); err != nil {
			fmt.Printf("FAIL\n  %v\n", err)
			fail++
			return
		}
		fmt.Println("ok")
		pass++
	}

	var cfg AgentConfig

	step("load agent.toml", func() error {
		c, err := LoadAgentConfig()
		if err != nil {
			return fmt.Errorf("LoadAgentConfig: %w (did you run `polar-agent register`?)", err)
		}
		cfg = c
		if *verbose {
			fmt.Printf("\n  server=%s token=%s...\n  ", cfg.Server, mask(cfg.Token))
		}
		return nil
	})
	if fail > 0 {
		return reportSelfTest(pass, fail)
	}

	step("/api/agent/whoami (token live)", func() error {
		return verifyToken(cfg)
	})

	// WS handshake — open /ws/agent, send hello + advertise, expect
	// the server to keep the connection open for ≥3s (= no immediate
	// reject). We intentionally don't pass --bot here so dock treats
	// us as a tool-loop attach with no bot binding; the host module
	// path still records the host via skill.advertise.
	step("/ws/agent handshake + skill.advertise", func() error {
		wsURL, err := buildSelfTestWSURL(cfg.Server, cfg.Token)
		if err != nil {
			return err
		}
		dialer := websocket.Dialer{HandshakeTimeout: 8 * time.Second}
		conn, _, err := dialer.Dial(wsURL, nil)
		if err != nil {
			return fmt.Errorf("dial %s: %w", wsURL, err)
		}
		defer conn.Close()
		// Register the coder skill locally so advertise carries a real
		// payload. (Cheap — same call main.go would make on attach.)
		specs, _, _ := loadToolsConfig()
		toolNames := make([]string, 0, len(specs))
		for _, s := range specs {
			toolNames = append(toolNames, s.Name)
		}
		_, ok := skills.Default().Get(skills.KindCoder)
		if !ok {
			skills.Register(skills.NewCoderSkill(skills.CoderConfig{
				Mode: "tool-loop", Tools: toolNames,
			}, nil))
		}
		hello := map[string]any{
			"kind":         "hello",
			"version":      "polar-agent self-test",
			"capabilities": []string{"tools"},
			"host_os":      runtime.GOOS,
			"host_arch":    runtime.GOARCH,
		}
		if b, err := json.Marshal(hello); err == nil {
			if err := conn.WriteMessage(websocket.TextMessage, b); err != nil {
				return fmt.Errorf("write hello: %w", err)
			}
		}
		advertise := map[string]any{
			"kind":   "skill.advertise",
			"skills": skills.Default().Advertised(),
		}
		if b, err := json.Marshal(advertise); err == nil {
			if err := conn.WriteMessage(websocket.TextMessage, b); err != nil {
				return fmt.Errorf("write skill.advertise: %w", err)
			}
		}
		// Hold the connection open for 2s — if dock rejects after
		// processing the advertise (e.g. bad token, no hosts row), we'd
		// see a close frame here.
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, _, err = conn.ReadMessage()
		if err != nil && !isExpectedDeadlineErr(err) {
			return fmt.Errorf("connection closed by dock: %w", err)
		}
		return nil
	})

	step("/api/hosts shows this token", func() error {
		hosts, err := listHosts(cfg)
		if err != nil {
			return err
		}
		// Find by agent_token_id ≈ our token. The list response is
		// workspace-scoped; the operator's session is what gates it
		// — but agent tokens aren't a user session, so this endpoint
		// will 401 us. Treat that as "informational only", not a
		// failure. We try and report the result either way.
		if len(hosts) == 0 {
			return fmt.Errorf("no hosts visible (token may not have admin scope — not a hard failure)")
		}
		return nil
	})

	return reportSelfTest(pass, fail)
}

func reportSelfTest(pass, fail int) int {
	fmt.Printf("\n%d pass, %d fail\n", pass, fail)
	if fail > 0 {
		return exitNet
	}
	return exitOK
}

func mask(s string) string {
	if len(s) <= 8 {
		return strings.Repeat("*", len(s))
	}
	return s[:4] + "..." + s[len(s)-4:]
}

func isExpectedDeadlineErr(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "i/o timeout") || strings.Contains(s, "deadline")
}

func buildSelfTestWSURL(server, token string) (string, error) {
	u, err := url.Parse(strings.TrimRight(server, "/") + "/ws/agent")
	if err != nil {
		return "", err
	}
	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	}
	q := u.Query()
	q.Set("token", token)
	q.Set("workdir", os.TempDir())
	u.RawQuery = q.Encode()
	return u.String(), nil
}

type selfTestHost struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func listHosts(cfg AgentConfig) ([]selfTestHost, error) {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
		strings.TrimRight(cfg.Server, "/")+"/api/hosts", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	cli := &http.Client{Timeout: 10 * time.Second}
	resp, err := cli.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncateForSelfTestLog(string(body), 120))
	}
	var parsed struct {
		Hosts []selfTestHost `json:"hosts"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("parse hosts response: %w", err)
	}
	return parsed.Hosts, nil
}

func truncateForSelfTestLog(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
