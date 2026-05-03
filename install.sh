#!/bin/sh
# Public installer for `denon`, a self-hosted vTuner replacement for Denon /
# Marantz / Yamaha / Onkyo / Pioneer AVRs whose internet-radio directory was
# discontinued.
#
# Quick install (downloads the latest release):
#   curl -fsSL https://raw.githubusercontent.com/victorantos/denon/main/install.sh | sh
#
# From a checked-out source tree (after `make build`):
#   ./install.sh --local
#
# Uninstall:
#   ./install.sh --uninstall
#
# Overrides:
#   BASE_URL=http://192.168.1.10 ./install.sh    pin the LAN IP explicitly
#   VERSION=v0.1.0 ./install.sh                  pick a specific release
#   REPO=fork/denon ./install.sh                 download from a fork

set -eu

REPO=${REPO:-victorantos/denon}
VERSION=${VERSION:-latest}
BASE_URL=${BASE_URL:-}
LABEL=com.denon.tuner
BIN=/usr/local/bin/denon
SYSTEMD_UNIT=/etc/systemd/system/denon.service
LAUNCHD_PLIST=/Library/LaunchDaemons/${LABEL}.plist

ACTION=install
LOCAL=0
for arg in "$@"; do
    case "$arg" in
        --local) LOCAL=1 ;;
        --uninstall) ACTION=uninstall ;;
        --version=*) VERSION=${arg#--version=} ;;
        --base-url=*) BASE_URL=${arg#--base-url=} ;;
        -h|--help)
            sed -n '2,/^$/p' "$0" | sed 's/^# \{0,1\}//'
            exit 0 ;;
        *)
            echo "unknown argument: $arg" >&2
            exit 2 ;;
    esac
done

die() { echo "error: $*" >&2; exit 1; }

detect_os() {
    case "$(uname -s)" in
        Darwin) echo darwin ;;
        Linux) echo linux ;;
        *) die "unsupported OS: $(uname -s) (only macOS and Linux right now)" ;;
    esac
}

detect_arch() {
    case "$(uname -m)" in
        arm64|aarch64) echo arm64 ;;
        x86_64|amd64) echo amd64 ;;
        *) die "unsupported architecture: $(uname -m)" ;;
    esac
}

# detect_lan_ip returns the IP that would be used to reach the public internet
# — i.e. the actual LAN IP, ignoring loopback, VPN tunnels, Docker bridges,
# and other interfaces the receiver can't reach. A static IP or DHCP
# reservation is recommended for permanence; this just picks the current one.
detect_lan_ip() {
    case "$(uname -s)" in
        Darwin)
            iface=$(route -n get 1.1.1.1 2>/dev/null | awk '/interface:/{print $2}')
            [ -n "$iface" ] && ipconfig getifaddr "$iface" 2>/dev/null
            ;;
        Linux)
            ip -4 route get 1.1.1.1 2>/dev/null | awk '{for(i=1;i<NF;i++) if($i=="src") {print $(i+1); exit}}'
            ;;
    esac
}

confirm() {
    printf '%s [y/N] ' "$1"
    read -r ans
    case "$ans" in y|Y|yes|YES) return 0 ;; *) return 1 ;; esac
}

require_cmd() {
    command -v "$1" >/dev/null 2>&1 || die "missing required command: $1"
}

install_launchd() {
    sudo tee "$LAUNCHD_PLIST" >/dev/null <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>${LABEL}</string>
  <key>ProgramArguments</key>
  <array>
    <string>${BIN}</string>
    <string>-http</string><string>:80</string>
    <string>-dns</string><string>:53</string>
    <string>-base-url</string><string>${BASE_URL}</string>
    <string>-intercept-ip</string><string>${INTERCEPT_IP}</string>
  </array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>StandardOutPath</key><string>/var/log/denon.out.log</string>
  <key>StandardErrorPath</key><string>/var/log/denon.err.log</string>
  <key>SoftResourceLimits</key>
  <dict><key>NumberOfFiles</key><integer>4096</integer></dict>
</dict>
</plist>
EOF
    sudo chmod 644 "$LAUNCHD_PLIST"
    sudo launchctl bootout "system/$LABEL" 2>/dev/null || true
    sudo launchctl bootstrap system "$LAUNCHD_PLIST"
}

install_systemd() {
    sudo tee "$SYSTEMD_UNIT" >/dev/null <<EOF
[Unit]
Description=denon — self-hosted vTuner replacement
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=${BIN} -http :80 -dns :53 -base-url ${BASE_URL} -intercept-ip ${INTERCEPT_IP}
Restart=on-failure
RestartSec=5
# Bind privileged ports without running as full root:
AmbientCapabilities=CAP_NET_BIND_SERVICE
CapabilityBoundingSet=CAP_NET_BIND_SERVICE
NoNewPrivileges=yes
DynamicUser=yes
ProtectSystem=strict
ProtectHome=yes

[Install]
WantedBy=multi-user.target
EOF
    sudo systemctl daemon-reload
    sudo systemctl enable --now denon.service
}

uninstall_launchd() {
    sudo launchctl bootout "system/$LABEL" 2>/dev/null || true
    sudo rm -f "$LAUNCHD_PLIST"
}

uninstall_systemd() {
    sudo systemctl disable --now denon.service 2>/dev/null || true
    sudo rm -f "$SYSTEMD_UNIT"
    sudo systemctl daemon-reload || true
}

do_uninstall() {
    case "$(uname -s)" in
        Darwin) uninstall_launchd ;;
        Linux) uninstall_systemd ;;
    esac
    sudo rm -f "$BIN"
    echo "uninstalled."
}

if [ "$ACTION" = uninstall ]; then
    do_uninstall
    exit 0
fi

require_cmd sudo
require_cmd curl
require_cmd awk
require_cmd sed

OS=$(detect_os)
ARCH=$(detect_arch)
ASSET=denon-${OS}-${ARCH}

if [ "$LOCAL" = 1 ]; then
    SRC=./bin/denon
    [ -x "$SRC" ] || die "no local build at $SRC — run 'make build' first"
else
    if [ "$VERSION" = latest ]; then
        URL="https://github.com/${REPO}/releases/latest/download/${ASSET}"
    else
        URL="https://github.com/${REPO}/releases/download/${VERSION}/${ASSET}"
    fi
    SRC=$(mktemp)
    trap 'rm -f "$SRC"' EXIT
    echo "downloading ${ASSET} from ${URL}"
    curl -fsSL --output "$SRC" "$URL" || die "download failed — does ${REPO} have a ${VERSION} release for ${OS}/${ARCH}?"
    chmod +x "$SRC"
fi

if [ -z "$BASE_URL" ]; then
    INTERCEPT_IP=$(detect_lan_ip)
    [ -n "$INTERCEPT_IP" ] || die "could not detect LAN IP — pass BASE_URL=http://192.168.x.y"
    BASE_URL="http://${INTERCEPT_IP}"
else
    INTERCEPT_IP=$(printf '%s' "$BASE_URL" | sed -E 's|^https?://([^:/]+).*$|\1|')
    [ -n "$INTERCEPT_IP" ] && [ "$INTERCEPT_IP" != "$BASE_URL" ] || die "could not extract host from BASE_URL=${BASE_URL}"
fi

cat <<EOF

About to install denon:
  binary       ${BIN}
  base URL     ${BASE_URL}
  intercept IP ${INTERCEPT_IP}
  service      $([ "$OS" = darwin ] && echo "LaunchDaemon ($LAUNCHD_PLIST)" || echo "systemd unit ($SYSTEMD_UNIT)")

This binds privileged ports 53 (DNS) and 80 (HTTP); sudo will prompt for your password.

EOF

# Skip the confirmation prompt when piped (curl|sh has no controlling tty).
# Users in that flow can't answer y/N anyway; we already showed them what's
# coming. To override, run the script directly: sh install.sh.
if [ -t 0 ]; then
    confirm "Continue?" || { echo "aborted"; exit 1; }
fi

sudo install -m 755 "$SRC" "$BIN"

case "$OS" in
    darwin) install_launchd ;;
    linux) install_systemd ;;
esac

cat <<EOF

Installed and running. Logs:
  macOS:  tail -f /var/log/denon.{out,err}.log
  Linux:  journalctl -fu denon.service

Next: on the receiver
  1. Settings -> Network -> DNS Settings -> Manual
  2. Primary DNS:    ${INTERCEPT_IP}
  3. Secondary DNS:  ${INTERCEPT_IP}
  4. Reboot the receiver
  5. Open Internet Radio

If the menu doesn't populate within ~15 seconds, the logs will show whether
the receiver reached us.
EOF
