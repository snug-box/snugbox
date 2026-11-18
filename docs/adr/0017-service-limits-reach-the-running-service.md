# Decision: put service limits on the running service when the reload doesn't

Status: proposed
Date: 2026-11-03
Deciders: the owner, on the build's proposal
Amends: [0005](0005-service-limits-by-drop-in.md)

## Context

ADR 0005 applies a service limit with a drop-in and `systemctl daemon-reload`, measured on systemd 255, and names its own reversal: "Wrong if some systemd version applies drop-in limits only on restart; the live tests on each distribution in the test fleet would show it."

They did. On 3 November 2026, in a Debian 13 virtual machine with systemd 257.13 and kernel 6.12 (`test/vm/host.sh debian13`), a drop-in with `MemoryHigh` and `MemoryMax` plus a reload changed what `systemctl show` reports, but `memory.high` and `memory.max` in the running service's cgroup stayed at `max`, also 5 seconds later. Removing the drop-in and reloading left a limit already in the cgroup in place. So the fix failed its check after applying, and on a box upgraded to systemd 257 with a limit in place, an undo would have reported success while the service kept the limit until its next restart. Debian 12 (systemd 252) and Ubuntu 24.04 (systemd 255) apply the reload at once, as before. systemd's own documentation allows both: after a reload the new configuration "might not be in effect immediately" (systemd.unit(5), checked 3 November 2026). No upstream changelog entry for the change was found.

`systemctl set-property --runtime` changes a running unit's cgroup without a restart on every version measured, but writes one file per setting to `/run/systemd/system.control/<unit>.d/`, `50-MemoryHigh.conf` and `50-MemoryMax.conf`, which stay until the next reboot. Drop-ins with different names apply in lexicographic order whatever their folder (systemd.unit(5)), so those files sort before Snugbox's `50-snugbox-memory.conf` and would hold the value after an undo removed it.

## Decision

The service limits fix keeps the drop-in and the reload, then runs a new step for each service: if the cgroup already holds the limits, it does nothing; otherwise it calls `systemctl set-property --runtime` for the settings that differ, removes the runtime files that call wrote, and reloads so systemd forgets them. Undo, rollback and recovery remove the drop-in, reload, and do the same with the service's own limits, read with `systemctl show`, so a snapshot from before this step existed is put back on the running service too. After a crash between `set-property` and the removal, recovery removes runtime files holding the step's values or `infinity`; one holding any other value was written by a person and stays, with a note. The fix refuses a service that already has a runtime setting for either value.

The helper's unit gains `ReadWritePaths=-/run/systemd/system.control`, and `ExecStartPre=-+/usr/bin/mkdir -p /run/systemd/system.control`, because systemd makes the folder only when first needed and `ReadWritePaths=` skips a folder missing when the sandbox is built. Both lines start with `-` so the helper still starts, and can still undo, if the folder can't be made.

## Options considered

- Restart the service so it starts with the drop-in. One step, but it interrupts the workload the limit is meant to protect, and the fix promised no restart.
- `set-property --runtime` and leave its files. Undo would then leave a runtime copy of the old value, which overrides the service's own unit file until the next reboot, against the rule that an undo leaves nothing of Snugbox behind.
- Remove the files through a transient `systemd-run` unit that may write only that folder, as the Compose override writer does. Keeps the helper's own sandbox as it was, at the cost of one more transient unit per call and a path the fake box in the unit tests can't follow as directly.
- A tmpfiles.d entry to create the folder at boot. Needs a file in each package and in a script install, and a `systemd-tmpfiles` call at install time; `mkdir -p` in the unit does the same in one line.
- Write `memory.max` directly. systemd owns the cgroup tree and may overwrite it; writing there also needs `ProtectControlGroups=` lifted.

## Consequences

On systemd up to 255 nothing changes: the cgroup holds the limits after the reload and no `set-property` call is made. On 257 each apply and each undo costs one `set-property` call and one more reload per service. The helper may now write one more folder, holding only runtime settings and emptied again by every step. `TestUnitWritePaths` keeps the unit and the code's list equal.

## Reversal

If a later systemd puts drop-in limits on the running service at the reload again, the step finds the cgroup right and does nothing, so nothing has to be reversed. Dropping the step is a change to one plan and one unit line.
