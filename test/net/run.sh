#!/bin/sh
# Blueprint section 16.1: a box that isn't enrolled sends nothing. nftables
# counts every packet that the watcher's user, and a user running an audit,
# send out, and where each goes. With SNUGBOX_OFFLINE=true and with
# --offline there must be none at all; without, only requests to the cloud
# metadata service at 169.254.169.254 on port 80 (docs/data-policy.md).
#
# It installs services and a user and changes the firewall, so run it as
# root on a disposable machine with systemd, nftables and no Snugbox:
#
#   sudo sh test/net/run.sh dist/snugbox_*_linux_amd64.tar.gz
set -eu

[ "$(id -u)" = 0 ] || {
	echo "run it as root; it installs services and firewall rules" >&2
	exit 1
}
if [ $# -ne 1 ] || [ ! -f "$1" ]; then
	echo "usage: $0 path/to/snugbox_<version>_linux_<arch>.tar.gz" >&2
	exit 2
fi
command -v nft >/dev/null || {
	echo "this test needs nft, from the nftables package" >&2
	exit 1
}
for p in /usr/bin/snugbox /usr/local/bin/snugbox /usr/local/libexec/snugbox /var/lib/snugbox /etc/systemd/system/snugboxd.service; do
	if [ -e "$p" ]; then
		echo "$p exists; this test needs a machine without Snugbox" >&2
		exit 1
	fi
done

libexec=/usr/local/libexec/snugbox
snugbox=/usr/local/bin/snugbox
auditor="snugbox-nettest"
table=snugbox_nettest
work=$(mktemp -d)
cleanup() {
	nft delete table inet "$table" 2>/dev/null || true
	if [ -x "$snugbox" ]; then
		"$snugbox" watch uninstall --purge --keep-fixes >/dev/null 2>&1 || true
	fi
	userdel "$auditor" 2>/dev/null || true
	rm -rf "$work" "$libexec" "$snugbox" /etc/snugbox /var/lib/snugbox /etc/sysusers.d/snugbox.conf
}
trap cleanup EXIT INT TERM

failures=0
pass() { echo "ok   $1"; }
fail() {
	echo "FAIL $1"
	failures=$((failures + 1))
}

# count starts the counters again: every packet out from either user, and
# those that go to the metadata service.
count() {
	nft delete table inet "$table" 2>/dev/null || true
	nft -f - <<EOF
table inet $table {
	chain out {
		type filter hook output priority 0; policy accept;
		meta skuid snugbox ip daddr 169.254.169.254 tcp dport 80 counter comment "watcher-metadata"
		meta skuid snugbox counter comment "watcher-all"
		meta skuid $auditor ip daddr 169.254.169.254 tcp dport 80 counter comment "audit-metadata"
		meta skuid $auditor counter comment "audit-all"
	}
}
EOF
}
packets() {
	nft list table inet "$table" | sed -n "s/.*counter packets \([0-9]*\) bytes [0-9]* comment \"$1\".*/\1/p"
}
# settle restarts the watcher with its settings file as it is, and gives it
# time to sample, analyse and, when online, ask the metadata service.
settle() {
	systemctl restart snugboxd.service
	sleep "$1"
}

tar -xzf "$1" -C "$work"
install -d -m 0755 "$libexec" /usr/local/bin
install -m 0755 "$work/snugbox" "$work/snugboxd" "$work/snugbox-helper" "$libexec/"
ln -sfn ../libexec/snugbox/snugbox "$snugbox"
"$snugbox" watch install >/dev/null
id "$auditor" >/dev/null 2>&1 || useradd --system --no-create-home --shell /usr/sbin/nologin "$auditor"

echo 'SNUGBOX_OFFLINE=true' >>/etc/snugbox/snugboxd.env
count
settle 80
w=$(packets watcher-all)
if [ "$w" = 0 ]; then pass "the watcher with SNUGBOX_OFFLINE=true sent nothing in 80 seconds"; else fail "the watcher with SNUGBOX_OFFLINE=true sent $w packets"; fi

sed -i '/^SNUGBOX_OFFLINE=true$/d' /etc/snugbox/snugboxd.env
count
settle 30
w=$(packets watcher-all)
m=$(packets watcher-metadata)
if [ "$w" = "$m" ]; then pass "the watcher sent only to the metadata service: $m of $w packets"; else fail "the watcher sent $w packets, $m of them to the metadata service"; fi

count
runuser -u "$auditor" -- "$snugbox" audit --for 10s --offline >"$work/offline.txt" 2>&1 || true
a=$(packets audit-all)
if [ "$a" = 0 ]; then pass "an audit with --offline sent nothing"; else fail "an audit with --offline sent $a packets"; fi
if grep -q '^  Ran offline: the cloud metadata service was not asked' "$work/offline.txt"; then pass "the offline audit wrote its report and says it ran offline"; else fail "the offline audit's report doesn't say it ran offline: $(cat "$work/offline.txt")"; fi

count
runuser -u "$auditor" -- "$snugbox" audit --for 10s >/dev/null 2>&1 || true
a=$(packets audit-all)
m=$(packets audit-metadata)
if [ "$a" = "$m" ]; then pass "an audit sent only to the metadata service: $m of $a packets"; else fail "an audit sent $a packets, $m of them to the metadata service"; fi

[ "$failures" -eq 0 ] || {
	echo "$failures failed"
	exit 1
}
echo "all passed"
