# The snugbox.dev server

A Linux server (Ubuntu 24.04) that serves snugbox.dev with Caddy, behind the
Cloudflare proxy. `setup.sh` in this folder sets it up and can be run again.

- Web server: Caddy from its official apt repository. unattended-upgrades
  updates it (/etc/apt/apt.conf.d/52unattended-upgrades-caddy).
- Config: /etc/caddy/Caddyfile, from `Caddyfile` here. The stock file is kept
  as Caddyfile.dist.
- Certificates: Caddy gets and renews them from Let's Encrypt on its own. The
  Cloudflare SSL/TLS mode must be Full (strict).
- Site files: /srv/snugbox.dev/releases/<UTC time>/. The live one is the
  symlink /srv/snugbox.dev/current. The newest 5 releases are kept.
- Access log: /var/log/caddy/snugbox.dev.log, without request headers, so no
  visitor IP address or user agent is stored. Kept 30 days at most.
- If snugbox.dev shows Cloudflare error 521 or 522, the server is down or
  Caddy is stopped.

Commands on the server:

    sudo snugbox-site deploy /tmp/site.tgz   # make a build live
    sudo snugbox-site rollback               # back to the release before
    snugbox-site list                        # releases, and which is live

From a checkout, `pwsh -File site/deploy.ps1` builds the site and deploys it.
