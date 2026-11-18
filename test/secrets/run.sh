#!/bin/sh
# Blueprint section 16.2: what the agent and the helper log passes the
# secret scan. It plants made-up secrets where Snugbox looks: a service's
# command line and environment, a crontab line and, when Docker runs, a
# container's environment. Then it runs the watcher, audits in every
# format, the fix list, a dry run of each fix and the report, and searches
# everything Snugbox wrote or logged for the secrets: both services' logs,
# the command's output, the saved reports, the history database and the
# helper's journal. When gitleaks is installed, it scans the same files.
#
# It installs services, so run it as root on a disposable machine with
# systemd, python3 and no Snugbox:
#
#   sudo sh test/secrets/run.sh dist/snugbox_*_linux_amd64.tar.gz
set -eu

[ "$(id -u)" = 0 ] || {
	echo "run it as root; it installs services" >&2
	exit 1
}
if [ $# -ne 1 ] || [ ! -f "$1" ]; then
	echo "usage: $0 path/to/snugbox_<version>_linux_<arch>.tar.gz" >&2
	exit 2
fi
for p in /usr/bin/snugbox /usr/local/bin/snugbox /usr/local/libexec/snugbox /var/lib/snugbox /etc/systemd/system/snugboxd.service; do
	if [ -e "$p" ]; then
		echo "$p exists; this test needs a machine without Snugbox" >&2
		exit 1
	fi
done

libexec=/usr/local/libexec/snugbox
snugbox=/usr/local/bin/snugbox
unit=secret-it.service
cronfile=/etc/cron.d/snugbox-secret-it
work=$(mktemp -d)
out=$work/out
mkdir "$out"
docker_ok=0
if command -v docker >/dev/null 2>&1 && docker info >/dev/null 2>&1; then
	docker_ok=1
fi
since=$(date '+%Y-%m-%d %H:%M:%S')
cleanup() {
	systemctl stop "$unit" 2>/dev/null || true
	rm -f "/etc/systemd/system/$unit" "$cronfile"
	[ "$docker_ok" = 0 ] || docker rm -f secret-it >/dev/null 2>&1 || true
	if [ -x "$snugbox" ]; then
		"$snugbox" watch uninstall --purge --keep-fixes >/dev/null 2>&1 || true
	fi
	systemctl daemon-reload
	rm -rf "$work" "$libexec" "$snugbox" /etc/snugbox /var/lib/snugbox /etc/sysusers.d/snugbox.conf
}
trap cleanup EXIT INT TERM

# Made up here, so no secret is in the repository: a password, and a key in
# the form of an AWS access key ID, which secret scanners look for.
password="snugbox-planted-$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n')"
key="AKIA$(LC_ALL=C tr -dc 'A-Z2-7' </dev/urandom | head -c 16)"

cat >"/etc/systemd/system/$unit" <<EOF
[Service]
Environment=API_TOKEN=$password AWS_ACCESS_KEY_ID=$key
ExecStart=/usr/bin/python3 -c "import time; b = b'x' * (80 << 20); time.sleep(10**6)" --password=$password --key=$key
EOF
systemctl daemon-reload
systemctl start "$unit"
if [ -d /etc/cron.d ]; then
	echo "0 3 * * * root /usr/bin/true --password=$password --key=$key" >"$cronfile"
fi
if [ "$docker_ok" = 1 ]; then
	docker run -d --name secret-it -e "DB_PASSWORD=$password" -e "AWS_ACCESS_KEY_ID=$key" busybox:1.37 sleep 3600 >/dev/null
fi
planted="a service's command line and environment"
[ ! -f "$cronfile" ] || planted="$planted, a crontab line"
[ "$docker_ok" = 0 ] || planted="$planted, a container's environment"
echo "planted secrets in $planted"

tar -xzf "$1" -C "$work"
install -d -m 0755 "$libexec" /usr/local/bin
install -m 0755 "$work/snugbox" "$work/snugboxd" "$work/snugbox-helper" "$libexec/"
ln -sfn ../libexec/snugbox/snugbox "$snugbox"
"$snugbox" watch install >"$out/watch-install.txt" 2>&1
# The first analysis runs once the watcher has sampled for a little while.
sleep 40

(
	cd "$out"
	"$snugbox" audit --for 10s >audit.txt 2>&1 || true
	"$snugbox" audit --json >audit.json 2>&1 || true
	"$snugbox" audit --markdown >audit.md 2>&1 || true
	"$snugbox" audit --share >share.txt 2>&1 || true
	"$snugbox" fit --list >fit.txt 2>&1 || true
	n=$(grep -cE '^  [0-9]+  ' fit.txt || true)
	i=1
	while [ "$i" -le "$n" ]; do
		"$snugbox" fit --dry-run "$i" >"dry-run-$i.txt" 2>&1 || true
		i=$((i + 1))
	done
	"$snugbox" report --week >report.txt 2>&1 || true
	"$snugbox" watch status >status.txt 2>&1 || true
)
journalctl -u snugboxd.service -u snugbox-helper.service --since "$since" --no-pager -o cat >"$out/journal.txt"
cp -r /var/lib/snugbox/reports "$out/" 2>/dev/null || true
cp /var/lib/snugbox/snugbox.db* "$out/" 2>/dev/null || true
[ ! -f /var/lib/snugbox-helper/journal ] || cp /var/lib/snugbox-helper/journal "$out/helper-journal"

failures=0
if grep -q "$unit\|secret-it" "$out/audit.txt" "$out/journal.txt"; then
	echo "ok   Snugbox saw the planted service"
else
	echo "FAIL Snugbox never saw the planted service, so the test proves nothing"
	failures=$((failures + 1))
fi
if found=$(grep -rlaF -e "$password" -e "$key" "$out"); then
	echo "FAIL a planted secret is in: $found"
	failures=$((failures + 1))
else
	echo "ok   no planted secret in $(find "$out" -type f | wc -l) files of output, logs and records"
fi
if command -v gitleaks >/dev/null 2>&1; then
	if gitleaks dir --no-banner --redact "$out" >"$work/gitleaks.txt" 2>&1; then
		echo "ok   gitleaks finds no secret in them"
	else
		echo "FAIL gitleaks finds a secret: $(tail -n 5 "$work/gitleaks.txt")"
		failures=$((failures + 1))
	fi
fi

[ "$failures" -eq 0 ] || {
	echo "$failures failed"
	exit 1
}
echo "all passed"
