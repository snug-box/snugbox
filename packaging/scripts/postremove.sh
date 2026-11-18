#!/bin/sh
# Runs after the package is removed. A .deb purge also deletes the watcher's
# history and saved reports in /var/lib/snugbox. The helper's journal, its
# snapshots and any swap file it made stay in /var/lib/snugbox-helper,
# because a fix still in place needs them to be put back by hand.
set -eu

if [ -d /run/systemd/system ]; then
	systemctl daemon-reload
fi

if [ "${1:-}" = "purge" ]; then
	rm -rf /var/lib/snugbox
fi
