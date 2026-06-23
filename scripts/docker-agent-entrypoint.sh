#!/bin/sh
# polar-agent docker entrypoint. Two required env vars + a few
# optional ones; runs `polar-agent login` once, optionally
# auto-registers coder bots when POLAR_BOT_IDS is unset, then fans
# out to N concurrent `polar-agent attach` processes (one per bot
# id). Background processes; `wait` blocks the entrypoint so the
# container stays alive while any agent is running.
#
# Required:
#   POLAR_DOCK_URL          https://dock.example.com
#   POLAR_AGENT_TOKEN       polar_agent_xxxxxxxxxxxxxxxxxx
#
# Optional:
#   POLAR_BOT_IDS           bot_kimi,bot_claude  (comma-sep). When
#                           empty (or unset), the container calls
#                           /api/agent/auto-register and creates one
#                           bot per coder it carries — operator runs
#                           docker with zero bot management.
#   POLAR_BOT_TOOLS         kimi,claude          (same order/length
#                                                 as POLAR_BOT_IDS;
#                                                 empty / "_" entry
#                                                 = --tool=auto for
#                                                 that slot)
#   POLAR_AUTO_REGISTER_CODERS  default kimi,claude,codex (which
#                                                 coders to declare
#                                                 to dock)
#   POLAR_WORK_ROOT         default /work
#   POLAR_KIMI_TIMEOUT      default 30m  (per-coder wall clock)

set -eu

require() {
  if [ -z "$(eval "echo \${$1:-}")" ]; then
    echo "❌ env $1 required" >&2
    echo "   docker run -e POLAR_DOCK_URL=... -e POLAR_AGENT_TOKEN=... ..." >&2
    exit 1
  fi
}

require POLAR_DOCK_URL
require POLAR_AGENT_TOKEN

WORK_ROOT="${POLAR_WORK_ROOT:-/work}"
mkdir -p "$WORK_ROOT"

echo "→ polar-agent login → $POLAR_DOCK_URL"
polar-agent login --server="$POLAR_DOCK_URL" --token="$POLAR_AGENT_TOKEN"

# Auto-register flow: when POLAR_BOT_IDS is empty, ask dock to
# create / fetch one bot per coder this image carries. Output of
# `polar-agent auto-register` is "bot_id<TAB>coder" per line; we
# parse back into BOT_IDS + BOT_TOOLS in matched order.
if [ -z "${POLAR_BOT_IDS:-}" ]; then
  CODERS="${POLAR_AUTO_REGISTER_CODERS:-kimi,claude,codex}"
  RESEARCH_FLAG="--research"
  if [ "${POLAR_AUTO_REGISTER_RESEARCH:-true}" = "false" ]; then
    RESEARCH_FLAG="--no-research"
  fi
  echo "→ POLAR_BOT_IDS empty; auto-registering coders=$CODERS research=${POLAR_AUTO_REGISTER_RESEARCH:-true}"
  REG=$(polar-agent auto-register --coders="$CODERS" "$RESEARCH_FLAG" --agent-name="$(hostname)")
  if [ -z "$REG" ]; then
    echo "❌ auto-register returned no bots; aborting" >&2
    exit 1
  fi
  POLAR_BOT_IDS=$(printf '%s\n' "$REG" | awk '{print $1}' | paste -sd, -)
  # Per-line preferred_tool is "" for the research bot (tool-loop
  # mode); coder bots have kimi/claude/codex. paste preserves empty
  # entries as bare commas (",,") which the attach loop translates
  # to --tool=auto (which then resolves to "" for research bots).
  POLAR_BOT_TOOLS=$(printf '%s\n' "$REG" | awk '{print $2}' | paste -sd, -)
  echo "  registered: $POLAR_BOT_IDS"
fi

# Convert comma-separated env vars to whitespace-separated for `for` loops.
BOT_IDS=$(printf '%s' "$POLAR_BOT_IDS" | tr ',' ' ')
BOT_TOOLS=$(printf '%s' "${POLAR_BOT_TOOLS:-}" | tr ',' ' ')

# Iterate bot ids by index. Default each bot uses --tool=auto, which
# polar-agent resolves at attach time by querying the dock for that
# bot's preferred_tool. POLAR_BOT_TOOLS overrides per index ("_" or
# empty entry means "auto" for that slot).
set -- $BOT_IDS
i=0
for bot in "$@"; do
  i=$((i + 1))
  workdir="$WORK_ROOT/$bot"
  mkdir -p "$workdir"

  tool="auto"
  if [ -n "$BOT_TOOLS" ]; then
    override=$(printf '%s' "$BOT_TOOLS" | awk -v idx="$i" '{ print $idx }')
    if [ -n "$override" ] && [ "$override" != "_" ]; then
      tool="$override"
    fi
  fi

  echo "→ attach bot=$bot workdir=$workdir tool=$tool"
  polar-agent attach \
    --bot="$bot" \
    --workdir="$workdir" \
    --tool="$tool" \
    --verbose &
done

# Block forever (or until a child crashes — we let docker restart
# the container in that case via --restart=unless-stopped).
wait
