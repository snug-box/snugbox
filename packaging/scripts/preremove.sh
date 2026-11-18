#!/bin/sh
# Runs before the package is removed, and before an upgrade. A .deb passes
# "remove", "purge" or "upgrade"; an .rpm passes 0 when it is erased and 1 for
# an upgrade. Only a removal stops the services.
set -eu

case "${1:-}" in
remove | purge | 0) ;;
*) exit 0 ;;
esac

# Ask the helper which fixes are in place while it can still answer. Exit
# status 0 means none, 2 means it listed some.
in_place=unknown
if [ -x /usr/libexec/snugbox/snugbox ]; then
	if /usr/libexec/snugbox/snugbox undo >/dev/null 2>&1; then
		in_place=no
	elif [ $? -eq 2 ]; then
		in_place=yes
	fi
fi

if [ -d /run/systemd/system ]; then
	if ! systemctl disable --now snugboxd.service snugbox-helper.socket; then
		echo "snugbox: could not stop snugboxd or the helper's socket; see systemctl status snugboxd" >&2
	fi
	if ! systemctl stop snugbox-helper.service; then
		echo "snugbox: could not stop snugbox-helper; see systemctl status snugbox-helper" >&2
	fi
fi

if [ "$in_place" = yes ] || { [ "$in_place" = unknown ] && [ -s /var/lib/snugbox-helper/journal ]; }; then
	echo "snugbox: fixes Snugbox applied may still be in place. Every file it wrote starts with a" >&2
	echo "snugbox: comment naming its transaction; the journal and snapshots stay in /var/lib/snugbox-helper." >&2
fi
