#!/bin/bash
# One-time setup of snugbox.dev on this server; safe to run again. Installs
# Caddy from its official apt repository and lets unattended-upgrades update
# it, lays out /srv/snugbox.dev, installs the Caddyfile and the snugbox-site
# command, and deploys the site build given as $1.
# Run as root from the folder holding these files: bash setup.sh site.tgz
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
site=${1:?usage: setup.sh <site.tgz>}
export DEBIAN_FRONTEND=noninteractive NEEDRESTART_MODE=l

# Caddy, as https://caddyserver.com/docs/install says for Debian and Ubuntu.
if ! command -v caddy >/dev/null; then
	apt-get install -y -q debian-keyring debian-archive-keyring apt-transport-https curl gnupg >/dev/null
	curl -1sLf https://dl.cloudsmith.io/public/caddy/stable/gpg.key |
		gpg --dearmor --yes -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg
	curl -1sLf https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt >/etc/apt/sources.list.d/caddy-stable.list
	chmod o+r /usr/share/keyrings/caddy-stable-archive-keyring.gpg /etc/apt/sources.list.d/caddy-stable.list
	apt-get update -q >/dev/null
	apt-get install -y -q caddy >/dev/null
fi

# unattended-upgrades only takes Ubuntu's own archives unless told otherwise.
origin=$(grep -h '^Origin:' /var/lib/apt/lists/*cloudsmith*caddy*Release 2>/dev/null | head -n 1 | cut -d' ' -f2-)
if [ -n "$origin" ]; then
	printf '// Let unattended-upgrades install Caddy updates from its official repository.\nUnattended-Upgrade::Origins-Pattern:: "origin=%s";\n' "$origin" \
		>/etc/apt/apt.conf.d/52unattended-upgrades-caddy
fi

install -d -m 0755 /srv/snugbox.dev /srv/snugbox.dev/releases
install -m 0755 "$here/snugbox-site" /usr/local/sbin/snugbox-site
install -m 0644 "$here/README.md" /srv/README.md
[ -f /etc/caddy/Caddyfile.dist ] || cp /etc/caddy/Caddyfile /etc/caddy/Caddyfile.dist
install -m 0644 "$here/Caddyfile" /etc/caddy/Caddyfile.new
runuser -u caddy -- caddy validate --config /etc/caddy/Caddyfile.new --adapter caddyfile
mv /etc/caddy/Caddyfile.new /etc/caddy/Caddyfile

/usr/local/sbin/snugbox-site deploy "$site"
systemctl reload caddy
echo "caddy $(caddy version | cut -d' ' -f1), $(systemctl is-active caddy), unattended origin: ${origin:-none}"
