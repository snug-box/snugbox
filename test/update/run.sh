#!/bin/sh
# Updates a script install of Snugbox through a test TUF repository, the way
# snugbox self-update does it for real, and checks that it refuses what it
# must: a release whose programs name another version, a release whose
# bytes were changed on the mirror, and metadata that expired. DIR is what
# test/update/prepare.sh made. Run it as root on a disposable machine with
# python3; it installs Snugbox into /usr/local, starts the watcher, and
# removes both again.
#
# Usage: sudo sh test/update/run.sh DIR
set -u

[ $# -eq 1 ] || {
	echo "usage: test/update/run.sh DIR" >&2
	exit 2
}
[ "$(id -u)" = 0 ] || {
	echo "run this as root" >&2
	exit 1
}
dir=$(cd "$1" && pwd)
lib=/usr/local/libexec/snugbox
cmd=/usr/local/bin/snugbox
pass=0
fail=0
server=""

ok() {
	pass=$((pass + 1))
	echo "ok   $*"
}
bad() {
	fail=$((fail + 1))
	echo "FAIL $*"
}
# expect WHAT PATTERN COMMAND...: the command's output holds PATTERN.
expect() {
	what=$1
	pattern=$2
	shift 2
	out=$("$@" 2>&1)
	code=$?
	if printf '%s\n' "$out" | grep -qF -- "$pattern"; then
		ok "$what"
	else
		bad "$what (exit $code): $out"
	fi
}
cleanup() {
	"$cmd" watch uninstall --purge >/dev/null 2>&1
	[ -z "$server" ] || kill "$server" 2>/dev/null
	rm -rf "$lib" "$cmd" /var/lib/snugbox-update
}
trap cleanup EXIT INT TERM

[ ! -e "$lib" ] || {
	echo "$lib exists; this test installs Snugbox there itself" >&2
	exit 1
}
install -d -m 0755 "$lib" /usr/local/bin
tar -xzf "$dir/snugbox-0.0.1-e2e.tar.gz" -C "$lib"
ln -sfn ../libexec/snugbox/snugbox "$cmd"
expect "the watcher runs 0.0.1-e2e" "snugboxd is running" "$cmd" watch install

python3 -m http.server 8099 --bind 127.0.0.1 --directory "$dir/repo" >/dev/null 2>&1 &
server=$!
for _ in 1 2 3 4 5 6 7 8 9 10; do
	python3 -c 'import urllib.request; urllib.request.urlopen("http://127.0.0.1:8099/metadata/1.root.json")' 2>/dev/null && break
	sleep 0.5
done

expect "an empty channel has nothing to install" "has no stable/version" "$cmd" self-update --check
"$dir/tufrepo" publish "$dir/repo" stable amd64 0.0.2-e2e "$dir/snugbox-0.0.2-e2e.tar.gz"
expect "--check sees 0.0.2-e2e" "Snugbox 0.0.2-e2e is out on the stable channel; this is 0.0.1-e2e." "$cmd" self-update --check
expect "--check changes nothing" "snugbox 0.0.1-e2e " "$cmd" version
expect "self-update installs 0.0.2-e2e" "Installed Snugbox 0.0.2-e2e in $lib, in place of 0.0.1-e2e." "$cmd" self-update
expect "the command is 0.0.2-e2e" "snugbox 0.0.2-e2e " "$cmd" version
expect "the watcher was restarted on 0.0.2-e2e" "snugboxd 0.0.2-e2e, running since" "$cmd" watch status
for p in snugboxd snugbox-helper; do
	want=$(tar -xzf "$dir/snugbox-0.0.2-e2e.tar.gz" -O "$p" | sha256sum | cut -d' ' -f1)
	got=$(sha256sum "$lib/$p" | cut -d' ' -f1)
	if [ "$want" = "$got" ]; then ok "$p is the one from 0.0.2-e2e"; else bad "$p is not the one from 0.0.2-e2e"; fi
done
expect "the update cache is root's alone" "700 root" stat -c '%a %U' /var/lib/snugbox-update
expect "nothing newer is installed again" "Snugbox 0.0.2-e2e is the newest release on the stable channel." "$cmd" self-update

# Programs that say they are another version replace nothing.
"$dir/tufrepo" publish "$dir/repo" stable amd64 0.0.3-e2e "$dir/snugbox-0.0.2-e2e.tar.gz"
expect "a release whose programs say 0.0.2-e2e, published as 0.0.3-e2e, is refused" "doesn't run, so nothing was replaced" "$cmd" self-update
expect "still 0.0.2-e2e after the refusal" "snugbox 0.0.2-e2e " "$cmd" version
left=""
for f in "$lib"/.*.update-*; do
	[ -e "$f" ] && left="$left $f"
done
if [ -n "$left" ]; then bad "staged files were left in $lib:$left"; else ok "no staged files left in $lib"; fi

# Bytes changed on the mirror don't match the signed metadata.
"$dir/tufrepo" publish "$dir/repo" stable amd64 0.0.4-e2e "$dir/snugbox-0.0.1-e2e.tar.gz"
"$dir/tufrepo" tamper "$dir/repo" stable amd64
expect "a release changed on the mirror is refused" "doesn't match its signed metadata" "$cmd" self-update
expect "still 0.0.2-e2e after the tampered release" "snugbox 0.0.2-e2e " "$cmd" version

# A repository whose signing stopped serves expired metadata.
"$dir/tufrepo" expire "$dir/repo"
expect "expired metadata is refused" "metadata doesn't verify" "$cmd" self-update --check

echo "$pass passed, $fail failed"
[ "$fail" = 0 ]
