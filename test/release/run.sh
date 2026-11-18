#!/bin/sh
# Tests test/release/verify.sh on a release made here: two archives with
# their bills of materials and packages, install.sh, checksums.txt, and a
# signature bundle for each file, made with a throwaway cosign key and no
# transparency log. The whole release must pass, and each way a release can
# be wrong must fail: a changed file, a missing package, a file checksums.txt
# doesn't list, a missing bill of materials or one of another archive, a
# missing rpm, a missing or swapped bundle, checksums signed by another key
# or not at all, and no provenance when --provenance asks for it.
#
# The release workflow's own identity can sign only on GitHub, so the check
# of a real release's keyless signatures, SLSA provenance and attestations
# runs in the release workflow, on every draft. Given a GoReleaser dist
# folder too, the snapshot's own files are signed here and must pass, so
# verify.sh keeps up with what GoReleaser makes.
#
# Usage: test/release/run.sh /path/to/cosign-v3.1.4 [dist]
set -eu

if [ $# -lt 1 ] || [ $# -gt 2 ] || [ ! -f "$1" ]; then
	echo "usage: $0 /path/to/the-real-cosign-v3.1.4-binary [goreleaser-dist-folder]" >&2
	exit 2
fi
dist=""
if [ $# -eq 2 ]; then
	dist=$(cd "$2" && pwd)
	[ -f "$dist/checksums.txt" ] || {
		echo "$2 has no checksums.txt; build it with make snapshot" >&2
		exit 2
	}
fi
here=$(cd "$(dirname "$0")" && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT INT TERM
cp "$1" "$work/cosign"
chmod 0755 "$work/cosign"
export COSIGN="$work/cosign" COSIGN_PASSWORD=""
(cd "$work" && "$COSIGN" generate-key-pair --output-key-prefix good >/dev/null 2>&1)
(cd "$work" && "$COSIGN" generate-key-pair --output-key-prefix other >/dev/null 2>&1)

tag=v9.9.9
rel="$work/release"
mkdir "$rel"
for arch in amd64 arm64; do
	a="snugbox_9.9.9_linux_$arch.tar.gz"
	echo "archive $arch" >"$rel/$a"
	printf '{"spdxVersion": "SPDX-2.3", "name": "%s"}\n' "$a" >"$rel/$a.spdx.json"
	printf '{"bomFormat": "CycloneDX", "metadata": {"component": {"name": "%s"}}}\n' "$a" >"$rel/$a.cdx.json"
	echo "deb $arch" >"$rel/snugbox_9.9.9_$arch.deb"
done
echo "rpm x86_64" >"$rel/snugbox-9.9.9-1.x86_64.rpm"
echo "rpm aarch64" >"$rel/snugbox-9.9.9-1.aarch64.rpm"
echo "#!/bin/sh" >"$rel/install.sh"

# sign signs file $1, in the current folder, with key $2 into its bundle.
sign() {
	"$COSIGN" sign-blob --key "$work/$2.key" --bundle "$1.sigstore.json" \
		--use-signing-config=false --tlog-upload=false --yes "$1" >/dev/null 2>&1
}
# relist writes checksums.txt again for the files it lists that are still
# here, and signs it, as a release built without the others would be.
relist() {
	awk '{ n = $2; sub(/^\*/, "", n); print n }' checksums.txt >"$work/names"
	: >"$work/sums"
	while read -r n; do
		if [ -f "$n" ]; then sha256sum -- "$n" >>"$work/sums"; fi
	done <"$work/names"
	mv "$work/sums" checksums.txt
	sign checksums.txt good
}
(
	cd "$rel"
	sha256sum -- * >"$work/sums"
	mv "$work/sums" checksums.txt
	for f in *; do sign "$f" good; done
)

failures=0
fail() {
	echo "FAIL $*"
	failures=$((failures + 1))
}
# expect runs verify.sh with $3 as extra flags on a copy of the release that
# $4 changed, and checks it exits with $2 and says $5.
expect() {
	name=$1 code=$2 flags=$3 change=$4 want=$5
	rm -rf "$work/copy"
	cp -r "$rel" "$work/copy"
	(cd "$work/copy" && eval "$change")
	set +e
	# shellcheck disable=SC2086 # flags are separate words
	out=$(sh "$here/verify.sh" --key "$work/good.pub" $flags "$work/copy" "$tag" 2>&1)
	got=$?
	set -e
	if [ "$got" -ne "$code" ] || ! printf '%s' "$out" | grep -qF -- "$want"; then
		fail "$name: exit $got, want $code with \"$want\"; output:"
		printf '%s\n' "$out" | sed 's/^/    /'
		return
	fi
	echo "ok   $name"
}

a64=snugbox_9.9.9_linux_amd64.tar.gz
expect "the whole release passes" 0 "" ":" "all passed"
expect "a changed archive fails" 1 "" "echo changed >$a64" "a file checksums.txt lists is missing or changed"
expect "a missing package fails" 1 "" "rm snugbox_9.9.9_arm64.deb" "a file checksums.txt lists is missing or changed"
expect "a file checksums.txt doesn't list fails" 1 "" "echo extra >extra.bin" "extra.bin is here but not in checksums.txt"
expect "a missing bill of materials fails" 1 "" "rm $a64.cdx.json $a64.cdx.json.sigstore.json && relist" \
	"$a64 has no cdx bill of materials"
expect "a bill of materials of another archive fails" 1 "" \
	"cp snugbox_9.9.9_linux_arm64.tar.gz.spdx.json $a64.spdx.json && sign $a64.spdx.json good && relist" \
	"$a64.spdx.json is not a spdx bill of materials of $a64"
expect "a missing rpm fails" 1 "" "rm snugbox-9.9.9-1.aarch64.rpm snugbox-9.9.9-1.aarch64.rpm.sigstore.json && relist" \
	"there is no aarch64 .rpm"
expect "a missing bundle fails" 1 "" "rm install.sh.sigstore.json" "install.sh has no signature bundle"
expect "another file's bundle fails" 1 "" "cp snugbox_9.9.9_amd64.deb.sigstore.json install.sh.sigstore.json" \
	"install.sh's signature bundle doesn't verify"
expect "checksums signed by another key fail" 1 "" "sign checksums.txt other" "checksums.txt is not signed by the key"
expect "unsigned checksums fail" 1 "" "rm checksums.txt.sigstore.json" "has no checksums.txt and checksums.txt.sigstore.json"
expect "--provenance fails without provenance" 1 "--provenance" ":" "there is no SLSA provenance"

set +e
sh "$here/verify.sh" --key "$work/good.pub" "$rel" main >/dev/null 2>&1
got=$?
set -e
if [ "$got" -eq 2 ]; then echo "ok   a tag that isn't a release tag is a usage error"; else fail "a branch name as the tag: exit $got, want 2"; fi

if [ -n "$dist" ]; then
	snap="$work/snapshot"
	mkdir "$snap"
	awk '{ n = $2; sub(/^\*/, "", n); print n }' "$dist/checksums.txt" | while read -r f; do
		# install.sh is attached to a release from packaging/, not dist/.
		if [ "$f" = install.sh ]; then cp "$here/../../packaging/install.sh" "$snap/"; else cp "$dist/$f" "$snap/"; fi
	done
	cp "$dist/checksums.txt" "$snap/"
	(
		cd "$snap"
		for f in *; do sign "$f" good; done
	)
	set +e
	out=$(sh "$here/verify.sh" --key "$work/good.pub" "$snap" v0.0.0 2>&1)
	got=$?
	set -e
	if [ "$got" -eq 0 ]; then
		echo "ok   the snapshot GoReleaser built passes: $(grep -c . "$dist/checksums.txt") files"
	else
		fail "the snapshot in $dist doesn't pass, exit $got:"
		printf '%s\n' "$out" | sed 's/^/    /'
	fi
fi

[ "$failures" -eq 0 ] || {
	echo "$failures failed"
	exit 1
}
echo "all passed"
