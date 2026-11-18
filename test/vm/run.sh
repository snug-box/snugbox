#!/bin/sh
# Blueprint sections 9.7 and 16.2: Snugbox audits and fixes a box on every
# supported distribution, and a fix survives a reboot in the middle. This
# runs as root inside a fresh virtual machine that test/vm/host.sh booted,
# with a release folder holding the tarball, the .deb or .rpm and the
# helper's live-test binary, helper-<arch>.test.
#
#   run.sh before <release-dir>   # everything up to the reboot
#   run.sh after <release-dir>    # once the host has rebooted the machine
#
# Before the reboot: earlyoom, systemd-oomd and btrfs-progs are installed
# where the distribution has them, so their live tests run too; snugbox
# doctor runs from the tarball, and where pressure stall information is off
# its advice is followed and the phase ends with "REBOOT FOR PSI", which
# makes the host reboot and run it again; a read-only audit must find a
# planted memory hog and offer no fix the helper would refuse; the package
# goes in with the command docs/install.md
# gives; the watcher and helper start from the package's units; every live
# test runs, with systemd-oomd masked until its own test as in CI; swap and
# a systemd-oomd policy the distribution set itself, as Fedora sets both,
# are first shown to be left alone by the fixes, then set aside the way a
# person would for the fixes' own tests; snugboxd undoes by itself a limit
# that hurts (test/autoundo/run.sh, copied in beside this script as
# autoundo.sh); then a swap fix and a cut-short service limit are left for
# the reboot. After it:
# the watcher is back, the swap fix is still being verified, the cut-short
# limit was put back, and the package is removed the way docs/install.md
# says.
#
# It changes the machine, so host.sh only runs it in a throwaway one.
set -eu

phase=${1:?usage: run.sh <before|after> <release-dir>}
rel=${2:?usage: run.sh <before|after> <release-dir>}
here=$(cd "$(dirname "$0")" && pwd)
[ "$(id -u)" = 0 ] || {
	echo "run it as root" >&2
	exit 1
}

case "$(uname -m)" in
x86_64) arch=amd64 rpm_arch=x86_64 ;;
aarch64) arch=arm64 rpm_arch=aarch64 ;;
*)
	echo "unsupported architecture $(uname -m)" >&2
	exit 1
	;;
esac
tarball=$(find "$rel" -maxdepth 1 -name "snugbox_*_linux_${arch}.tar.gz" | head -1)
live=/usr/local/libexec/snugbox-tests/helper.test
if ! { [ -f "$tarball" ] && [ -f "$rel/helper-$arch.test" ]; }; then
	echo "$rel needs the $arch tarball and helper-$arch.test" >&2
	exit 1
fi

failures=0
pass() { echo "ok   $1"; }
fail() {
	echo "FAIL $1"
	failures=$((failures + 1))
}
section() { echo "##### $*"; }

if command -v apt-get >/dev/null 2>&1; then
	pkg=$(find "$rel" -maxdepth 1 -name "snugbox_*_${arch}.deb" | head -1)
	pkg_install() { DEBIAN_FRONTEND=noninteractive apt-get install -y -qq "$1" >/dev/null; }
	pkg_remove() { DEBIAN_FRONTEND=noninteractive apt-get purge -y -qq snugbox >/dev/null; }
	tool_install() { DEBIAN_FRONTEND=noninteractive apt-get install -y -qq --no-install-recommends "$1" >/dev/null 2>&1; }
	tools_refresh() { apt-get update -qq >/dev/null 2>&1; }
else
	pkg=$(find "$rel" -maxdepth 1 -name "snugbox-*.${rpm_arch}.rpm" | head -1)
	pkg_install() { dnf install -y -q "$1" >/dev/null; }
	pkg_remove() { dnf remove -y -q snugbox >/dev/null; }
	tool_install() { dnf install -y -q "$1" >/dev/null 2>&1; }
	tools_refresh() { :; }
fi

# A memory hog as a service, so the audit and the live tests have a
# workload that holds real pages.
hog=snugbox-vm-hog.service
start_hog() {
	cat >"/etc/systemd/system/$hog" <<EOF
[Service]
ExecStart=/usr/bin/python3 -c "import time; b=bytearray(80<<20); b[::4096]=b'\\x01'*((80<<20)//4096); time.sleep(10**6)"
EOF
	systemctl daemon-reload
	systemctl start "$hog"
}
stop_hog() {
	systemctl stop "$hog" 2>/dev/null || true
	rm -f "/etc/systemd/system/$hog"
	systemctl daemon-reload
}

# live_tests runs the live tests matching $1, skipping $2, prints each
# result with its reason, and fails the gate when the binary does.
live_tests() {
	if "$live" -test.v -test.count=1 -test.run "$1" -test.skip "$2" >/tmp/live.txt 2>&1; then
		ok=1
	else
		ok=0
	fi
	grep -E '^(--- (PASS|FAIL|SKIP)|panic)|^\s+\S+_test\.go:[0-9]+:' /tmp/live.txt || true
	if [ "$ok" = 1 ]; then pass "live tests $1 passed"; else fail "live tests $1: $(tail -n 3 /tmp/live.txt)"; fi
}

# oomd_policy prints the slices whose systemd-oomd policy the box set
# itself, as Fedora's systemd-oomd-defaults sets one for system.slice.
oomd_policy() {
	for s in system.slice user.slice; do
		v=$(systemctl show -p ManagedOOMMemoryPressure --value -- "$s" 2>/dev/null || true)
		if [ -n "$v" ] && [ "$v" != auto ]; then echo "$s"; fi
	done
}

# has_swap says whether any swap area is on.
has_swap() { [ -n "$(swapon --noheadings --show=NAME)" ]; }

before_reboot() {
	section "$(sed -n 's/^PRETTY_NAME=//p' /etc/os-release | tr -d '"'), kernel $(uname -r), $(systemctl --version | head -n 1)"

	section "test tools from the distribution"
	tools_refresh
	command -v python3 >/dev/null || tool_install python3
	for t in earlyoom systemd-oomd btrfs-progs; do
		if tool_install "$t"; then echo "installed $t"; else echo "$t isn't packaged here, or comes with systemd"; fi
	done
	# Their live tests need both daemons stopped, as a box Snugbox fixes has.
	systemctl disable --now earlyoom.service >/dev/null 2>&1 || true
	systemctl disable --now systemd-oomd.socket systemd-oomd.service >/dev/null 2>&1 || true

	section "snugbox doctor, from the tarball"
	install -d -m 0755 /usr/local/libexec/snugbox /usr/local/bin
	tar -xzf "$tarball" -C /usr/local/libexec/snugbox
	ln -sfn ../libexec/snugbox/snugbox /usr/local/bin/snugbox
	if /usr/local/bin/snugbox doctor >/tmp/doctor.txt 2>&1; then pass "doctor passes"; else fail "doctor failed (exit $?)"; fi
	cat /tmp/doctor.txt
	# RHEL 9 and its rebuilds ship with pressure stall information off, and
	# no fix runs without it. Do what doctor says, as a person would; the
	# host reboots and runs this phase again.
	if [ ! -e /proc/pressure/memory ]; then
		if command -v grubby >/dev/null && grep -q 'sudo grubby --update-kernel=ALL --args=psi=1' /tmp/doctor.txt; then
			grubby --update-kernel=ALL --args=psi=1
			echo "REBOOT FOR PSI"
			exit 0
		fi
		fail "pressure stall information is off, and doctor's advice doesn't fit this box"
	fi

	section "read-only audit"
	start_hog
	sleep 12
	/usr/local/bin/snugbox audit --for 10s --offline >/tmp/audit.txt 2>&1 || true
	if grep -q '^SUMMARY' /tmp/audit.txt; then pass "audit wrote a report"; else fail "audit wrote no report: $(tail -n 5 /tmp/audit.txt)"; fi
	if grep -q "$hog" /tmp/audit.txt; then pass "audit saw the memory hog"; else fail "audit missed the hog"; fi
	# Fixes the helper would refuse aren't offered: none for swap where the
	# box has swap of its own, as Fedora has on zram, and none for systemd-oomd
	# where the distribution gave it a policy, which gets advice instead.
	if has_swap; then
		if grep -qE '^  [0-9]+  Swap file' /tmp/audit.txt; then fail "audit offered a swap fix on a box with swap"; else pass "audit offers no swap fix on a box with swap"; fi
	fi
	if [ -n "$(oomd_policy)" ]; then
		if grep -q 'Early out-of-memory protection, systemd-oomd' /tmp/audit.txt || ! grep -q 'sudo systemctl enable --now systemd-oomd' /tmp/audit.txt; then
			fail "audit doesn't say to start the distribution's systemd-oomd: $(sed -n '/^FIXES/,/^$/p' /tmp/audit.txt)"
		else
			pass "audit says to start the distribution's systemd-oomd instead of offering a fix"
		fi
	fi
	stop_hog
	rm -rf /usr/local/libexec/snugbox /usr/local/bin/snugbox

	section "the package, installed as docs/install.md says"
	pkg_install "$pkg"
	snugbox watch install >/dev/null
	if systemctl is-active --quiet snugboxd.service; then pass "the watcher runs"; else fail "the watcher doesn't run"; fi
	if systemctl is-active --quiet snugbox-helper.socket; then pass "the helper's socket listens"; else fail "the helper's socket isn't listening"; fi

	section "the helper's live tests"
	install -D -m 0755 "$rel/helper-$arch.test" "$live"
	# A login can turn systemd-oomd on, so it stays masked until its test.
	systemctl mask --runtime --now systemd-oomd.socket systemd-oomd.service >/dev/null 2>&1 || true
	# The swap fix must leave the box's own swap alone. Fedora's swap on
	# zram then goes off for good, as on a box without swap, so the swap fix
	# is tested on Fedora's Btrfs root too.
	if has_swap; then live_tests TestLiveLeavesTheirSwapAlone '^$'; fi
	if swapon --noheadings --show=NAME | grep -q '^/dev/zram'; then
		: >/etc/systemd/zram-generator.conf
		swapon --noheadings --show=NAME | grep '^/dev/zram' | xargs -r -n 1 swapoff || fail "swap on zram didn't go off"
		systemctl daemon-reload
		systemctl stop 'systemd-zram-setup@*.service' >/dev/null 2>&1 || true
		echo "swap on zram turned off; swap now: [$(swapon --noheadings --show=NAME | tr '\n' ' ')]"
	fi
	live_tests TestLive 'TestLiveOOMD|TestLiveReboot'
	systemctl unmask --runtime systemd-oomd.socket systemd-oomd.service >/dev/null 2>&1 || true
	systemctl stop systemd-oomd.socket systemd-oomd.service >/dev/null 2>&1 || true
	# The systemd-oomd fix must leave the distribution's policy alone. Then
	# the drop-ins that set it are masked with links to /dev/null of the same
	# name in /etc, as a person would, for the fix's own test, and put back.
	masks=""
	if [ -n "$(oomd_policy)" ]; then
		live_tests TestLiveOOMDLeavesTheirPolicyAlone '^$'
		for s in $(oomd_policy); do
			for f in $(systemctl show -p DropInPaths --value -- "$s"); do
				if grep -q '^ManagedOOMMemoryPressure=' "$f" 2>/dev/null; then
					install -d -m 0755 "/etc/systemd/system/$s.d"
					ln -sfn /dev/null "/etc/systemd/system/$s.d/${f##*/}"
					masks="$masks /etc/systemd/system/$s.d/${f##*/}"
				fi
			done
		done
		systemctl daemon-reload
		echo "masked for the fix's test:$masks; policy now: [$(oomd_policy)]"
	fi
	live_tests 'TestLiveOOMD$' '^$'
	if [ -n "$masks" ]; then
		for m in $masks; do
			rm -f "$m"
			rmdir "${m%/*}" 2>/dev/null || true
		done
		systemctl daemon-reload
		if [ -n "$(oomd_policy)" ]; then pass "the distribution's systemd-oomd policy is back"; else fail "the distribution's systemd-oomd policy didn't come back"; fi
	fi

	section "the watcher undoes a limit that hurts, by itself"
	if sh "$here/autoundo.sh" >/tmp/autoundo.txt 2>&1; then pass "snugboxd undid a limit that hurt"; else fail "automatic undo: $(tail -n 4 /tmp/autoundo.txt)"; fi
	grep -E '^(ok|FAIL) ' /tmp/autoundo.txt || true

	section "a swap fix and a service limit left for the reboot"
	SNUGBOX_LIVE_REBOOT=before live_tests TestLiveReboot '^$'
	echo "swap before the reboot: $(swapon --noheadings --show=NAME | tr '\n' ' ')"
}

after_reboot() {
	section "after the reboot"
	echo "up since $(uptime -s); swap $(swapon --noheadings --show=NAME | tr '\n' ' '); zswap $(cat /sys/module/zswap/parameters/enabled 2>/dev/null || echo none)"
	if systemctl is-active --quiet snugboxd.service; then pass "the watcher came back"; else fail "the watcher didn't come back"; fi
	SNUGBOX_LIVE_REBOOT=after live_tests TestLiveReboot '^$'

	section "removed as docs/install.md says"
	snugbox watch uninstall --purge >/dev/null
	pkg_remove
	if systemctl is-active --quiet snugboxd.service; then fail "the watcher runs after removal"; else pass "nothing of Snugbox runs after removal"; fi
	for d in /usr/libexec/snugbox /usr/share/doc/snugbox; do
		if [ -e "$d" ]; then fail "$d is left: $(find "$d" -mindepth 1 -maxdepth 1 | tr '\n' ' ')"; else pass "$d is gone"; fi
	done
	left=$(grep -rl "Made by Snugbox" /etc/systemd/system /etc/tmpfiles.d 2>/dev/null || true)
	if [ -z "$left" ]; then pass "no file a fix wrote is left"; else fail "fix files left: $left"; fi
	if [ -z "$(ls -A /run/systemd/system.control 2>/dev/null)" ]; then pass "no runtime setting is left"; else fail "runtime settings left: $(ls /run/systemd/system.control)"; fi
}

case "$phase" in
before) before_reboot ;;
after) after_reboot ;;
*)
	echo "unknown phase $phase" >&2
	exit 2
	;;
esac
[ "$failures" -eq 0 ] || {
	echo "$failures failed"
	exit 1
}
echo "all passed"
