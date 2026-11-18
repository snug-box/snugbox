#!/bin/sh
# Runs after the .deb or .rpm is installed or upgraded. It makes the snugbox
# user and tells systemd about the units. Nothing starts until the person runs
# sudo snugbox watch install; an upgrade restarts what was already running.
set -eu

if command -v systemd-sysusers >/dev/null 2>&1; then
	systemd-sysusers /usr/lib/sysusers.d/snugbox.conf
fi

if [ -d /run/systemd/system ]; then
	systemctl daemon-reload
	for unit in snugboxd.service snugbox-helper.service; do
		if ! systemctl try-restart "$unit"; then
			echo "snugbox: $unit did not restart after the upgrade; see journalctl -u $unit" >&2
		fi
	done
fi
