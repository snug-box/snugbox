#!/bin/sh
# Runs packaging/install.sh against a fake GitHub, served by a stand-in curl
# placed first in PATH, and checks that it stops at each mismatch it can
# meet without a real release: a wrong flag, a cosign whose SHA-256 is not
# the pinned one, checksums with no valid signature, an ordinary user with
# no way to become root, and a script cut short in the download. A good
# signature needs the real release workflow, so the happy path is checked
# on the first release instead.
#
# It runs as root so it can also run the installer as nobody.
# Usage: sudo test/install/run.sh /path/to/cosign-linux-amd64
set -eu

[ "$(id -u)" = 0 ] || {
	echo "run it as root; the cases for an ordinary user start from root" >&2
	exit 1
}
if [ $# -ne 1 ] || [ ! -f "$1" ]; then
	echo "usage: $0 /path/to/the-real-cosign-v3.1.4-binary" >&2
	exit 2
fi
real_cosign=$(cd "$(dirname "$1")" && pwd)/$(basename "$1")
here=$(cd "$(dirname "$0")" && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT INT TERM
chmod 0755 "$work"
mkdir "$work/release" "$work/bin" "$work/tools" "$work/sudo" "$work/prefix"
# A copy every user can read, whatever the checkout's permissions.
installer="$work/install.sh"
cp "$here/../../packaging/install.sh" "$installer"
chmod 0644 "$installer"

for b in snugbox snugboxd snugbox-helper; do
	printf '#!/bin/sh\necho %s\n' "$b" >"$work/release/$b"
	chmod 0755 "$work/release/$b"
done
archive=snugbox_9.9.9_linux_amd64.tar.gz
tar -czf "$work/release/$archive" -C "$work/release" snugbox snugboxd snugbox-helper
(cd "$work/release" && sha256sum "$archive" >checksums.txt)
echo '{}' >"$work/release/checksums.txt.sigstore.json"

cat >"$work/bin/curl" <<'EOF'
#!/bin/sh
# Stand-in for curl: writes the file the URL names from $RELEASE, or
# $COSIGN_FILE for cosign, and notes each call in $CURL_LOG when it is set.
out="" url=""
while [ $# -gt 0 ]; do
	case "$1" in
	-o) out=$2; shift 2 ;;
	--proto | --retry) shift 2 ;;
	-*) shift ;;
	*) url=$1; shift ;;
	esac
done
[ -z "${CURL_LOG:-}" ] || echo "$url" >>"$CURL_LOG"
case "$url" in
*/cosign-linux-*) src=$COSIGN_FILE ;;
*) src=$RELEASE/${url##*/} ;;
esac
[ -f "$src" ] || exit 22
if [ -n "$out" ]; then cp "$src" "$out"; else cat "$src"; fi
EOF
chmod 0755 "$work/bin/curl"

# A PATH with the tools the installer needs and no sudo or doas, and a
# second folder that adds a sudo it may name but must not run in a dry run.
cp "$work/bin/curl" "$work/tools/curl"
for t in sh sha256sum tar install mktemp sed grep uname id cat cp chmod mkdir rm ln; do
	ln -s "$(command -v "$t")" "$work/tools/$t"
done
cat >"$work/sudo/sudo" <<'EOF'
#!/bin/sh
echo "sudo ran: $*" >&2
exit 99
EOF
chmod 0755 "$work/sudo/sudo"

failures=0
fail() {
	echo "FAIL $*"
	failures=$((failures + 1))
}
expect() {
	name=$1 want=$2 code=$3
	shift 3
	set +e
	out=$(PATH="$work/bin:$PATH" RELEASE="$work/release" "$@" 2>&1)
	got=$?
	set -e
	if [ "$got" -ne "$code" ] || ! printf '%s' "$out" | grep -q "$want"; then
		fail "$name: exit $got, want $code with \"$want\"; output:"
		printf '%s\n' "$out" | sed 's/^/    /'
		return
	fi
	echo "ok   $name"
}
nothing_installed() {
	if [ -n "$(ls -A "$work/prefix")" ]; then
		fail "$1 left files in $work/prefix"
		rm -rf "${work:?}/prefix/"*
	fi
}

printf '#!/bin/sh\nexit 0\n' >"$work/fake-cosign"
expect "an unknown flag is a usage error" "Usage: install.sh" 2 sh "$installer" --bogus
expect "a bad version is refused" "is not a release version" 1 sh "$installer" --version 1.2
expect "a relative prefix is refused" "must be an absolute path" 1 sh "$installer" --prefix local --version 9.9.9
expect "a dry run as root changes nothing" "Dry run: nothing was downloaded or installed." 0 \
	sh "$installer" --dry-run --version 9.9.9 --prefix "$work/prefix"
expect "a cosign with the wrong SHA-256 stops it" "SHA-256 does not match" 1 \
	env COSIGN_FILE="$work/fake-cosign" sh "$installer" --version 9.9.9 --prefix "$work/prefix"
expect "checksums without a valid signature stop it" "is not signed by" 1 \
	env COSIGN_FILE="$real_cosign" sh "$installer" --version 9.9.9 --prefix "$work/prefix"
nothing_installed "a refused install"

if command -v setpriv >/dev/null 2>&1; then
	as_nobody="setpriv --reuid=65534 --regid=65534 --clear-groups"
	# shellcheck disable=SC2086 # as_nobody is a command and its flags
	expect "an ordinary user's dry run says sudo does the copy" "link $work/prefix/bin/snugbox, with sudo" 0 \
		$as_nobody env PATH="$work/tools:$work/sudo" sh "$installer" --dry-run --version 9.9.9 --prefix "$work/prefix"
	: >"$work/curl.log"
	chmod 0666 "$work/curl.log"
	# shellcheck disable=SC2086 # as_nobody is a command and its flags
	expect "an ordinary user with no sudo or doas is stopped before any download" "neither sudo nor doas" 1 \
		$as_nobody env PATH="$work/tools" CURL_LOG="$work/curl.log" sh "$installer" --version 9.9.9 --prefix "$work/prefix"
	[ ! -s "$work/curl.log" ] || fail "the installer downloaded before finding it could not install: $(cat "$work/curl.log")"
	nothing_installed "a user with no way to root"
else
	echo "skip the ordinary-user cases: setpriv is not installed"
fi

# A download cut short must run nothing: main is only called on the last
# line, so any shorter copy either fails to parse or defines main and ends.
size=$(wc -c <"$installer")
for cut in $((size / 4)) $((size / 2)) $((size - 40)) $((size - 11)); do
	head -c "$cut" "$installer" >"$work/cut.sh"
	set +e
	out=$(PATH="$work/bin:$PATH" RELEASE="$work/release" sh "$work/cut.sh" --version 9.9.9 --prefix "$work/prefix" 2>&1)
	set -e
	if printf '%s' "$out" | grep -q "This will:"; then
		fail "the script cut at byte $cut of $size started working:"
		printf '%s\n' "$out" | sed 's/^/    /'
	else
		echo "ok   the script cut at byte $cut of $size does nothing"
	fi
	nothing_installed "a script cut at byte $cut"
done

[ "$failures" -eq 0 ] || {
	echo "$failures failed"
	exit 1
}
echo "all passed"
