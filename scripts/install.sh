#!/bin/sh
# polar-agent one-shot installer — register + install as a boot service.
#
# Usage (operator gets the one-time enroll token from /hosts.html → 注册主机):
#
#   curl -fsSL https://hosts.4950.store/install.sh | \
#     sh -s -- --token=polar_agent_<enroll> --name="<host name>"
#
# or, if the script is already on the box:
#
#   sh install.sh --token=<enroll> --name="<host>" [--server=<url>]
#
# What it does, OS-aware (FreeBSD / Linux / macOS):
#   1. detect os/arch
#   2. fetch the polar-agent binary from the Polar release endpoint
#      (override with POLAR_AGENT_BIN=/path to use a local binary, e.g. on
#      boxes that already have one staged), install to ~/.local/bin
#   3. `polar-agent register` — consumes the enroll token, writes
#      ~/.polar/agent.toml (v4 auto-binds the agent's bot identity)
#   4. install + enable a boot service that runs `polar-agent attach`:
#        FreeBSD → /usr/local/etc/rc.d/polar_agent   (needs doas/root)
#        Linux   → /etc/systemd/system/polar-agent.service (needs sudo/doas/root)
#        macOS   → ~/Library/LaunchAgents/com.polar.agent.plist (per-user)
#
# Idempotent: re-running re-registers and reinstalls the service.
set -eu

SERVER="https://hosts.4950.store"
TOKEN=""
NAME=""
# Where to pull the binary from. Per-os/arch path is appended below.
RELEASE_BASE="${POLAR_RELEASE_BASE:-https://hosts.4950.store/dl/polar-agent}"

for arg in "$@"; do
  case "$arg" in
    --token=*)  TOKEN="${arg#*=}" ;;
    --name=*)   NAME="${arg#*=}" ;;
    --server=*) SERVER="${arg#*=}" ;;
    *) echo "unknown arg: $arg" >&2; exit 2 ;;
  esac
done

[ -n "$TOKEN" ] || { echo "ERROR: --token=<enroll> required (mint it on /hosts.html → 注册主机)" >&2; exit 2; }
[ -n "$NAME" ]  || NAME="$(hostname 2>/dev/null || echo polar-host)"

# ---- 1. detect os/arch ----
OS="$(uname -s)"
ARCH="$(uname -m)"
case "$ARCH" in
  x86_64|amd64) ARCH=amd64 ;;
  arm64|aarch64) ARCH=arm64 ;;
  *) echo "ERROR: unsupported arch $ARCH" >&2; exit 1 ;;
esac
case "$OS" in
  FreeBSD) GOOS=freebsd ;;
  Linux)   GOOS=linux ;;
  Darwin)  GOOS=darwin ;;
  *) echo "ERROR: unsupported OS $OS" >&2; exit 1 ;;
esac
echo ">> polar-agent installer — os=$GOOS arch=$ARCH name=\"$NAME\" server=$SERVER"

# ---- 2. acquire binary ----
BINDIR="$HOME/.local/bin"
BIN="$BINDIR/polar-agent"
mkdir -p "$BINDIR"
if [ -n "${POLAR_AGENT_BIN:-}" ]; then
  echo ">> using staged binary $POLAR_AGENT_BIN"
  cp "$POLAR_AGENT_BIN" "$BIN"
elif [ -x "$BIN" ] && [ "${POLAR_REUSE_BIN:-0}" = "1" ]; then
  echo ">> reusing existing $BIN (POLAR_REUSE_BIN=1)"
else
  URL="$RELEASE_BASE/$GOOS/$ARCH/polar-agent"
  echo ">> downloading $URL"
  if command -v curl >/dev/null 2>&1; then
    curl -fsSL "$URL" -o "$BIN.tmp"
  else
    fetch -q -o "$BIN.tmp" "$URL"   # FreeBSD base has fetch, not always curl
  fi
  mv "$BIN.tmp" "$BIN"
fi
chmod +x "$BIN"
echo ">> installed $("$BIN" status >/dev/null 2>&1; echo "$BIN")"

# ---- 3. register (consumes enroll token, writes ~/.polar/agent.toml) ----
echo ">> registering with $SERVER ..."
"$BIN" register --server="$SERVER" --token="$TOKEN" --name="$NAME"

# pick a root escalation tool for system services
SUDO=""
if [ "$(id -u)" != "0" ]; then
  if command -v doas >/dev/null 2>&1; then SUDO=doas
  elif command -v sudo >/dev/null 2>&1; then SUDO=sudo
  fi
fi

WORKDIR="$HOME"

# ---- 4. install the boot service ----
case "$GOOS" in
freebsd)
  RC=/usr/local/etc/rc.d/polar_agent
  echo ">> writing rc.d service $RC (via ${SUDO:-root})"
  TMP="$(mktemp)"
  cat > "$TMP" <<RC
#!/bin/sh
# PROVIDE: polar_agent
# REQUIRE: NETWORKING
# KEYWORD: shutdown
. /etc/rc.subr
name=polar_agent
rcvar=polar_agent_enable
load_rc_config \$name
: \${polar_agent_enable:=NO}
pidfile=/var/run/\${name}.pid
command=/usr/sbin/daemon
command_args="-P \${pidfile} -r -u $(id -un) -o /var/log/\${name}.log $BIN attach --workdir=$WORKDIR"
run_rc_command "\$1"
RC
  ${SUDO} install -m 0555 "$TMP" "$RC"
  rm -f "$TMP"
  ${SUDO} sysrc polar_agent_enable=YES >/dev/null
  ${SUDO} service polar_agent restart || ${SUDO} service polar_agent start
  echo ">> done. tail -f /var/log/polar_agent.log"
  ;;
linux)
  UNIT=/etc/systemd/system/polar-agent.service
  echo ">> writing systemd unit $UNIT (via ${SUDO:-root})"
  TMP="$(mktemp)"
  cat > "$TMP" <<UNIT
[Unit]
Description=Polar host agent
After=network-online.target
Wants=network-online.target
[Service]
ExecStart=$BIN attach --workdir=$WORKDIR
Restart=always
RestartSec=5
User=$(id -un)
[Install]
WantedBy=multi-user.target
UNIT
  ${SUDO} install -m 0644 "$TMP" "$UNIT"
  rm -f "$TMP"
  ${SUDO} systemctl daemon-reload
  ${SUDO} systemctl enable --now polar-agent
  echo ">> done. journalctl -u polar-agent -f"
  ;;
darwin)
  PLIST="$HOME/Library/LaunchAgents/com.polar.agent.plist"
  mkdir -p "$HOME/Library/LaunchAgents"
  echo ">> writing launchd agent $PLIST"
  cat > "$PLIST" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>com.polar.agent</string>
  <key>ProgramArguments</key>
  <array><string>$BIN</string><string>attach</string><string>--workdir=$WORKDIR</string></array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>StandardOutPath</key><string>$HOME/Library/Logs/polar-agent.log</string>
  <key>StandardErrorPath</key><string>$HOME/Library/Logs/polar-agent.log</string>
</dict></plist>
PLIST
  launchctl unload "$PLIST" 2>/dev/null || true
  launchctl load "$PLIST"
  echo ">> done. tail -f $HOME/Library/Logs/polar-agent.log"
  ;;
esac

echo ">> polar-agent installed + service enabled. It will auto-start on boot."
