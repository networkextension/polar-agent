package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// AgentConfig is the on-disk state of `polar-agent login` (and the
// post-v4 `polar-agent register` flow). TOML-ish flat key/value (no
// sections; tiny). We avoid pulling a real TOML dependency for a
// small file.
//
// v4 (see doc/arch/agent-identity-v4.md) adds AgentID + BotUserID:
//
//   - AgentID    — server-issued "ag_<32hex>" identity for this agent
//     instance. Persisted so reconnects / `attach` carry
//     it in the hello frame; absent on legacy installs
//     (pre-Phase E) and tolerated.
//   - BotUserID  — bot the operator wants `attach` to default to.
//     Written when the v4 register response includes it
//     (auto-create) and read by `attach` when --bot is
//     omitted. Absent → operator must pass --bot.
type AgentConfig struct {
	Server    string
	Token     string
	AgentID   string // v4: "ag_<32hex>"; empty on legacy installs
	BotUserID string // v4: default bot for `attach`; empty → require --bot
	// HostID is the server-issued host identity (host_id =
	// sha256(salt+machine_uuid)). The platform 下发s it in the register
	// response; we persist it so it survives agent restarts and rides in
	// every hello frame — which lets dock stamp wg_devices.host_id for the
	// WG↔Hosts UI cross-link (doc/arch/wg-host-crosslink.md) without
	// needing to read the WG public key (unreadable on wg-mac NE boxes).
	// Empty on legacy installs; self-healed on the next register.
	HostID string
}

func configPath() string {
	if v := strings.TrimSpace(os.Getenv("POLAR_AGENT_CONFIG")); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ".polar/agent.toml"
	}
	return filepath.Join(home, ".polar", "agent.toml")
}

func (c AgentConfig) Save() error {
	if strings.TrimSpace(c.Server) == "" || strings.TrimSpace(c.Token) == "" {
		return errors.New("server and token required")
	}
	path := configPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "server = %q\n", c.Server)
	fmt.Fprintf(&b, "token = %q\n", c.Token)
	// v4: omit empty so legacy installs round-trip clean and the file
	// stays minimal until the operator registers under the v4 server.
	if strings.TrimSpace(c.AgentID) != "" {
		fmt.Fprintf(&b, "agent_id = %q\n", c.AgentID)
	}
	if strings.TrimSpace(c.BotUserID) != "" {
		fmt.Fprintf(&b, "bot_user_id = %q\n", c.BotUserID)
	}
	if strings.TrimSpace(c.HostID) != "" {
		fmt.Fprintf(&b, "host_id = %q\n", c.HostID)
	}
	return os.WriteFile(path, []byte(b.String()), 0o600)
}

func LoadAgentConfig() (AgentConfig, error) {
	path := configPath()
	raw, err := os.ReadFile(path)
	if err != nil {
		return AgentConfig{}, err
	}
	var cfg AgentConfig
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		val := strings.Trim(strings.TrimSpace(line[eq+1:]), `"`)
		switch key {
		case "server":
			cfg.Server = val
		case "token":
			cfg.Token = val
		case "agent_id":
			cfg.AgentID = val
		case "bot_user_id":
			cfg.BotUserID = val
		case "host_id":
			cfg.HostID = val
		}
	}
	if cfg.Server == "" || cfg.Token == "" {
		return cfg, fmt.Errorf("config %s is missing server or token", path)
	}
	return cfg, nil
}

func resolveWorkdir(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s is not a directory", abs)
	}
	return abs, nil
}
