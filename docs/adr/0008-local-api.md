# Decision: the watcher's local API is read-only HTTP and JSON on a Unix socket

Status: proposed
Date: 2026-10-31
Deciders: the owner, on the build's proposal

## Context

The `snugbox` command needs the watcher's history, its latest analysis, its live view and its record of fixes. The helper's protocol uses protobuf frames, and the hub's will use Connect. The local API is the one a person is most likely to read by hand when something looks wrong.

## Decision

`snugboxd` serves `GET /v1/status`, `/v1/result`, `/v1/live`, `/v1/events` and `/v1/changes` as JSON over HTTP on `/run/snugbox/snugboxd.sock`, which only root and the `snugbox` group may open. Nothing it serves changes the server. `?refresh=1` on `/v1/changes` only asks the watcher to read the helper's list again sooner.

## Options considered

- Connect with protobuf, like the hub. One schema for everything, but a person can't read it with `curl --unix-socket`, and the types are Go-only on both ends anyway.
- A TCP port on localhost. Any local user could reach it, and the socket's file permissions would be gone.
- Doing nothing, with the command reading the database itself. The command would need the database code and read access to `/var/lib/snugbox`, and two processes would share one SQLite file.

## Consequences

`curl --unix-socket /run/snugbox/snugboxd.sock http://snugboxd/v1/status` works for debugging. The JSON shapes live in `agent/internal/localapi` and are versioned by the `/v1` prefix.

## Reversal

Cheap: only the `snugbox` command uses it. A new version would be served beside `/v1` for a release before the old one goes.
