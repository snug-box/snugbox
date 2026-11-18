# Changelog

Every release of the agent is listed here, newest first. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html). Before 1.0.0, a minor version may change the command line or the JSON report; each such change is listed under its version.

## [Unreleased]

The first release, 0.1.0, will hold everything below.

### Fixed

- Keep retired cgroups out of the kernel peak allowance and count a new workload's startup memory once. Upgrading corrects legacy burst measurements where the retained samples can substantiate the correction.
- Keep the privileged helper asleep between checks when the transaction journal and Docker metadata stay unchanged. Active verification still checks the helper on schedule.

### Added

- `snugbox audit`: a read-only report of what each container, service and process uses, what the box needs, the waste, the risks and the plans that fit, from the kernel's own files. Terminal, Markdown and versioned JSON output, and `--share` for a card with every name removed.
- `schema/audit/v1.json`: a JSON Schema of the JSON report, which a test holds every report to.
- `snugbox top`: a live view of each workload's memory and pressure.
- `snugbox watch`: `snugboxd`, the watcher, samples every 10 seconds, keeps 30 days of history in a capped SQLite file, warns before the box runs out of memory, and can send warnings to a webhook. With a month of history for 50 workloads it stays under its budget of 24 MiB and 0.5% of one CPU core.
- `snugbox fit`, `undo`, `pause` and `resume`: fixes from a fixed catalogue, applied by `snugbox-helper`, a root helper started by its socket, with a snapshot and a hash-chained journal for each change, 30 minutes of verification, and an automatic undo of a limit that hurts.
- The fix catalogue: a swap file with zswap; early out-of-memory protection with systemd-oomd or earlyoom; container limits, kept in a Compose override written as the project folder's owner; service limits by systemd drop-in, put on the running service without a restart also on systemd 257, which no longer applies them at a reload; heap caps for Java, Node.js, Go and glibc programs; and an earlier start for a timer whose job keeps starting together with another heavy job, with a suggested later schedule for a cron job that does.
- Reports that change nothing: crash-looping containers, headless browsers left running, idle containers, each database's memory settings, and PHP-FPM's `pm.max_children`.
- `snugbox plan` and `snugbox doctor`.
- `snugbox report [--week]`: what changed on the box in the last day or week, and the saving from a move to a cheaper plan, counted only once the watcher has seen the box on it. The watcher remembers the box's size, so it sees a resize made while it was stopped.
- `snugbox self-update [--check] [--channel]`: updates a script install from a TUF repository whose root of trust is built into the release, refuses anything older or not matching the signed metadata, and restarts the watcher on the new version. Packages are updated by apt or dnf.
- Packages for amd64 and arm64: `.deb`, `.rpm` and tarballs with SPDX and CycloneDX SBOMs, built reproducibly, signed with cosign through the release workflow's identity, and an install script that checks the signature before it installs anything.

### Known limitations

- Fixes are applied on the box only. Approvals from the dashboard, and `snugbox enroll`, come with the hub.
- The update repository isn't set up yet, so this release carries no root of trust and its `snugbox self-update` says to run the install script again. The staged rollout of self-updates comes with the hub, which can see when updated agents do worse.
- Snugbox never edits a crontab: a cron job that collides gets a suggested schedule for you to write.
- Swap on Btrfs needs btrfs-progs 6.1 or newer.
- Fixes need the kernel's pressure stall information, which RHEL 9 and its rebuilds ship turned off. `snugbox doctor` says how to turn it on: `sudo grubby --update-kernel=ALL --args=psi=1`, then a reboot.
- A box's own swap, such as Fedora's swap on zram, and a systemd-oomd policy the distribution set, such as Fedora's for `system.slice`, are left alone. Where systemd-oomd has such a policy but was stopped, the report says how to start it instead of offering a fix.
