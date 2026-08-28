#!/bin/sh
# Metrics agent removal: unit, binary, directories, user.
set -eu

USER_NAME=metrics-agent
BINARY=/usr/local/bin/metrics-agent
CONFIG_DIR=/etc/metrics-agent
STATE_DIR=/var/lib/metrics-agent
UNIT=/etc/systemd/system/metrics-agent.service

[ "$(id -u)" = "0" ] || {
	echo "metrics-agent: run the removal as root" >&2
	exit 1
}

if command -v systemctl >/dev/null 2>&1; then
	systemctl disable --now metrics-agent.service >/dev/null 2>&1 || true
fi

rm -f "$UNIT"
if command -v systemctl >/dev/null 2>&1; then
	systemctl daemon-reload
	systemctl reset-failed metrics-agent.service >/dev/null 2>&1 || true
fi

rm -f "$BINARY"
rm -rf "$CONFIG_DIR" "$STATE_DIR"

if id "$USER_NAME" >/dev/null 2>&1; then
	userdel "$USER_NAME" >/dev/null 2>&1 || true
fi

echo "metrics-agent: removed"
