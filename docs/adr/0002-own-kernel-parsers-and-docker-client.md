# Decision: read kernel files and the Docker API with our own small code

Status: proposed
Date: 2026-10-31
Deciders: the owner, on the build's proposal

## Context

The agent reads about twenty kernel files (`/proc/meminfo`, `memory.stat`, pressure files, `smaps_rollup`, the kernel log and others) and asks Docker four things: list containers, inspect one, follow events, and update limits. The code runs with privileges, and the blueprint allows writing the subset we need (section 5.12). The Docker calls Snugbox may make are limited by section 5.4.

## Decision

Snugbox parses the kernel files itself, in `agent/internal/kernel`, with a fuzz target for every parser. It talks to Docker through its own client in `agent/internal/docker`, which has methods for those four calls and no others.

## Options considered

- prometheus/procfs and containerd/cgroups. Mature and widely used, but they parse far more than Snugbox reads and bring their own dependencies into a program that runs as root, and they would still need fuzzing for the parts we use.
- moby/moby/client, Docker's own client. Complete, but it can call everything the Docker API offers, which is exactly what section 5.4 forbids; the limit would be a convention, not something the compiler holds.
- Doing nothing, shelling out to `docker` and `cat`. Slow, fragile across versions, and the output formats are for people.

## Consequences

The formats are flat and stable, so the parsers are short, and each is fuzzed against what it parses. A new kernel field means a change here rather than a dependency update. The Docker client's type has no method that could stop, start or delete a container.

## Reversal

Wrong if kernel formats drift often, which would show as fuzz or golden-file failures on new kernels in the test fleet. Moving to procfs would be a change inside `collect` only, since the rest of the code uses Snugbox's own types.
