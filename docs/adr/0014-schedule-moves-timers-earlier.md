# Decision: spread colliding jobs by moving a timer earlier, never later

Status: proposed
Date: 2026-10-31
Deciders: the owner, on the build's proposal

## Context

Blueprint section 3.7 asks for a fix that moves a systemd timer with a drop-in, with `RandomizedDelaySec` or a new `OnCalendar`, when two heavy jobs keep starting in the same minute. The obvious move is later: let the first job finish, then start the second.

systemd works out a calendar timer's next run from its last run. In `timer_enter_waiting` the base is the timer's last trigger when it has one, else the time the timer was last started, else now ([timer.c at v255](https://github.com/systemd/systemd/blob/v255/src/core/timer.c) and [on main](https://github.com/systemd/systemd/blob/main/src/core/timer.c), read on 31 October 2026). When a new calendar puts a time between that base and now, the timer fires at once. Measured on Ubuntu 24.04 with systemd 255.4-1ubuntu8.18 on 31 October 2026, with timers enabled the way packages ship them:

- A drop-in and a reload that put a later time on a running timer, a time that had passed since its last run, started the job at once.
- Stopping the timer, reloading and starting it again did the same, persistent or not, and so did clearing a persistent timer's stamp with `systemctl clean`. On this build a stop and a start keep the last trigger. Upstream v255.4 resets it on start; reading its code, it then counts from when the timer was started before, at boot for most timers.
- A second drop-in that held the timer on `OnActiveSec=100y` while it restarted changed nothing either.
- A timer that no other unit wants loses its last trigger when stopped, because systemd frees the unit, which is why a first live test, with timers that were started but not enabled, passed.

A later time always falls between the last run and now, so a later move starts a heavy job at whatever moment someone applies the fix. An earlier time falls there only when the job is due within the move.

## Decision

The schedule fix moves timers earlier. The helper writes the drop-in and reloads, nothing more, and refuses a move when the job is due within it, or within a minute more, saying to apply it after the job has run. The proposer lays a group of colliding jobs out so that each ends before the next starts: the jobs that can't move keep their time, or the longest one when all can move, and the others start before it.

## Options considered

- Move later and accept the extra run on apply. The fix would start a heavy job at an unplanned time, which is the load the fix exists to spread.
- Move later and reset what systemd knows about the last run: touching a persistent timer's stamp before a restart works for persistent timers only and writes systemd's own state; making the timer persistent for the move leaves stamps behind and two reloads; a hold drop-in doesn't work, as measured.
- `RandomizedDelaySec`. The delay is drawn again for every run, so two jobs can still start together on any night, and no one can tell from the unit when a job runs. `FixedRandomDelay=` makes it stable but not chosen.
- Delay the job inside its service, with `ExecStartPre=` and a sleep. The service would sit in activating for minutes, anything ordered after it would wait, and the watcher would still see the two cgroups appear together.
- Doing nothing: report the collision and leave the timers alone.

## Consequences

Applying the fix never starts a job. Undoing it is a later move: when the job has run at its earlier time since the fix, systemd starts it once more at its old time that day, or at once when that time has passed, and the undo says which. A job on some days only can't move back past midnight, and a job that runs every hour moves by less than an hour. Jobs that cron starts are not covered.

## Reversal

Wrong if systemd gains a way to move a running timer's base to now, or if people find that an earlier start breaks an order their jobs depend on, such as a dump that must finish before a backup starts; then a later move offered only together with a stated extra run, or a choice of direction per job, would be the next step.
