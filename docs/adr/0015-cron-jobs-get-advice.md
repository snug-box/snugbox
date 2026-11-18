# Decision: cron jobs that collide get a suggested schedule, never an edit

Status: proposed
Date: 2026-11-01
Deciders: the owner, on the build's proposal

## Context

Blueprint section 3.7 says the schedule fix, for cron, "writes the suggested new line in the report". The timer half of the fix ([ADR 0014](0014-schedule-moves-timers-earlier.md)) finds collisions from the runs of each service, which the watcher records as the service's cgroup appears and goes. Cron jobs have no cgroup of their own: Debian's cron runs them inside `cron.service`, and cronie with `pam_systemd` in a new session scope each time, so the watcher can't tell one job's start from another's.

A crontab says when each job starts, though, and the watcher keeps the box's highest working set for every minute of the last 30 days.

## Decision

The watcher reads the crontabs the installed cron reads, keeping each job's schedule and program name only. Where a cron job started in the same minute as another job on at least 2 of the last 7 days, each time with the box's working set rising by a tenth of RAM within 10 minutes, the schedule fix carries advice: which job to start later, by how much, and the new schedule when Snugbox can write it. The helper never receives cron advice, and nothing edits a crontab.

## Options considered

- Edit the crontab. Users' crontabs belong to `crontab -e`, which locks and signals the daemon; editing them as root behind the user's back, or rewriting `/etc/cron.d` files a package owns, is a change Snugbox couldn't undo cleanly once the user edits the file too.
- Turn the job into a timer. That changes how the job runs, its environment and its mail, which is more than spreading a schedule.
- Count each job's own memory by the programs seen in `cron.service`. The watcher reads a workload's top programs once a minute, so a job shorter than that is missed, and the processes of two jobs in one service can't be told apart reliably.
- Report nothing, as before.

## Consequences

The evidence is the whole box's rise, so a collision of two light jobs during an unrelated spike can be flagged; requiring the rise on two different days makes that rarer, and the advice is only advice. Jobs that run more often than hourly are left out. A cron job can move later, since cron has no memory of its last run; Snugbox moves it later and never touches the timer it met.

## Reversal

Wrong if the advice is often followed and the collisions stay, which would show the box-wide rise isn't telling the jobs apart; then per-job measurement inside `cron.service`, from the processes that appear in it, would be the next step.
