#!/bin/sh
# polar-agent wrapper for FreeBSD rc.d (run as the agent user via su -m).
export HOME="$(getent passwd "$(id -un)" | cut -d: -f6)"
export PATH=/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin
export POLAR_AGENT_ROUTING_BASE="${POLAR_AGENT_ROUTING_BASE:-https://zen.4950.store:2443/api/routing}"
export POLAR_AGENT_FW_BASE="${POLAR_AGENT_FW_BASE:-https://zen.4950.store:2443/api/firewall}"
exec "$HOME/.local/bin/polar-agent" attach --workdir="$HOME"
