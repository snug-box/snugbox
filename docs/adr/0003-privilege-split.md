# Decision: an unprivileged watcher and a root helper started by its socket

Status: proposed
Date: 2026-10-31
Deciders: the owner, on the build's proposal

## Context

Watching memory needs to read every process's memory map and every cgroup file. Fixing it needs root: writing systemd drop-ins, calling `docker update`, making a swap file, writing zswap's parameters. The watcher runs all the time and parses input from every process on the server; the fixes run rarely and are what an attacker would want. Blueprint section 5 asks for the split.

## Decision

`snugboxd` runs as the `snugbox` user with two capabilities that only read, `CAP_SYS_PTRACE` and `CAP_DAC_READ_SEARCH`, under a systemd sandbox. `snugbox-helper` runs as root only while there is work: systemd starts it when a request arrives on `/run/snugbox/helper.sock` and it exits after 60 seconds idle. It accepts requests from root and from the `snugbox` user only, checks the sender of every message with `SCM_CREDENTIALS`, refuses messages that carry file descriptors, and runs nothing outside the fix catalogue. Its unit may write only the paths the catalogue needs, and a test fails if the two drift apart. Only root may apply a fix; the watcher may list fixes, move one on after watching it, and undo a limit fix that is still being verified.

## Options considered

- One daemon running as root. Simplest to build, but every parser bug would run as root.
- The watcher with `CAP_SYS_ADMIN` and friends, applying fixes itself. Fewer moving parts, but the capabilities needed to write systemd and sysfs files are close to root anyway, held all the time.
- A long-running root helper. Answers faster, but holds root while idle for no benefit; fixes are rare.
- Doing nothing, leaving fixes to the person. The audit would still help, but the blueprint's phase 2 is the fixes.

## Consequences

`systemd-analyze security` scores the watcher at 2.2 and the helper at 1.5 on Ubuntu 24.04's systemd 255, against the blueprint's target of under 6.0. The helper is about 3,100 lines without blanks or comments, a little over the blueprint's 3,000, and it is the code the outside security review should read first. A compromised watcher can ask for only what the watcher may ask: a list, a step forward, or the undo of a limit in verification.

## Reversal

Wrong if socket activation proves unreliable on a supported distribution, which the package test would show. Moving to a long-running helper is a change to its unit and its accept loop; the protocol stays.
