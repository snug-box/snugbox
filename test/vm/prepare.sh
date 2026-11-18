#!/bin/sh
# Builds what test/vm/host.sh needs into OUT: the release archives and
# packages for amd64 and arm64 from goreleaser, and the helper's live-test
# binary for each architecture. Run it from a checkout, with Go and
# goreleaser.
#
# Usage: test/vm/prepare.sh OUT
set -eu

[ $# -eq 1 ] || {
	echo "usage: test/vm/prepare.sh OUT" >&2
	exit 2
}
[ -f go.mod ] || {
	echo "run this from the top of a Snugbox checkout" >&2
	exit 1
}

# goreleaser empties dist/ first, so OUT, which may lie inside it, is made
# afterwards.
goreleaser release --snapshot --clean --skip=sign,sbom --parallelism 1
mkdir -p "$1"
out=$(cd "$1" && pwd)
cp dist/snugbox_*_linux_amd64.tar.gz dist/snugbox_*_linux_arm64.tar.gz \
	dist/snugbox_*_amd64.deb dist/snugbox_*_arm64.deb \
	dist/snugbox-*.x86_64.rpm dist/snugbox-*.aarch64.rpm "$out/"

for arch in amd64 arm64; do
	CGO_ENABLED=0 GOOS=linux GOARCH=$arch \
		go test -tags integration -c -o "$out/helper-$arch.test" ./agent/internal/helper
done
echo "prepared $out"
