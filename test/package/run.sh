#!/bin/sh
# Takes the .deb from goreleaser release --snapshot through install, the
# watcher started from the package's own units, an upgrade that must keep
# edited settings, removal and purge, and checks what each step leaves
# behind. With DURING set, that command runs while the package is installed
# and the watcher runs, so the helper's live tests can drive the packaged
# helper.
#
# It installs and removes a package and starts services, so run it as root
# on a disposable Debian or Ubuntu machine with systemd:
#
#   sudo test/package/run.sh dist/snugbox_*_amd64.deb
#   sudo DURING='./helper.test -test.run TestLive' test/package/run.sh dist/snugbox_*_amd64.deb
set -eu

[ "$(id -u)" = 0 ] || {
	echo "run it as root; it installs a package" >&2
	exit 1
}
if [ $# -ne 1 ] || [ ! -f "$1" ]; then
	echo "usage: $0 path/to/snugbox.deb" >&2
	exit 2
fi
deb=$1
units=/usr/lib/systemd/system

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
main_pid() { systemctl show -p MainPID --value snugboxd.service; }

if dpkg -s snugbox >/dev/null 2>&1; then
	echo "snugbox is installed already; this test needs a machine without it" >&2
	exit 1
fi

dpkg -i "$deb" >/dev/null
check "snugbox on PATH is a link into /usr/libexec/snugbox" test "$(readlink -f /usr/bin/snugbox)" = /usr/libexec/snugbox/snugbox
check "the watcher and the helper sit beside it" test -x /usr/libexec/snugbox/snugboxd -a -x /usr/libexec/snugbox/snugbox-helper
check "snugbox version runs" /usr/bin/snugbox version
check "the package made the snugbox user" id snugbox
check "systemd accepts the units" systemd-analyze verify "$units/snugboxd.service" "$units/snugbox-helper.service" "$units/snugbox-helper.socket"
check "the package installs the watcher's settings, root's alone" test "$(stat -c %a:%U:%G /etc/snugbox/snugboxd.env)" = 600:root:root
check "the settings folder lets the watcher see a pause" test "$(stat -c %a:%U /etc/snugbox)" = 755:root
refute "installing leaves the watcher disabled" systemctl is-enabled snugboxd.service
refute "installing leaves the helper's socket disabled" systemctl is-enabled snugbox-helper.socket

snugbox watch install >/dev/null
check "watch install starts the watcher" systemctl is-active snugboxd.service
check "watch install starts the helper's socket" systemctl is-active snugbox-helper.socket
check "the watcher runs from the package's unit" test "$(systemctl show -p FragmentPath --value snugboxd.service)" = "$units/snugboxd.service"
refute "watch install wrote no copy of the units" test -e /etc/systemd/system/snugboxd.service
check "the watcher's socket is open to its group only" test "$(stat -c %a:%U:%G /run/snugbox/snugboxd.sock)" = 660:snugbox:snugbox
check "the helper's socket is open to the snugbox group only" test "$(stat -c %a:%G /run/snugbox/helper.sock)" = 660:snugbox
exposure=$(systemd-analyze security snugbox-helper.service --no-pager 2>/dev/null | sed -n 's/.*Overall exposure level for snugbox-helper.service: \([0-9.]*\).*/\1/p')
check "the helper's exposure ($exposure) is under 2.0" awk -v e="$exposure" 'BEGIN { exit !(e != "" && e < 2.0) }'
check "the watcher answers snugbox watch status" snugbox watch status
check "the helper answers snugbox fit --status" snugbox fit --status

if [ -n "${DURING:-}" ]; then
	echo "--- $DURING"
	if sh -c "$DURING"; then pass "DURING"; else fail "DURING: $DURING"; fi
	echo "---"
fi

before=$(main_pid)
echo 'SNUGBOX_OFFLINE=true' >>/etc/snugbox/snugboxd.env
dpkg -i "$deb" >/dev/null
after=$(main_pid)
check "an upgrade restarts the running watcher" test "$before" != "$after" -a "$after" != 0
check "an upgrade leaves the watcher enabled" systemctl is-enabled snugboxd.service
check "an upgrade keeps edited settings" grep -qx 'SNUGBOX_OFFLINE=true' /etc/snugbox/snugboxd.env

dpkg -r snugbox >/dev/null
refute "removal stops the watcher" systemctl is-active snugboxd.service
refute "removal stops the helper's socket" systemctl is-active snugbox-helper.socket
refute "removal disables the watcher" systemctl is-enabled snugboxd.service
check "removal keeps the watcher's history" test -d /var/lib/snugbox
check "removal keeps the settings" test -f /etc/snugbox/snugboxd.env
check "removal keeps the snugbox user" id snugbox

dpkg --purge snugbox >/dev/null
refute "purge deletes the watcher's history" test -e /var/lib/snugbox
refute "purge deletes the settings" test -e /etc/snugbox/snugboxd.env
check "purge keeps the helper's journal and snapshots" test -d /var/lib/snugbox-helper
refute "nothing of the package is left in /usr" test -e /usr/libexec/snugbox

[ "$failures" -eq 0 ] || {
	echo "$failures failed"
	exit 1
}
echo "all passed"
