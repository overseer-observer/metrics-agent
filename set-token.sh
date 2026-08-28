#!/bin/sh
# Metrics agent token replacement: rewrites the token in an existing config and
# restarts the service. Use it after the backend issued a new token for this host.
#
# Usage:
#   curl -fsSL https://github.com/__REPO__/releases/latest/download/set-token.sh \
#     | sudo sh -s -- <token>
#
# A token passed as an argument is visible in ps to any user of the machine.
# On a shared machine pass it through the environment or let the script ask:
#   METRICS_AGENT_TOKEN=<token> sudo -E sh set-token.sh
#   sudo sh set-token.sh
set -eu

TOKEN="${METRICS_AGENT_TOKEN:-}"

USER_NAME=metrics-agent
CONFIG_DIR=/etc/metrics-agent
CONFIG="$CONFIG_DIR/config.yaml"
UNIT=metrics-agent.service

die() {
	echo "metrics-agent: $*" >&2
	exit 1
}

while [ $# -gt 0 ]; do
	case "$1" in
	-*) die "usage: set-token.sh [<token>]" ;;
	*)
		[ -z "$TOKEN" ] || die "usage: set-token.sh [<token>]"
		TOKEN="$1"
		shift
		;;
	esac
done

[ "$(id -u)" = "0" ] || die "run the token replacement as root"
[ -f "$CONFIG" ] || die "$CONFIG not found, install the agent first"

if [ -z "$TOKEN" ]; then
	[ -e /dev/tty ] || die "usage: set-token.sh [<token>]"
	printf 'Server token: ' >&2
	read -r TOKEN </dev/tty
fi

echo "$TOKEN" | grep -Eqi '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' ||
	die "the token does not look like a UUID"

# We rewrite through a temporary file in the same directory and rename it: a crash
# in the middle must not leave the agent with a truncated config.
TMP="$(mktemp "$CONFIG_DIR/config.yaml.XXXXXX")"
trap 'rm -f "$TMP"' EXIT

awk -v token="$TOKEN" '
	!done && /^[[:space:]]*token[[:space:]]*:/ { print "token: " token; done = 1; next }
	{ print }
	END { if (!done) print "token: " token }
' "$CONFIG" >"$TMP"

chown root:"$USER_NAME" "$TMP"
chmod 0640 "$TMP"
mv "$TMP" "$CONFIG"
trap - EXIT

echo "metrics-agent: token updated in $CONFIG"

if command -v systemctl >/dev/null 2>&1; then
	# The agent reads the config only at startup, so the new token takes effect
	# after a restart.
	systemctl restart "$UNIT" || die "failed to restart $UNIT"
	echo "metrics-agent: $UNIT restarted"
else
	echo "metrics-agent: systemd not found, restart the agent manually" >&2
fi
