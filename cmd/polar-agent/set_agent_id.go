package main

// `polar-agent set-agent-id <ag_xxx> [<bot_xxx>]`
//
// Agent Identity v4 migration helper. The 4 hosts currently in
// production were registered under the v3 model and have an
// agent.toml that only carries `server` + `token`. v4 expects each
// agent to persist its server-issued `agent_id` (plus the
// auto-bound `bot_user_id`) so reconnects send it in the hello
// frame and `attach` no longer requires --bot.
//
// Operators run this once per box after the dock-side schema
// migration assigns each legacy agent_token its ag_<32hex> id:
//
//   ssh box.example.com 'polar-agent set-agent-id ag_a1b2c3...'
//   ssh box.example.com 'polar-agent set-agent-id ag_a1b2c3... bot_xxx'
//
// Strictly local: reads agent.toml, mutates AgentID (+ BotUserID),
// writes back. Never touches the network.
//
// See doc/arch/agent-identity-v4.md for the design.

import (
	"fmt"
	"os"
	"strings"
)

// validateAgentID enforces the `ag_<32hex>` shape the server mints.
// We're deliberately strict here so operators catch a paste error
// (e.g. truncated id, copied a bot_xxx instead) at the CLI rather
// than three hours later in a hello-frame mismatch.
func validateAgentID(id string) error {
	id = strings.TrimSpace(id)
	if !strings.HasPrefix(id, "ag_") {
		return fmt.Errorf("agent_id must start with ag_ (got %q)", id)
	}
	rest := id[len("ag_"):]
	if len(rest) != 32 {
		return fmt.Errorf("agent_id payload must be 32 hex chars (got %d in %q)", len(rest), id)
	}
	for _, r := range rest {
		switch {
		case r >= '0' && r <= '9':
		case r >= 'a' && r <= 'f':
		case r >= 'A' && r <= 'F':
		default:
			return fmt.Errorf("agent_id payload must be hex (offending char %q in %q)", r, id)
		}
	}
	return nil
}

// validateBotUserID is intentionally laxer than validateAgentID —
// dock mints bot ids with varying suffix shapes (alphanumeric, not
// strictly hex). We just sanity-check the prefix.
func validateBotUserID(id string) error {
	id = strings.TrimSpace(id)
	if !strings.HasPrefix(id, "bot_") {
		return fmt.Errorf("bot_user_id must start with bot_ (got %q)", id)
	}
	if len(id) <= len("bot_") {
		return fmt.Errorf("bot_user_id payload empty (got %q)", id)
	}
	return nil
}

func runSetAgentID(args []string) int {
	if len(args) == 0 || len(args) > 2 {
		fmt.Fprintln(os.Stderr, "usage: polar-agent set-agent-id <ag_xxx> [<bot_xxx>]")
		return exitUsage
	}
	agentID := strings.TrimSpace(args[0])
	if err := validateAgentID(agentID); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return exitUsage
	}
	var botUserID string
	if len(args) == 2 {
		botUserID = strings.TrimSpace(args[1])
		if err := validateBotUserID(botUserID); err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			return exitUsage
		}
	}

	cfg, err := LoadAgentConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "load %s: %v (run `polar-agent register` first)\n", configPath(), err)
		return exitConfig
	}

	changed := false
	if cfg.AgentID != agentID {
		cfg.AgentID = agentID
		changed = true
	}
	if botUserID != "" && cfg.BotUserID != botUserID {
		cfg.BotUserID = botUserID
		changed = true
	}

	if !changed {
		fmt.Printf("%s unchanged: agent_id=%s bot_user_id=%s\n",
			configPath(), cfg.AgentID, cfg.BotUserID)
		return exitOK
	}

	if err := cfg.Save(); err != nil {
		fmt.Fprintf(os.Stderr, "save %s: %v\n", configPath(), err)
		return exitConfig
	}

	botPart := cfg.BotUserID
	if botPart == "" {
		botPart = "(unchanged)"
	}
	fmt.Printf("%s updated: agent_id=%s bot_user_id=%s\n",
		configPath(), cfg.AgentID, botPart)
	return exitOK
}
