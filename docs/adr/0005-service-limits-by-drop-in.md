# Decision: apply service limits with a drop-in and a daemon reload

Status: proposed; amended by [0017](0017-service-limits-reach-the-running-service.md), since systemd 257 no longer applies the limits at the reload
Date: 2026-10-31
Deciders: the owner, on the build's proposal

## Context

The blueprint applies a service limit "at once with `systemctl set-property --runtime` and kept in the drop-in" (section 3.7). `set-property --runtime` writes its own file under `/run/systemd/system.control/`, which takes precedence over drop-ins in `/etc` until the next reboot. An undo that removes Snugbox's drop-in would then leave the runtime value in force, and so would a person's own later change in `/etc`.

On 31 October 2026, in WSL with systemd 255, writing a drop-in with `MemoryHigh` and `MemoryMax` and running `systemctl daemon-reload` set `memory.high` and `memory.max` on the running service at once, without a restart, and removing the drop-in and reloading put both back to `max`.

## Decision

The helper writes `/etc/systemd/system/<unit>.d/50-snugbox-memory.conf` and runs `systemctl daemon-reload`. It never calls `set-property`. Undo removes the file and reloads.

## Options considered

- The blueprint's `set-property --runtime` plus a drop-in. Works on first apply, but leaves a second copy of the setting that shadows later changes until reboot.
- `set-property` without `--runtime`. It writes its own persistent file in `system.control`, which isn't where a person would look for it.
- Doing nothing, leaving limits to the person. Service limits are one of the main fixes.

## Consequences

One file holds the whole change, and it begins with a comment naming the transaction, so a person who finds it knows where it came from and how to remove it. A reload touches all units, which is cheap but not free on servers with thousands of units.

## Reversal

Wrong if some systemd version applies drop-in limits only on restart; the live tests on each distribution in the test fleet would show it. Changing back is a change to one step in the helper.
