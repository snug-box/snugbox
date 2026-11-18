# Spread colliding schedules

Risk: low. Two heavy jobs that systemd timers start at the same moment, such as a backup and a database dump both at 02:00, need their memory at the same time. Starting one of them a little earlier lets it finish before the other starts, so the box needs less at its busiest minute.

## What it changes

For each timer it moves, Snugbox writes `/etc/systemd/system/<timer>.d/50-snugbox-schedule.conf`, which clears the timer's calendar and gives it the same one, a number of minutes earlier:

```ini
[Timer]
OnCalendar=
OnCalendar=*-*-* 01:50:00
```

Then `systemctl daemon-reload` applies it. Snugbox takes the calendar from systemd, which writes it in a form of its own where `daily` reads `*-*-* 00:00:00`, and changes only the time of day; weekdays, dates and a time zone stay as they are. After the reload it asks systemd again, and puts everything back if the timer doesn't fire at the new time. That happens when a drop-in of your own, later in the alphabet, sets `OnCalendar` as well.

Jobs that keep starting together are laid out so that each ends before the next starts. The jobs Snugbox can't move keep their time; when it can move them all, the one that runs longest keeps it. The others start earlier: those that run most often nearest that time and, among jobs that run as often, the heaviest nearest. Each job that moves starts its own longest run, rounded up to 5 minutes, and 5 minutes more, before the job after it, and 5 minutes earlier again while that would start it within a minute of another heavy job. No timer moves more than 2 hours, and one fix moves at most 16 timers.

## Why earlier and never later

systemd works out when a timer fires next from when it last fired, and it keeps that time when the timer is stopped and started again. When a new calendar puts a time between the last run and now, systemd starts the job at once. A later time always falls in that gap: a backup that ran at 02:00 and moved to 02:20 at 10:00 would start again at 10:00. An earlier time falls there only when the job is due within the move, and Snugbox refuses the move then, so after a move the job next starts at its new time.

## When it is offered

- The watcher saw two services start within a minute of each other on at least 2 of the last 7 days, and their peaks added up came to a tenth of the box's RAM or more. Only the watcher keeps the history this needs, so a one-off `snugbox audit` never offers it.
- A run of 6 hours or less counts as a job. A service that runs longer isn't one.
- A timer starts the job: Snugbox looks for it among the timer units in `/etc/systemd/system`, `/run/systemd/system`, `/usr/local/lib/systemd/system`, `/usr/lib/systemd/system` and `/lib/systemd/system`, where a timer starts the unit its `Unit=` names, or the service of its own name. A job two timers start is left alone, because moving one would leave it starting at the other's time.
- The timer fires on its calendar alone: exactly one `OnCalendar`, and no `OnBootSec`, `OnUnitActiveSec` or other time after boot or after a run, which a new calendar wouldn't move.
- Its time has a fixed minute, and an hour that is fixed, a list or every hour (`*`). A calendar for every day may move back past midnight, so `daily` moved 20 minutes becomes `*-*-* 23:40:00`, and one for every hour may move back past the hour. Any other keeps to its own days and hours: a job at 00:10 on weekdays can't move to the evening before. For a calendar such as `*:0/15`, Snugbox says why it won't move it; `RandomizedDelaySec` in a drop-in of your own spreads such a timer instead.
- A job never moves by as long as it waits between its own runs, so an hourly job moves by less than an hour.
- The job isn't due within the move, or within a minute more. At 01:55, a job due at 02:00 can't move 20 minutes earlier: its new time would have passed, and systemd would start it at once. Snugbox says to apply the fix after the job has run.
- Neither the timer nor its job is one a fix may never touch: sshd, D-Bus, systemd's own units, Docker, containerd or Snugbox's.
- Snugbox hasn't moved the timer already. Runs from before a move are left out, since they show the old time.

## Jobs that cron starts

Cron runs its jobs inside its own service or a login session, so the watcher can't tell when each of them started, and Snugbox never edits a crontab. Instead the report, `snugbox fit --list` and `snugbox fit schedule` name a cron job that keeps starting together with another heavy job, and suggest a new schedule for it:

```text
logrotate, line 2 of /etc/cron.d/shop: change its schedule from "0 2 * * *" to "25 2 * * *", 25 minutes later.
It started at 02:00 together with backup.sh on 3 of the last 7 days, and the box's memory rose 400 MiB within minutes.
```

The watcher reads the crontabs cron reads: `/etc/crontab`, the files in `/etc/cron.d` (on Debian and Ubuntu only names made of letters, digits, `_` and `-`, as cron there does), and each user's crontab in `/var/spool/cron/crontabs` on Debian and Ubuntu or `/var/spool/cron` elsewhere. It keeps each job's schedule and the name of its program, never the rest of the command line, which can hold a password. A crontab that sets `CRON_TZ` or `TZ` is left out, since its times are in another zone.

A cron job gets a suggestion when:

- It started in the same minute as another job, cron's or a timer's, on at least 2 of the last 7 days, and each time the box's working set rose by a tenth of its RAM or more within 10 minutes. The watcher can't split that rise between the jobs, so it counts the rise of the whole box.
- It runs once an hour or less often. A job that runs more often meets every other job, and is rarely the heavy one.
- A timer's job keeps its time, and so does the first cron job in the files when no timer is part of it. The others start later, each by as long as memory stayed high, rounded up to 5 minutes, and 5 minutes more, after the one before, and 5 minutes later again while that would start it within a minute of another heavy job. No job moves more than 2 hours, or by as long as it waits between its own runs.

Unlike a timer, a cron job can move later: cron starts a job when its crontab says, without counting from its last run. Snugbox writes the new schedule out when the minute and the hour are plain numbers, or the minute is and the job runs every hour; a job on some days only isn't moved past midnight. For any other schedule, such as `0,30 2 * * 1-5`, it says how many minutes later to start the job, and you write the schedule.

Snugbox doesn't use `RandomizedDelaySec` itself. The delay is drawn again for every run, so two jobs can still land together on any night, and no one can tell from the unit file when a job will run.

## After it is applied

systemd uses the new time from the timer's next run on, and `systemctl list-timers` shows it at once. Like every fix, it has 30 minutes of verification, during which no other fix starts. A timer move changes no limit, so the watcher never undoes it on its own.

## How it is undone

`sudo snugbox undo <transaction>` removes the drop-in and reloads systemd, which puts the timer back on its old calendar. If someone edited the drop-in after Snugbox wrote it, it is left in place and the undo says so.

Putting the later time back is the move systemd catches up on. If the job has run at its earlier time since the fix, systemd counts from that run, so the job runs once more at its old time that day, or at once if that time has passed; the undo says which. If the job hasn't run at its new time yet, the undo starts nothing.
