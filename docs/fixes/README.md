# Fixes

A fix is one change from a short catalogue: a swap file with zswap, early out-of-memory protection, memory limits for containers or services, a heap cap for a runtime, or an earlier start for a timer whose job keeps starting together with another heavy job. Nothing outside the catalogue can be applied, and every fix can be put back with `snugbox undo`.

| Fix | Page | Risk |
| --- | --- | --- |
| Swap file plus zswap | [swap](swap.md) | low |
| Early out-of-memory protection | [oom-protection](oom-protection.md) | medium |
| Container limits | [container-limits](container-limits.md) | medium |
| Service limits | [service-limits](service-limits.md) | medium |
| Runtime heap caps | [heap-caps](heap-caps.md) | medium to high |
| Spread colliding schedules | [schedule](schedule.md) | low |

## How a fix runs

1. The watcher must be running, because it is what checks a fix afterwards: `sudo snugbox watch install`.
2. `snugbox fit` lists the fixes the watcher's latest analysis proposes for this box.
3. `snugbox fit --dry-run <number>` shows what the fix changes. With sudo, it asks the helper, which shows every file it would write and every old value it would replace.
4. `sudo snugbox fit <number>` shows the same preview and asks you to type yes; `--yes` skips the question for scripts. Without a terminal and without `--yes`, nothing runs.
5. The helper checks the fix's preconditions, writes a snapshot of everything it will touch, applies the change step by step, and records each step in its journal.

Limits worked out from under 7 days of history are early estimates. `snugbox fit` refuses them unless you add `--accept-low-confidence`.

No fix runs while `snugbox pause` is in force, while the box is under memory pressure, or within 10 minutes of an out-of-memory kill. One fix is verified at a time; a second waits until the first has had its 30 minutes.

## After a fix

For 30 minutes the watcher compares each changed workload with how it behaved before. A limit fix is undone on its own if a changed workload restarts, is killed, stops, or stalls on memory for too much of the last minute. Too much means more than the workload's own 99th percentile over the 7 days before the fix and more than 1%, or more than 5% when there is no history to compare with. `snugbox fit --status` says which it was. Trouble with any other fix in those 30 minutes reopens it for you to decide.

For the next 7 days it keeps watching. If in that time a workload restarts, is killed, or is throttled at a service's MemoryHigh, the fix is reopened for you to decide, rather than undone without you: keep it with `sudo snugbox fit --keep <transaction>`, or put it back with `sudo snugbox undo <transaction>`. A fix counts as done once a full day, nightly jobs included, has passed under it without trouble.

`snugbox fit --status` shows every fix, its state, and what is left of its verification.

## Undo

`sudo snugbox undo` lists the fixes still in place. `sudo snugbox undo --last` puts back the newest, and `sudo snugbox undo <transaction>` a given one. Undo restores each file and setting from the snapshot. A file someone edited after Snugbox wrote it is left as it is, and the undo says so. A folder a fix made for its drop-in, such as `/etc/systemd/system/ssh.service.d`, goes with the last Snugbox drop-in in it; a folder that holds anything else, or that was there before, stays.

Every file a fix writes starts with a comment naming its transaction and the command that takes it out, so you can find Snugbox's changes without Snugbox.

## When a fix was undone on its own

`snugbox fit --status` gives the reason, such as "restarted 2 times" or "stalled on memory 4% of the last minute". The usual causes, and what to do:

- The workload needs more memory at some moment the history didn't show, such as a monthly job. Watch longer before trying again; the next proposal will include that moment.
- The workload's memory grows without bound, a leak. A limit only moves the crash; fix the leak, or keep a limit with `--keep` after an undo was reopened, if a restart at the limit is what you want.
- Two fixes were tried on one workload close together. Only one fix is verified at a time, so try one, wait, then the next.

Pages for problems with the helper itself: [stuck](stuck.md), when it refuses everything, and [the journal](../journal.md).
