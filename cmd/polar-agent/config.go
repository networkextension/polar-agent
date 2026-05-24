package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// AgentConfig is the on-disk state of `polar-agent login`. TOML-ish
// flat key/value (no sections; tiny). We avoid pulling a real TOML
// dependency for a 2-key file.
type AgentConfig struct {
	Server string
	Token  string
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
	body := fmt.Sprintf("server = %q\ntoken = %q\n", c.Server, c.Token)
	return os.WriteFile(path, []byte(body), 0o600)
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
