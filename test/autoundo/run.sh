#!/bin/sh
# The fix catalogue's safety net end to end, as blueprint sections 3.7
# and 16.2 ask: the watcher learns a service, snugbox fit limits it, the
# service then grows past its limit, and snugboxd undoes the limit by itself
# inside the fix's 30 minutes of verification, leaving nothing of it behind.
# It needs the package installed with the watcher running (sudo snugbox
# watch install) and python3, runs as root, and changes the box only through
# Snugbox and a throwaway service that it removes again.
#
# Usage: sudo test/autoundo/run.sh
set -eu

[ "$(id -u)" = 0 ] || {
	echo "run it as root" >&2
	exit 1
}
for u in snugboxd.service snugbox-helper.socket; do
	systemctl is-active --quiet "$u" || {
		echo "$u isn't running; run sudo snugbox watch install first" >&2
		exit 1
	}
done
command -v python3 >/dev/null || {
	echo "it needs python3 for the service that grows" >&2
	exit 1
}

unit=grow-it.service
cg=/sys/fs/cgroup/system.slice/$unit
work=$(mktemp -d /var/tmp/snugbox-autoundo.XXXXXX)
tx=""
cleanup() {
	# A fix the watcher didn't undo is put back by hand, then the service
	# goes.
	if [ -n "$tx" ]; then snugbox undo "$tx" >/dev/null 2>&1 || true; fi
	systemctl stop "$unit" >/dev/null 2>&1 || true
	rm -f "/etc/systemd/system/$unit"
	rm -rf "/etc/systemd/system/$unit.d"
	systemctl daemon-reload
	systemctl reset-failed "$unit" >/dev/null 2>&1 || true
	rm -rf "$work"
}
trap cleanup EXIT
trap 'exit 130' INT TERM
stamp() { echo "##### $(date -u +%H:%M:%S) $*"; }
failures=0
pass() { echo "ok   $*"; }
fail() {
	echo "FAIL $*"
	failures=$((failures + 1))
}

# The service holds 150 MiB, and 400 MiB more once it gets SIGUSR1.
cat >"$work/grow.py" <<'EOF'
import signal, time
held = []
def take(mib):
    b = bytearray(mib << 20)
    b[::4096] = b"\x01" * (len(b) // 4096)
    held.append(b)
take(150)
signal.signal(signal.SIGUSR1, lambda *_: take(400))
while True:
    time.sleep(1)
EOF
printf '[Service]\nExecStart=/usr/bin/python3 %s\nRestart=on-failure\n' "$work/grow.py" >"/etc/systemd/system/$unit"
systemctl daemon-reload
systemctl start "$unit"

stamp "waiting for the watcher to propose a limit for $unit"
i=0
until snugbox fit --list 2>/dev/null | grep -qF "$unit"; do
	i=$((i + 1))
	if [ "$i" -ge 60 ]; then
		fail "no limit for $unit was proposed in 10 minutes: $(snugbox fit --list 2>&1 | tail -n 5)"
		exit 1
	fi
	sleep 10
done
pass "the watcher proposed a limit for $unit within $((i * 10 + 10)) seconds"

if ! out=$(snugbox fit service-limits --only "$unit" --accept-low-confidence --yes 2>&1); then
	fail "snugbox fit failed: $(printf '%s' "$out" | tail -n 3)"
	exit 1
fi
tx=$(printf '%s\n' "$out" | sed -n 's/^Applied as \([^ ]*\)\. .*/\1/p')
if [ -z "$tx" ]; then
	fail "snugbox fit named no transaction: $out"
	exit 1
fi
max=$(cat "$cg/memory.max")
if [ "$max" = max ]; then fail "$tx left memory.max at max"; else pass "applied as $tx: memory.max $max, memory.high $(cat "$cg/memory.high")"; fi

stamp "growing $unit past its limit"
systemctl kill --signal=SIGUSR1 "$unit"
state=""
i=0
while [ "$i" -lt 90 ]; do
	state=$(snugbox fit --status 2>&1 | grep -A1 "^$tx " || true)
	case "$state" in *undone* | *failed*) break ;; esac
	i=$((i + 1))
	sleep 10
done
case "$state" in
*undone*)
	pass "snugboxd undid $tx by itself within $((i * 10 + 10)) seconds: $(printf '%s' "$state" | tr -s ' \n' ' ')"
	tx=""
	;;
*) fail "$tx wasn't undone by snugboxd within 15 minutes: $state" ;;
esac
if [ -e "$cg/memory.max" ] && [ "$(cat "$cg/memory.max")" != max ]; then
	fail "memory.max is $(cat "$cg/memory.max") after the undo"
fi
if [ -e "/etc/systemd/system/$unit.d/50-snugbox-memory.conf" ] || [ -e "/run/systemd/system.control/$unit.d" ]; then
	fail "the fix's drop-in or a runtime setting is left for $unit"
else
	pass "nothing of the fix is left: no drop-in, no runtime setting, memory.max back to max"
fi
journalctl -u snugboxd.service --since "-20 min" --no-pager -o cat 2>/dev/null | grep -iE "undo|undid|kill" | tail -n 5 || true

# The service stalled the box, and whatever runs next needs it calm again
# by the helper's own measure: under 1% in the one-minute average and 0.1%
# with every task stalled in the last 10 seconds. The one-minute average
# takes a few minutes to fall that far.
systemctl stop "$unit" >/dev/null 2>&1 || true
calm() {
	awk '$1 == "some" { split($3, a, "="); some = a[2] } $1 == "full" { split($2, b, "="); full = b[2] }
		END { exit !(some + 0 <= 1.0 && full + 0 <= 0.1) }' /proc/pressure/memory
}
stamp "waiting for the box to calm down"
i=0
until calm; do
	i=$((i + 1))
	if [ "$i" -ge 120 ]; then
		fail "the box was still under memory pressure 10 minutes after the service stopped: $(tr '\n' ' ' </proc/pressure/memory)"
		break
	fi
	sleep 5
done
[ "$i" -ge 120 ] || pass "the box was calm again $((i * 5)) seconds after the service stopped"

[ "$failures" -eq 0 ] || {
	echo "$failures failed"
	exit 1
}
echo "all passed"
