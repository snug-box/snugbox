# Your first audit, and how to read it

```sh
sudo snugbox audit
```

The audit samples the box every 2 seconds for a minute and changes nothing. It reads the kernel's own files for every container, service and process, and also what the kernel already remembers: `memory.peak` for each workload since it started, on Linux 5.19 or newer, pressure totals since boot, out-of-memory kills in the kernel log, and uptime. That is why one minute can say something useful, and also why its confidence is low.

Without sudo the audit still runs, but it can't read other users' processes, Docker's names or the kernel log, and the report says what it missed. With the watcher running, the audit answers at once from the watcher's history; `--for 10m` takes a fresh sample of that length instead.

## The report

The terminal report comes in sections, and the first three lines answer the main question.

SUMMARY. What the box pays for, from the cloud provider's metadata when it has one, or else the RAM it has; what it needs, the snug size, with its parts; the waste between the two; the plan that fits; and the confidence, with what it would take to raise it.

RISKS. Out-of-memory kills, missing swap, no early out-of-memory protection, containers without a memory limit, memory pressure, settings that hurt a database, containers Docker keeps restarting, and headless browsers still running after the program that started them stopped, each with a `!`. A container counts as restarting when Docker's restart policy has started it at least 3 times and it is waiting to start again or started within the last 10 minutes. A line starting `ok` says what was checked and found fine.

WORKLOADS. Each workload's 99th percentile working set, its peak, and its limit if it has one, largest first, then a line for the kernel and everything else. Workloads under 1 MiB are left out of the terminal and counted on one line; the Markdown and JSON reports list every workload. Containers that used under 0.1% of one CPU core over a week or more of watching are named on a line of their own, with the memory they hold, since stopping one gives that memory back; only the watcher has the history to say so.

FIXES. What `snugbox fit` could change, numbered, with each fix's risk. Limits from under 7 days of data are marked as early estimates. Below them, a note for each database says what it holds and how large its own memory settings may grow on the plan that fits, such as `shared_buffers` for PostgreSQL or `maxmemory` for Redis, with the database's documented rule of thumb. The plan was sized from what the database holds now, so a cache set larger than that can outgrow it later. Snugbox shows a setting's value only when it is on the database's command line; it doesn't read databases' configuration files.

PLANS THAT FIT. The cheapest plans across the providers whose prices ship in this release, with what each would save and what moving takes: a resize in place or a new server. Plans a provider no longer sells are named on a line of their own and never ranked.

DATA QUALITY. How long and how often the box was sampled, any gaps, which kernel features were there, and what couldn't be read and why.

The memory model behind every number is written out in full in [the memory model](memory-model.md).

## The files it leaves

When it prints the terminal report, a root audit also saves the full report as Markdown in `/var/lib/snugbox/reports/audit-<time>.md`, with the time in UTC. The file belongs to root, and only root can read it. Apart from that file and the share card, an audit writes nothing.

| Option | Does |
| --- | --- |
| `--json` | Writes the report as JSON to standard output; its `schema` field names the version, and a field is removed only in a new major version. The JSON Schema in [`schema/audit/v1.json`](../schema/audit/v1.json) describes every field, under the same URL as its `$id` |
| `--markdown` | Writes the Markdown report to standard output |
| `--share` | Writes `snugbox-card.svg` and `snugbox-card.txt` in the current folder: the headline numbers with every name removed, workloads called by their role, such as "database service". Nothing is uploaded |
| `--offline` | Skips the cloud metadata service, so the audit makes no network request at all |

## Exit codes

0 means the audit ran and found no risk, 3 that it ran and found at least one, which monitoring scripts can test for. 1 is an error, 2 a mistake in the command line, 4 a command that needs root.
