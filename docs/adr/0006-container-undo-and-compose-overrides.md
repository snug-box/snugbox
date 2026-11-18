# Decision: undo a container limit by raising it to the maximum, and write Compose overrides as the folder's owner

Status: proposed
Date: 2026-10-31
Deciders: the owner, on the build's proposal

## Context

Most containers start with no memory limit. `docker update` can set one but can't remove it: in moby's `daemon_unix.go` a memory value of -1 is refused with "Minimum memory limit allowed is 6MB", and 0 means "leave unchanged". The undo for a limit set on an unlimited container can't restore "no limit" exactly.

A limit set with `docker update` is lost when the container is created again, so the blueprint keeps it in `compose.override.yaml`, written "as the owner of the Compose project folder and never as root" (section 3.7, rule 6).

## Decision

Undo of a container limit sets `Memory` to the largest 64-bit value, which the kernel reads as `max`, and sets the soft limit to 6 MiB where there was none, the smallest Docker accepts; the undo's notes tell the person both. The override is written by the helper through `systemd-run`, as the folder's owner, sandboxed to that folder, with `O_EXCL` so it never replaces a file. A folder owned by root, a project with its own list of Compose files, or an override that exists already gets the snippet shown for the person to add instead.

## Options considered

- Recreating the container without a limit. Exact, but it restarts the workload, which an undo shouldn't do on its own.
- Leaving the limit in place and only telling the person. Safe, but then undo doesn't undo.
- Writing the override as root with checks against links. The helper would need write access to every place a project can live, such as `/home`, `/srv` and `/opt`, which widens the helper's sandbox far beyond the catalogue.

## Consequences

After an undo, `docker inspect` shows a huge `Memory` value instead of 0. Behaviour is the same as no limit, but a person reading the inspect output needs the note. The helper's sandbox stays limited to `/etc/systemd/system`, `/etc/tmpfiles.d` and zswap's parameters.

## Reversal

Wrong if Docker one day accepts a way to clear a limit; then undo uses it, which is a change to one step. Not yet tested against a live Docker Engine on this build: Docker Desktop was stopped on the build machine, so the CI runner and the test fleet are the first places it runs.
