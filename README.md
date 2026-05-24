# polar-agent

Local executor for [Polar](https://github.com/networkextension/Polar) bots. Runs on a developer / operator machine, holds a long-lived WebSocket to dock, and executes bot-issued tool calls (shell, MCP, iOS sign, etc.) in a configured working directory.

Two binaries:

| binary | what |
|---|---|
| `polar-agent` | the runtime — `attach`, `register`, `submit-build`, `research`, `self-test`, `status` subcommands |
| `polar-agent-test` | standalone test harness — runs the same `attach` loop against a fake dock for CI-style smoke testing |

## Install

```bash
go install github.com/networkextension/polar-agent/cmd/polar-agent@latest
go install github.com/networkextension/polar-agent/cmd/polar-agent-test@latest
```

Cross-platform pre-built tarballs ship in the [umbrella release bundle](https://github.com/networkextension/Polar/releases).

## Quick start (operator side)

```bash
# 1. Mint an enroll token from /hosts.html on your dock host.
# 2. Register this machine:
polar-agent register --server=https://zen.example.com --token=<enroll>

# 3. Attach a bot to a working directory:
polar-agent attach --bot=bot_<id> --workdir=/path/to/repo --tool=claude

# Smoke check:
polar-agent self-test
```

`agent.toml` lives at `~/.polar/agent.toml` after register; subsequent commands read server URL + agent_token from there.

## Subcommands

```
polar-agent login    --server=<url> --token=<raw>     # write agent.toml from an existing agent_token
polar-agent register --server=<url> --token=<enroll>  # consume one-time enroll token from /hosts.html
                                                      # add --start to immediately exec attach
polar-agent self-test                                 # WS handshake + skill.advertise smoke
polar-agent status                                    # print config + last verify
polar-agent attach   --bot=<id> --workdir=<path> [--tool=auto|kimi|claude|codex]
polar-agent submit-build <ipa> [--project=<id>] [--sign-method=auto|zsign|codesign]
polar-agent research --workdir=<path> --llm-base-url=<url> --llm-api-key=<key> --llm-model=<id>
```

See `polar-agent --help` for the full surface.

## How it connects to dock

WebSocket `/ws/agent` carries the tool-call protocol. On hello the agent sends `skill.advertise` advertising which skills (shell / mcp / vnc / iosdist / etc.) it compiled in. A 60-second timer re-sends the same advertise frame so the dock-side `last_seen_at` stays fresh for the UI.

All HTTP calls — register, self-test verify, submit-build — go through the same `server` URL configured in `agent.toml`.

## Build

```bash
git clone https://github.com/networkextension/polar-agent
cd polar-agent
CGO_ENABLED=0 go build ./cmd/polar-agent
go test ./...
```

Cross-compile for any OS/arch combo Go supports (the binary is statically linked, stdlib + 2 small deps: `gorilla/websocket`, `creack/pty`, plus `go.bug.st/serial` for the recovery watcher).

## License

UNLICENSED (internal to the networkextension org for now). License + open-source positioning aligns with Polar dock's release.
