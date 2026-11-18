# Decision: let an automatic undo of a limit run under pressure and soon after a kill

Status: proposed, for the outside memory review to confirm
Date: 2026-10-31
Deciders: the owner, on the build's proposal

## Context

Blueprint section 3.7 has two rules that meet. Rule 1: no fix, and no automatic undo, runs while the server is under memory pressure or within 10 minutes of an out-of-memory event. Rule 3: if a changed workload restarts, is killed, or shows pressure above its 7-day 99th percentile in the 30 minutes after a fix, a limit fix is undone automatically. Rule 3's triggers are pressure and kills, the exact conditions in which rule 1 forbids the undo, so read literally a limit that causes a kill can't be undone until 10 quiet minutes later, while it goes on causing kills.

## Decision

The automatic undo of a limit fix runs even while the server is under pressure and right after a kill. Every other fix and every other undo keeps rule 1. `snugbox pause` still stops the automatic undo too.

## Options considered

- Rule 1 as written. Consistent with the document, but it keeps a harmful limit in place during the harm.
- Undo everything under pressure, swap included. Turning swap off under pressure can itself start the kernel's out-of-memory killer, which rule 1 exists to prevent.
- Doing nothing automatically, only reopening the fix for a person. Safe from mistakes by the watcher, but slow when the harm is happening now.

## Consequences

An undo of a limit only ever raises a limit, so it can't add pressure. The pressure gate the helper uses elsewhere is full pressure over 0.1% in the last 10 seconds or some pressure over 1% in the last minute, and a kill blocks other fixes for 10 minutes; both values are starting points for the review.

## Reversal

Wrong if the outside review finds a case where raising a limit under pressure makes things worse, for instance by letting a leaking workload starve the rest. Going back to rule 1 for limits is a one-line change in the helper's gate.
