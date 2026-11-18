#!/bin/sh
# Builds what test/update/run.sh needs into OUT: two releases of Snugbox,
# versions 0.0.1-e2e and 0.0.2-e2e, as release tarballs that both trust a
# test update repository at http://127.0.0.1:8099; the repository with its
# test keys; and the tufrepo tool that publishes to it. Run it from a
# checkout, with Go. The trust files it writes into the source tree for the
# build are removed again before it ends, and it refuses to start if a real
# root is there.
#
# Usage: test/update/prepare.sh OUT
set -eu

[ $# -eq 1 ] || {
	echo "usage: test/update/prepare.sh OUT" >&2
	exit 2
}
mkdir -p "$1"
out=$(cd "$1" && pwd)
trust=agent/internal/update/trust
[ -d "$trust" ] || {
	echo "run this from the top of a Snugbox checkout" >&2
	exit 1
}
[ ! -e "$trust/root.json" ] || {
	echo "$trust/root.json exists; prepare.sh won't overwrite a real root" >&2
	exit 1
}
trap 'rm -f "$trust/root.json" "$trust/repository.json"' EXIT INT TERM

export CGO_ENABLED=0
go build -trimpath -o "$out/tufrepo" ./agent/internal/update/repotest/tufrepo
rm -rf "$out/repo"
"$out/tufrepo" init "$out/repo"
"$out/tufrepo" trust "$out/repo" http://127.0.0.1:8099 "$trust"

for v in 0.0.1-e2e 0.0.2-e2e; do
	rm -rf "${out:?}/$v"
	mkdir "$out/$v"
	for p in snugbox snugboxd snugbox-helper; do
		go build -trimpath -ldflags "-s -w -X snugbox.dev/snugbox/agent/internal/version.Version=$v" -o "$out/$v/$p" "./agent/cmd/$p"
	done
	tar -C "$out/$v" -czf "$out/snugbox-$v.tar.gz" snugbox snugboxd snugbox-helper
	rm -rf "${out:?}/$v"
done
echo "prepared $out"
