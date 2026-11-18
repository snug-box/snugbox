# Service limits

Risk: medium. The same idea as container limits, for systemd services: one runaway service can't take the box down with it.

## What it changes

For each service it covers, Snugbox writes `/etc/systemd/system/<unit>.d/50-snugbox-memory.conf` with:

- `MemoryHigh` at 1.15 times the base below, rounded up to 64 MiB; above it the kernel slows the service and reclaims from it, but doesn't kill it;
- `MemoryMax` at 1.4 times the base, rounded up to 64 MiB and always at least 64 MiB above `MemoryHigh`; it is the hard wall.

The base is the larger of the 99.9th percentile of the service's working set and the highest memory it couldn't give back. Then Snugbox reloads systemd, which puts the limits on the running service at once on systemd 255 and older. systemd 257 only loads them at a reload, so where the service's cgroup doesn't hold them yet, Snugbox sets them on it with `systemctl set-property --runtime` and removes the runtime files that command leaves in `/run/systemd/system.control/`. Either way there is no restart.

## When it is offered

- The service has run for 7 days, or you accept a limit worked out from less with `--accept-low-confidence`.
- It has no `MemoryMax` of its own, isn't a database, and its base is at least 64 MiB; a limit on a tiny service protects nothing.
- Nobody has set `MemoryHigh` or `MemoryMax` on it at runtime with `systemctl set-property --runtime`; such a setting lasts until the next reboot, and Snugbox leaves it alone.
- It isn't sshd, D-Bus, systemd itself, a `user@` unit, Docker, containerd or one of Snugbox's own units, which the helper refuses whatever it is asked. Docker stops every running container when its daemon ends, unless live restore is on, so a limit that killed it would take every container with it.
- Shared memory over 10% of the working set needs swap, as for containers.

## How it is undone

`sudo snugbox undo <transaction>` removes the drop-in, reloads systemd, and puts the service's own limits back on it while it runs, in the same way as above, so nothing of the fix is left. If someone edited the drop-in after Snugbox wrote it, it is left in place and the undo says so; a limit someone set at runtime in the meantime stays too.
