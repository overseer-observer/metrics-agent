#!/bin/sh
# Metrics agent installer.
#
# Usage:
#   curl -fsSL https://github.com/__REPO__/releases/latest/download/install.sh \
#     | sh -s -- --endpoint <url> <token>
#
# A token passed as an argument is visible in ps to any user of the machine during the install.
# On a shared machine pass it through the environment:
#   curl -fsSL .../install.sh -o install.sh
#   METRICS_AGENT_TOKEN=<token> sh install.sh --endpoint <url>
#
# Re-running updates the binary and the unit without overwriting an existing config,
# so --endpoint is only required on the first install.
# Environment overrides: METRICS_AGENT_REPO, METRICS_AGENT_BASE_URL,
# METRICS_AGENT_ENDPOINT, METRICS_AGENT_TOKEN.
set -eu

# __REPO__ is substituted by the release pipeline from GITHUB_REPOSITORY.
REPO="${METRICS_AGENT_REPO:-__REPO__}"
BASE_URL="${METRICS_AGENT_BASE_URL:-https://github.com/$REPO/releases/latest/download}"
ENDPOINT="${METRICS_AGENT_ENDPOINT:-}"
TOKEN="${METRICS_AGENT_TOKEN:-}"

USER_NAME=metrics-agent
BINARY=/usr/local/bin/metrics-agent
CONFIG_DIR=/etc/metrics-agent
CONFIG="$CONFIG_DIR/config.yaml"
STATE_DIR=/var/lib/metrics-agent
UNIT=/etc/systemd/system/metrics-agent.service

die() {
	echo "metrics-agent: $*" >&2
	exit 1
}

usage() {
	die "usage: install.sh --endpoint <url> [<token>]"
}

while [ $# -gt 0 ]; do
	case "$1" in
	--endpoint)
		[ $# -ge 2 ] || usage
		ENDPOINT="$2"
		shift 2
		;;
	--endpoint=*)
		ENDPOINT="${1#--endpoint=}"
		shift
		;;
	-*) usage ;;
	*)
		[ -z "$TOKEN" ] || usage
		TOKEN="$1"
		shift
		;;
	esac
done

fetch() {
	if command -v curl >/dev/null 2>&1; then
		curl -fsSL "$1" -o "$2"
	elif command -v wget >/dev/null 2>&1; then
		wget -qO "$2" "$1"
	else
		die "curl or wget is required"
	fi
}

[ "$(id -u)" = "0" ] || die "run the installation as root"
command -v systemctl >/dev/null 2>&1 || die "systemd not found, installation is impossible"

case "$(uname -m)" in
x86_64 | amd64) ARCH=amd64 ;;
aarch64 | arm64) ARCH=arm64 ;;
*) die "architecture $(uname -m) is not supported" ;;
esac

# The endpoint and the token are only needed on the first install: an update leaves
# the config untouched.
if [ ! -f "$CONFIG" ]; then
	[ -n "$ENDPOINT" ] || usage
	if [ -z "$TOKEN" ]; then
		[ -t 0 ] || [ -e /dev/tty ] || usage
		printf 'Server token: ' >&2
		read -r TOKEN </dev/tty
	fi
fi

if [ -n "$TOKEN" ]; then
	echo "$TOKEN" | grep -Eqi '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' ||
		die "the token does not look like a UUID"
fi

# We create the group ourselves: useradd creates a same-named one only with USERGROUPS_ENAB yes,
# and without it the next install -g "$USER_NAME" fails under set -e.
grep -q "^$USER_NAME:" /etc/group || groupadd --system "$USER_NAME" ||
	die "failed to create the group $USER_NAME"

if ! id "$USER_NAME" >/dev/null 2>&1; then
	shell=/usr/sbin/nologin
	[ -x "$shell" ] || shell=/bin/false
	useradd --system --gid "$USER_NAME" --no-create-home --shell "$shell" "$USER_NAME" ||
		die "failed to create the user $USER_NAME"
fi

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

NAME="metrics-agent-linux-$ARCH"
echo "metrics-agent: downloading $BASE_URL/$NAME"
fetch "$BASE_URL/$NAME" "$TMP/$NAME" || die "failed to download the binary"
fetch "$BASE_URL/SHA256SUMS" "$TMP/SHA256SUMS" || die "failed to download SHA256SUMS"

# The checksum catches a truncated or corrupted download before we install it.
command -v sha256sum >/dev/null 2>&1 || die "sha256sum is required to verify the checksum"
grep " $NAME\$" "$TMP/SHA256SUMS" >"$TMP/expected" || die "SHA256SUMS has no line for $NAME"
(cd "$TMP" && sha256sum -c expected) >/dev/null 2>&1 || die "the checksum of $NAME did not match"

chmod 0755 "$TMP/$NAME"
"$TMP/$NAME" --version >/dev/null 2>&1 || die "the downloaded file does not run"

install -d -m 0750 -o root -g "$USER_NAME" "$CONFIG_DIR"
install -d -m 0700 -o "$USER_NAME" -g "$USER_NAME" "$STATE_DIR"

if [ -f "$CONFIG" ]; then
	echo "metrics-agent: config $CONFIG left unchanged"
else
	umask 027
	cat >"$CONFIG" <<EOF
endpoint: $ENDPOINT
token: $TOKEN
log_level: info
buffer_path: $STATE_DIR/buffer
EOF
	chown root:"$USER_NAME" "$CONFIG"
	chmod 0640 "$CONFIG"
fi

install -m 0755 "$TMP/$NAME" "$BINARY"

cat >"$UNIT" <<EOF
[Unit]
Description=Metrics agent
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=$USER_NAME
Group=$USER_NAME
ExecStart=$BINARY --config $CONFIG
Restart=always
RestartSec=10

# Hardening: the agent only needs to read /proc and write to the buffer.
NoNewPrivileges=yes
PrivateTmp=yes
ProtectSystem=strict
ProtectHome=read-only
ReadWritePaths=$STATE_DIR
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectControlGroups=yes
RestrictSUIDSGID=yes
RestrictRealtime=yes
RestrictNamespaces=yes
LockPersonality=yes
MemoryDenyWriteExecute=yes
CapabilityBoundingSet=
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX
SystemCallFilter=~@clock @debug @module @mount @obsolete @raw-io @reboot @swap @privileged @resources
SystemCallErrorNumber=EPERM

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable metrics-agent.service >/dev/null
systemctl restart metrics-agent.service

echo "metrics-agent: installed, status: systemctl status metrics-agent"
