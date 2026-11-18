#!/bin/sh
# Takes an install like install.sh's through snugbox watch install and
# uninstall: the programs in /usr/local/libexec/snugbox, the units written
# to /etc/systemd/system, the settings file, a pause the watcher must see
# though sudo passed on a umask of 077, and what uninstall leaves behind.
# It removes what it installed, except the snugbox user and the helper's
# journal, which Snugbox never deletes either.
#
# It installs services, so run it as root on a disposable machine with
# systemd and no Snugbox installed:
#
#   sudo test/watch/run.sh dist/snugbox_*_linux_amd64.tar.gz
set -eu

[ "$(id -u)" = 0 ] || {
	echo "run it as root; it installs services" >&2
	exit 1
}
if [ $# -ne 1 ] || [ ! -f "$1" ]; then
	echo "usage: $0 path/to/snugbox_<version>_linux_<arch>.tar.gz" >&2
	exit 2
fi
for p in /usr/bin/snugbox /usr/local/bin/snugbox /usr/local/libexec/snugbox /etc/snugbox /var/lib/snugbox \
	/etc/systemd/system/snugboxd.service /usr/lib/systemd/system/snugboxd.service; do
	if [ -e "$p" ]; then
		echo "$p exists; this test needs a machine without Snugbox" >&2
		exit 1
	fi
done

here=$(cd "$(dirname "$0")" && pwd)
template=$here/../../agent/internal/units/snugboxd.env
libexec=/usr/local/libexec/snugbox
local_units=/etc/systemd/system
work=$(mktemp -d)
cleanup() {
	if [ -x /usr/local/bin/snugbox ]; then
		/usr/local/bin/snugbox watch uninstall --purge --keep-fixes >/dev/null 2>&1 || true
	fi
	rm -rf "$work" "$libexec" /usr/local/bin/snugbox /etc/snugbox /var/lib/snugbox /etc/sysusers.d/snugbox.conf
}
trap cleanup EXIT INT TERM

failures=0
pass() { echo "ok   $1"; }
fail() {
	echo "FAIL $1"
	failures=$((failures + 1))
}
# check passes when the command succeeds, refute when it fails.
check() {
	name=$1
	shift
	if "$@" >/dev/null 2>&1; then pass "$name"; else fail "$name"; fi
}
refute() {
	name=$1
	shift
	if "$@" >/dev/null 2>&1; then fail "$name"; else pass "$name"; fi
}
mode() { stat -c %a:%U:%G "$1"; }
# strict runs a command the way sudo does for someone whose umask is 077.
strict() { (umask 077 && "$@"); }

tar -xzf "$1" -C "$work"
install -d -m 0755 "$libexec" /usr/local/bin
install -m 0755 "$work/snugbox" "$work/snugboxd" "$work/snugbox-helper" "$libexec/"
ln -sfn ../libexec/snugbox/snugbox /usr/local/bin/snugbox
snugbox=/usr/local/bin/snugbox

check "watch install starts the watcher and the helper's socket" strict "$snugbox" watch install
check "the watcher is running" systemctl is-active snugboxd.service
check "the helper's socket is listening" systemctl is-active snugbox-helper.socket
check "the units are written to $local_units" \
	test -f "$local_units/snugboxd.service" -a -f "$local_units/snugbox-helper.service" -a -f "$local_units/snugbox-helper.socket"
check "systemd accepts the written units" \
	systemd-analyze verify "$local_units/snugboxd.service" "$local_units/snugbox-helper.service" "$local_units/snugbox-helper.socket"
check "the watcher runs from the written unit" test "$(systemctl show -p FragmentPath --value snugboxd.service)" = "$local_units/snugboxd.service"
check "the unit starts the watcher from where the script put it" grep -qx "ExecStart=$libexec/snugboxd" "$local_units/snugboxd.service"
check "the units are 0644 under a umask of 077" test "$(mode "$local_units/snugboxd.service")" = 644:root:root
check "watch install made the snugbox user" id snugbox
check "/etc/snugbox is 0755 under a umask of 077" test "$(mode /etc/snugbox)" = 755:root:root
check "the settings file is root's alone" test "$(mode /etc/snugbox/snugboxd.env)" = 600:root:root
check "the settings file is the shipped one, every setting commented out" cmp -s "$template" /etc/snugbox/snugboxd.env
check "the watcher answers snugbox watch status" "$snugbox" watch status

check "pause under a umask of 077 works" strict "$snugbox" pause
check "the pause flag is 0644 under a umask of 077" test "$(mode /etc/snugbox/paused)" = 644:root:root
check "the watcher, as the snugbox user, sees the pause" sh -c "'$snugbox' watch status | grep -q '^Paused'"
check "resume lifts it" "$snugbox" resume
refute "the watcher sees the pause lifted" sh -c "'$snugbox' watch status | grep -q '^Paused'"

echo 'SNUGBOX_OFFLINE=maybe' >>/etc/snugbox/snugboxd.env
since=$(date '+%Y-%m-%d %H:%M:%S')
check "a second watch install works" strict "$snugbox" watch install
check "a second watch install keeps edited settings" grep -qx 'SNUGBOX_OFFLINE=maybe' /etc/snugbox/snugboxd.env
read_settings() {
	for _ in 1 2 3 4 5 6 7 8 9 10; do
		if journalctl -u snugboxd.service --since "$since" --no-pager -q | grep -q 'neither true nor false; staying offline'; then
			return 0
		fi
		sleep 1
	done
	return 1
}
check "the restarted watcher read the settings file and stayed offline" read_settings

check "watch uninstall --purge works" "$snugbox" watch uninstall --purge
refute "uninstall stops the watcher" systemctl is-active snugboxd.service
refute "uninstall stops the helper's socket" systemctl is-active snugbox-helper.socket
refute "uninstall removes the written units" \
	test -e "$local_units/snugboxd.service" -o -e "$local_units/snugbox-helper.service" -o -e "$local_units/snugbox-helper.socket"
refute "--purge deletes the watcher's history" test -e /var/lib/snugbox/snugbox.db
check "uninstall keeps the settings" test -f /etc/snugbox/snugboxd.env

[ "$failures" -eq 0 ] || {
	echo "$failures failed"
	exit 1
}
echo "all passed"
