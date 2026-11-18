#!/bin/sh
# Runs every fuzz target in the module, one at a time, since go test fuzzes
# one target of one package per run. Each gets FUZZTIME, 30s unless set, or
# an equal share of FUZZ_MINUTES when that is set instead. A failure leaves
# its input in the package's testdata/fuzz folder; commit it with the fix so
# every go test replays the case afterwards.
#
# Usage: test/fuzz.sh    FUZZTIME=5m test/fuzz.sh    FUZZ_MINUTES=60 test/fuzz.sh
set -eu

targets=$(mktemp)
trap 'rm -f "$targets"' EXIT INT TERM
for pkg in $(go list ./...); do
	go test -list '^Fuzz' "$pkg" | sed -n "s|^\(Fuzz[A-Za-z0-9_]*\)\$|$pkg \1|p" >>"$targets"
done
count=$(wc -l <"$targets")
[ "$count" -gt 0 ] || {
	echo "no fuzz targets found" >&2
	exit 1
}
fuzztime=${FUZZTIME:-30s}
if [ -n "${FUZZ_MINUTES:-}" ]; then
	fuzztime="$((FUZZ_MINUTES * 60 / count))s"
fi
echo "fuzzing $count targets for $fuzztime each"
while read -r pkg target; do
	echo "--- $target in $pkg"
	go test -run '^$' -fuzz "^$target\$" -fuzztime "$fuzztime" "$pkg"
done <"$targets"
