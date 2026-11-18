# Decision: keep the helper's journal, snapshots and swap file in /var/lib/snugbox-helper

Status: proposed
Date: 2026-10-31
Deciders: the owner, on the build's proposal

## Context

The blueprint puts the journal in `/var/lib/snugbox/journal` (section 3.7, rule 5) and the swap file at `/var/swap.snugbox`. But `/var/lib/snugbox` belongs to the `snugbox` user, through systemd's `StateDirectory`, and root restores snapshots from the undo folder. If the watcher's user can write where root reads snapshots from, a compromised watcher could plant a snapshot that root then restores. A swap file in `/var` would also need the helper to write to `/var`.

## Decision

The helper's state lives in `/var/lib/snugbox-helper`, owned by root with mode 0700: the journal (0600), `undo/<transaction>/manifest.json` with the copies it restores, and the swap file. systemd creates the folder from the helper unit's `StateDirectory`. The swap unit is `var-lib-snugbox\x2dhelper-swapfile.swap`, and zswap's settings are kept across reboots by `/etc/tmpfiles.d/snugbox-zswap.conf`.

## Options considered

- The blueprint's paths. They match the document, but give the watcher's user a way to feed root.
- `/var/lib/snugbox` owned by root, with the watcher writing elsewhere. Works, but systemd's `StateDirectory` chowns the tree to the service's user, so it would fight the unit file.
- Doing nothing about it. Leaves the planted-snapshot path open.

## Consequences

Removing the package never deletes this folder, not even a purge, because a fix still in place needs its snapshot to be undone. The documentation and the removal scripts say so.

## Reversal

Cheap before the first release; after it, a move needs a migration in the helper that carries the journal's hash chain over unchanged.
