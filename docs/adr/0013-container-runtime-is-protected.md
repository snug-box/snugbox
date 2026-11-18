# Decision: never limit or restart the services of Docker and containerd

Status: proposed
Date: 2026-10-31
Deciders: the owner, on the build's proposal

## Context

Blueprint section 3.7 keeps service limits off sshd, systemd itself and Snugbox. Docker's documentation says that by default, when the Docker daemon terminates, it shuts down running containers, unless live restore is on ([Live restore](https://docs.docker.com/engine/daemon/live-restore/), read on 31 October 2026). A `MemoryMax` on `docker.service` or `containerd.service` that the kernel enforced with a kill, or the restart a heap cap needs, would then stop every container on the box at once. Both daemons are Go programs, so once Snugbox recognised Go programs for heap caps, it would have offered them `GOMEMLIMIT` too.

## Decision

`docker.service` and `containerd.service` join the units no fix may touch, in `helperapi.ProtectedUnit`, which both the proposer and the helper check. They are still measured and reported like any other workload.

## Options considered

- Limit them like any other service, as the blueprint's list allows. One kill takes every container down with it.
- Limit them only when live restore is on. That needs reading `/etc/docker/daemon.json`, and the containers still can't be managed until the daemon is back.
- Doing nothing: leave the proposer and the helper as they were.

## Consequences

A dockerd or containerd that grows is reported but not limited, and the person decides what to do about it.

## Reversal

Wrong if the canary fleet shows that a runaway container daemon harms boxes more often than a limit on it would, for example a dockerd that leaks until the box falls over; then a limit offered only with live restore on would be the next step.
