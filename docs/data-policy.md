# What Snugbox reads, keeps and sends

The short version: Snugbox reads memory figures from the kernel and keeps them on the box. It sends no data anywhere unless you ask it to; on its own, it only asks the cloud provider's metadata service which provider and region the box is in.

## What it reads

- The kernel's memory, pressure and kill figures for the box and for each container, service and process group, and the CPU time each cgroup used: files under `/proc` and `/sys/fs/cgroup`.
- Each workload's name: the container's name, Compose project and service from Docker, the unit name from systemd, or the program's name. Docker's answer about a container also carries its environment; Snugbox's Docker client skips that field and keeps nothing of it.
- For heap caps, a short fixed list of runtime flags from a program's command line, such as Java's `-Xmx`, and for databases the memory settings on the main process's command line, such as PostgreSQL's `shared_buffers=256MB`. Nothing else from command lines is kept, because they can hold passwords, and databases' configuration files are never read.
- For heap caps, whether the main program of a system service or a container is a Go program, and from which Go release: Go writes that into every executable it builds, and Snugbox reads the file's headers and that part only, once per program. A user's own processes are never read this way.
- For a browser process, whether its command line holds `--headless`, to find headless browsers left running after the program that started them stopped. Only that answer is kept.
- For the schedule fix, the watcher reads the timer units, the `.timer` files, in systemd's unit folders under `/etc`, `/run`, `/usr/local/lib`, `/usr/lib` and `/lib`, to learn which timer starts a job, and looks for the drop-in Snugbox writes when it moves one. It uses each file's `Unit=` line and nothing else, and keeps none of it.
- For the same fix, when cron is installed, the watcher reads the crontabs cron reads: `/etc/crontab`, `/etc/cron.d`, and users' crontabs in `/var/spool/cron`. It keeps each job's schedule, its file and line, and the name of the program it runs, in memory only and for the report; the rest of each command line, which can hold a password, is dropped as the line is read, and nothing of it is stored or logged.
- The provider, region and, on AWS, the instance type, from the cloud metadata service at 169.254.169.254, unless it is turned off as below.
- On a root audit, and in the helper before each fix, the out-of-memory reports in the kernel log. The watcher can't read the kernel log; it counts kills from `/proc/vmstat` and each cgroup's `memory.events`.

Besides the kernel's files, its own files, `/etc/os-release`, which names the distribution, the Go build information and the timer units above, Snugbox reads only these, and only when you ask for a fix:

- `/etc/fstab`, before a swap fix, to see whether it lists a swap area that is turned off;
- a service's `Environment=` lines and environment files, before a heap cap, to see whether the service already sets the one variable the cap would set, such as `NODE_OPTIONS`. Nothing read there is kept;
- each file a fix would replace, to keep a copy for undo.

It never reads a running program's environment, application logs, any other file, or process memory.

## What it keeps, and where

| What | Where | How long |
| --- | --- | --- |
| The watcher's history: the box's figures, each workload's figures, when each service started and stopped, events, and each resize of the box with the plans that matched it before and after | `/var/lib/snugbox/snugbox.db` | 10-second points for 48 hours for the box and 6 hours for each workload; the box's minute points, hourly summaries of both, and each service's runs with their peaks, for 30 days; events, resizes and past estimates for 400 days. Never more than 64 MiB |
| The watcher's settings | `/etc/snugbox/snugboxd.env`, readable by root only | Until you delete them; a package purge deletes them |
| Saved audit reports | `/var/lib/snugbox/reports`, readable by root only | Until you delete them; a package purge deletes them |
| The helper's journal and snapshots | `/var/lib/snugbox-helper` | Until you delete them; removing Snugbox leaves them |
| The update metadata `snugbox self-update` has trusted: public keys, versions and hashes, nothing about the box | `/var/lib/snugbox-update`, writable by root only | Until you delete it |

`sudo snugbox watch uninstall --purge`, or a package purge, deletes the watcher's history.

## What it sends

Nothing, apart from these, each of which you can turn off:

- a few requests to the cloud metadata service at 169.254.169.254, on your own provider's network, to learn the provider and region: when an audit samples the box itself, and when the watcher starts and once a day after that. They go over plain HTTP to that one address, follow no redirect, and never ask for user data or credentials. `snugbox audit --offline` turns them off for an audit, and `SNUGBOX_OFFLINE=true` in `/etc/snugbox/snugboxd.env` for the watcher;
- early warnings, as JSON over HTTPS, to the webhook in `SNUGBOX_WEBHOOK_URL`, only if you set one. [Watch mode](watch.md) shows what a warning holds;
- when you run `sudo snugbox self-update`, requests for the update repository's metadata and, if there is a newer release, its tarball. They carry nothing about the box beyond what any download does, and nothing runs them on its own.

The share card that `snugbox audit --share` writes stays on your disk. It has no workload, host or container names.

## With a hub

Snugbox Cloud, where a box sends summaries to a hub for a dashboard, isn't available yet. When it is, nothing leaves a box until its owner enrols it, and this page will list what goes, how long it is kept and how it is deleted. Command lines, environment variables, file contents, logs and process memory will never go to a hub.
