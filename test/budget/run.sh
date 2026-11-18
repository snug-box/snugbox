#!/bin/sh
# Measures Snugbox against the budgets of blueprint section 9.4, on the
# machine it runs on:
#   - snugboxd's working set (anonymous memory, kernel memory and the file
#     cache it keeps hot, its own binary included) stays under 24 MiB at the
#     99th percentile, with 50 workloads running, both with memory to spare
#     and under a light, steady reclaim of the watcher's memory;
#   - snugboxd uses under 0.5% of one core over the measured window;
#   - snugbox-helper stays under 15 MiB while it answers a request, and exits
#     after 60 seconds idle.
# It needs root and a running snugboxd (sudo snugbox watch install), and
# starts 50 throwaway services, so run it on a disposable machine.
#
# Usage: sudo test/budget/run.sh [minutes] [history]   default 10 minutes
#
# history is a store that the soak test kept, with 30 days of 50 workloads
# named like the services this starts:
#   SNUGBOX_SOAK=1 SNUGBOX_SOAK_DB=history.db go test -run TestThirtyDaysOfFiftyWorkloads ./agent/internal/store
# The watcher then restarts on it, as on a box it has watched for a month:
# every summary covers 30 days and the early warnings hold two full hours
# of points. On a new box's empty store it uses far less.
set -eu

[ "$(id -u)" = 0 ] || {
	echo "run it as root; it starts services" >&2
	exit 1
}
minutes=${1:-10}
history=${2:-}
watcher=/sys/fs/cgroup/system.slice/snugboxd.service
helper=/sys/fs/cgroup/system.slice/snugbox-helper.service
[ -d "$watcher" ] || {
	echo "snugboxd is not running; start it with sudo snugbox watch install" >&2
	exit 1
}
if [ -n "$history" ] && [ ! -f "$history" ]; then
	echo "no history at $history" >&2
	exit 1
fi
# The watcher's own binary counts in its working set once the watcher has
# read it in itself, as after a reboot or once memory pressure has pushed it
# out. Installing the package leaves those pages charged to the installer,
# which hid them from this check, so they are dropped before the watcher
# starts again and reads them in.
systemctl stop snugboxd.service
if [ -n "$history" ]; then
	rm -f /var/lib/snugbox/snugbox.db /var/lib/snugbox/snugbox.db-wal /var/lib/snugbox/snugbox.db-shm
	install -m 0600 -o snugbox -g snugbox "$history" /var/lib/snugbox/snugbox.db
fi
dd if=/usr/libexec/snugbox/snugboxd iflag=nocache count=0 status=none
systemctl start snugboxd.service
if [ -n "$history" ]; then
	echo "snugboxd restarted on the history in $history"
fi
work=$(mktemp -d)
cleanup() {
	systemctl stop 'budget-load-*.service' 2>/dev/null || true
	rm -rf "$work"
}
trap cleanup EXIT INT TERM

failures=0
pass() { echo "ok   $1"; }
fail() {
	echo "FAIL $1"
	failures=$((failures + 1))
}

# working_set prints a cgroup's anonymous, kernel and active file memory in
# bytes. Kernels before 5.18 have no "kernel" line; their slab, stacks, page
# tables and socket buffers stand in for it.
working_set() {
	awk '
		$1 == "anon" || $1 == "active_file" { ws += $2 }
		$1 == "kernel" { kernel = $2; seen = 1 }
		$1 == "slab" || $1 == "kernel_stack" || $1 == "pagetables" || $1 == "sock" { parts += $2 }
		END { print ws + (seen ? kernel : parts) }
	' "$1/memory.stat"
}
cpu_usec() { awk '$1 == "usage_usec" { print $2 }' "$1/cpu.stat"; }
p99() { sort -n "$1" | awk '{ v[NR] = $1 } END { i = int(NR * 0.99); if (i < NR * 0.99) i++; if (i < 1) i = 1; print v[i] }'; }
mib() { awk -v b="$1" 'BEGIN { printf "%.1f MiB", b / 1048576 }'; }

i=1
while [ "$i" -le 50 ]; do
	systemd-run --quiet --unit="budget-load-$i" --property=MemoryMax=32M sleep infinity
	i=$((i + 1))
done
echo "50 workloads started; letting snugboxd find them"
sleep 70

samples="$work/ws"
start_cpu=$(cpu_usec "$watcher")
start=$(date +%s)
end=$((start + minutes * 60))
# With memory to spare the kernel ages nothing, so the watcher's binary pages
# go uncounted while its kernel caches pile up; under pressure the kernel
# keeps only what the watcher keeps using, binary pages included. The second
# half of the window runs under a light, steady reclaim of the watcher's own
# memory, 1 MiB before each sample, with its swap closed so that only file
# pages and kernel caches can go, as on a box short of memory.
pressure=
if [ -w "$watcher/memory.reclaim" ] && [ -w "$watcher/memory.swap.max" ] && echo 0 >"$watcher/memory.swap.max"; then
	pressure=$((start + minutes * 30))
	echo "the second half of the window runs under reclaim"
else
	echo "this kernel has no memory.reclaim or memory.swap.max, so the whole window runs with memory to spare"
fi
while [ "$(date +%s)" -lt "$end" ]; do
	if [ -n "$pressure" ] && [ "$(date +%s)" -ge "$pressure" ]; then
		echo 1M >"$watcher/memory.reclaim" 2>/dev/null || true
	fi
	working_set "$watcher" >>"$samples"
	sleep 10
done
used=$(($(cpu_usec "$watcher") - start_cpu))
elapsed=$(($(date +%s) - start))
echo "snugboxd at the end: $(awk '$1 == "anon" || $1 == "kernel" || $1 == "active_file" { printf "%s %.1f MiB  ", $1, $2 / 1048576 }' "$watcher/memory.stat")"

ws99=$(p99 "$samples")
n=$(wc -l <"$samples")
if [ "$ws99" -lt $((24 << 20)) ]; then
	pass "snugboxd's working set, 99th percentile of $n samples: $(mib "$ws99"), under 24 MiB"
else
	fail "snugboxd's working set, 99th percentile of $n samples: $(mib "$ws99"), over 24 MiB"
fi
share=$(awk -v u="$used" -v s="$elapsed" 'BEGIN { printf "%.3f", u / (s * 1e6) * 100 }')
if awk -v p="$share" 'BEGIN { exit !(p < 0.5) }'; then
	pass "snugboxd used $share% of one core over $elapsed seconds, under 0.5%"
else
	fail "snugboxd used $share% of one core over $elapsed seconds, over 0.5%"
fi

# The helper starts for a request and is measured while it is alive. The
# watcher asks the helper things every few minutes, which would restart its
# idle clock, so it is stopped for this part and started again after.
systemctl stop snugboxd.service
snugbox undo >/dev/null 2>&1 || true
if [ -d "$helper" ]; then
	hws=$(working_set "$helper")
	if [ "$hws" -lt $((15 << 20)) ]; then
		pass "snugbox-helper's working set while running: $(mib "$hws"), under 15 MiB"
	else
		fail "snugbox-helper's working set while running: $(mib "$hws"), over 15 MiB"
	fi
else
	fail "snugbox-helper did not start for a request"
fi
asked=$(date +%s)
while systemctl is-active --quiet snugbox-helper.service && [ $(($(date +%s) - asked)) -lt 90 ]; do
	sleep 1
done
idle=$(($(date +%s) - asked))
if systemctl is-active --quiet snugbox-helper.service; then
	fail "snugbox-helper is still running $idle seconds after its last request"
elif [ "$idle" -lt 55 ]; then
	fail "snugbox-helper exited after $idle seconds, before its 60 idle seconds were up"
else
	pass "snugbox-helper exited $idle seconds after its last request"
fi
systemctl start snugboxd.service

[ "$failures" -eq 0 ] || {
	echo "$failures failed"
	exit 1
}
echo "all passed"
