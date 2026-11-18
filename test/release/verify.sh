#!/bin/sh
# Checks a downloaded Snugbox release, as blueprint sections 6.3 and 16.1
# ask: checksums.txt is signed by this repository's release workflow at
# this tag; every file it lists is here with that SHA-256, and every file
# here is listed; each listed file has its own signature bundle, which
# verifies; each archive comes with an SPDX and a CycloneDX bill of
# materials; and each archive has a .deb and an .rpm beside it. With
# --provenance, the SLSA provenance covers every listed file, and with
# --attestations, so do GitHub's artifact attestations.
#
#   test/release/verify.sh [--key FILE] [--provenance] [--attestations] DIR TAG
#
# --key checks signatures made with that public key and no transparency
# log instead of the release workflow's identity, as test/release/run.sh
# makes them. It needs cosign, or $COSIGN, and sha256sum; --provenance needs
# slsa-verifier, or $SLSA_VERIFIER, and --attestations a logged-in gh. The
# release workflow runs it on each draft before a person publishes it.
set -eu

repo=${SNUGBOX_REPO:-snug-box/snugbox}
cosign=${COSIGN:-cosign}
slsa=${SLSA_VERIFIER:-slsa-verifier}
key="" provenance=0 attestations=0
usage() {
	echo "usage: $0 [--key FILE] [--provenance] [--attestations] DIR TAG" >&2
	exit 2
}
while [ $# -gt 0 ]; do
	case "$1" in
	--key)
		[ $# -ge 2 ] || usage
		key=$(cd "$(dirname "$2")" && pwd)/$(basename "$2")
		shift 2
		;;
	--provenance)
		provenance=1
		shift
		;;
	--attestations)
		attestations=1
		shift
		;;
	-*) usage ;;
	*) break ;;
	esac
done
[ $# -eq 2 ] || usage
dir=$1 tag=$2
case "$tag" in
v[0-9]*) ;;
*)
	echo "$tag is not a release tag such as v0.1.0" >&2
	exit 2
	;;
esac
cd "$dir"

failures=0
pass() { echo "ok   $*"; }
fail() {
	echo "FAIL $*"
	failures=$((failures + 1))
}
identity="https://github.com/$repo/.github/workflows/release.yml@refs/tags/$tag"
signer="the release workflow at $tag"
[ -z "$key" ] || signer="the key $key"

# verify_blob checks file $1 against its signature bundle $2.
verify_blob() {
	if [ -n "$key" ]; then
		"$cosign" verify-blob --key "$key" --bundle "$2" --insecure-ignore-tlog=true "$1" >/dev/null 2>&1
	else
		"$cosign" verify-blob --bundle "$2" --certificate-identity "$identity" \
			--certificate-oidc-issuer https://token.actions.githubusercontent.com "$1" >/dev/null 2>&1
	fi
}

# Nothing below can be trusted unless the checksums can.
if [ ! -f checksums.txt ] || [ ! -f checksums.txt.sigstore.json ]; then
	echo "FAIL $dir has no checksums.txt and checksums.txt.sigstore.json"
	exit 1
fi
if ! verify_blob checksums.txt checksums.txt.sigstore.json; then
	echo "FAIL checksums.txt is not signed by $signer"
	exit 1
fi
pass "checksums.txt is signed by $signer"

if sha256sum --strict --quiet -c checksums.txt; then
	pass "every file checksums.txt lists is here, with its SHA-256"
else
	fail "a file checksums.txt lists is missing or changed"
fi
listed=$(awk '{ n = $2; sub(/^\*/, "", n); print n }' checksums.txt)
is_listed() { printf '%s\n' "$listed" | grep -qxF "$1"; }

unlisted=0
for f in *; do
	[ -f "$f" ] || continue
	case "$f" in
	checksums.txt | *.sigstore.json | *.intoto.jsonl) continue ;;
	esac
	if ! is_listed "$f"; then
		fail "$f is here but not in checksums.txt"
		unlisted=1
	fi
done
[ "$unlisted" = 1 ] || pass "every file here is in checksums.txt"

bad=0
for f in $listed; do
	if [ ! -f "$f.sigstore.json" ]; then
		fail "$f has no signature bundle"
		bad=1
	elif ! verify_blob "$f" "$f.sigstore.json"; then
		fail "$f's signature bundle doesn't verify as $signer"
		bad=1
	fi
done
[ "$bad" = 1 ] || pass "every listed file has its own signature, by $signer"

archives=$(printf '%s\n' "$listed" | grep -E '^snugbox_[^/]+_linux_[a-z0-9]+\.tar\.gz$' || true)
[ -n "$archives" ] || fail "checksums.txt lists no archive"
for a in $archives; do
	arch=${a%.tar.gz}
	arch=${arch##*_}
	for kind in spdx cdx; do
		s="$a.$kind.json"
		marker='"spdxVersion"'
		[ "$kind" = spdx ] || marker='"bomFormat"'
		if ! is_listed "$s"; then
			fail "$a has no $kind bill of materials in checksums.txt"
		elif ! grep -q "$marker" "$s" || ! grep -qF "$a" "$s"; then
			fail "$s is not a $kind bill of materials of $a"
		fi
	done
	rpm_arch=$arch
	case "$arch" in
	amd64) rpm_arch=x86_64 ;;
	arm64) rpm_arch=aarch64 ;;
	esac
	printf '%s\n' "$listed" | grep -qE "^snugbox_[^/]+_${arch}\.deb$" || fail "there is no $arch .deb"
	printf '%s\n' "$listed" | grep -qE "^snugbox-[^/]+\.${rpm_arch}\.rpm$" || fail "there is no $rpm_arch .rpm"
done
pass "checked the bills of materials and packages of $(printf '%s\n' "$archives" | grep -c .) archives"

if [ "$provenance" = 1 ]; then
	prov=$(find . -maxdepth 1 -name '*.intoto.jsonl' | head -n 1)
	if [ -z "$prov" ]; then
		fail "there is no SLSA provenance, *.intoto.jsonl"
	else
		# shellcheck disable=SC2086 # one argument per listed file
		if "$slsa" verify-artifact --provenance-path "$prov" --source-uri "github.com/$repo" --source-tag "$tag" $listed >/dev/null 2>&1; then
			pass "the SLSA provenance in ${prov#./} covers every listed file, built from $repo at $tag"
		else
			fail "the SLSA provenance in ${prov#./} doesn't verify every listed file"
		fi
	fi
fi

if [ "$attestations" = 1 ]; then
	bad=0
	for f in $listed; do
		if ! gh attestation verify "$f" --repo "$repo" --signer-workflow "github.com/$repo/.github/workflows/release.yml" \
			--source-ref "refs/tags/$tag" >/dev/null 2>&1; then
			fail "$f has no artifact attestation from the release workflow at $tag"
			bad=1
		fi
	done
	[ "$bad" = 1 ] || pass "every listed file has an artifact attestation from the release workflow at $tag"
fi

[ "$failures" -eq 0 ] || {
	echo "$failures failed"
	exit 1
}
echo "all passed"
