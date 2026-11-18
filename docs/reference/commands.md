# Command reference

This page is generated from the commands themselves by `go test ./agent/internal/cli -run TestCommandReferenceIsCurrent -update`; edit the commands, not this file.

Every command takes `--help`. Colour is used only on a terminal, and never when `NO_COLOR` is set.

| Exit code | Meaning |
| --- | --- |
| 0 | done |
| 1 | an error |
| 2 | wrong usage |
| 3 | the audit found risks |
| 4 | the command needs root |

## snugbox audit

Report what this box needs, read-only.

```text
snugbox audit [flags]
```

Sample this box's memory for a minute and report what it pays for, what it needs, the waste, the risks, the fixes available and the plans that fit. It changes nothing and sends no data anywhere; unless --offline, it asks the cloud metadata service which provider this is. Run it with sudo for the full report: process details, Docker names and the kernel log need root. Exit code 3 means risks were found.

Examples:

```sh
sudo snugbox audit
sudo snugbox audit --for 10m
sudo snugbox audit --json > audit.json
sudo snugbox audit --share
```

| Flag | Default | Does |
| --- | --- | --- |
| `--for` | `1m0s` | how long to sample, from 10s to 1h |
| `--json` |  | write the versioned JSON report to standard output |
| `--markdown` |  | write the full Markdown report to standard output |
| `--offline` |  | don't ask the cloud metadata service which provider this is |
| `--share` |  | also write a share card with every name removed, as snugbox-card.svg and snugbox-card.txt |

## snugbox doctor

Check the kernel, cgroups, pressure files, zswap, virtualisation and permissions.

```text
snugbox doctor
```

Check whether this box has everything Snugbox reads, and say what is missing. Exit code 1 means a check failed and the audit can't run here.

Examples:

```sh
sudo snugbox doctor
```

## snugbox fit

List, preview or apply the fixes Snugbox proposes for this box.

```text
snugbox fit [<fix>] [flags]
```

snugbox fit lists the fixes the watcher's latest analysis proposes. Name one by its number or its kind to see what it changes, then apply it. Every fix saves the old values first; snugboxd then watches it for 30 minutes and undoes a limit that hurts, and snugbox undo puts any fix back. Kinds: swap, oom-protection, container-limits, service-limits, heap-caps, schedule.

Examples:

```sh
snugbox fit --list
snugbox fit --dry-run 2
sudo snugbox fit service-limits --only app.service
snugbox fit --status
sudo snugbox fit --keep 20261031-103015-service-limits
```

| Flag | Default | Does |
| --- | --- | --- |
| `--accept-low-confidence` |  | use limits worked out from less than 7 days of history |
| `--dry-run` |  | show what the fix changes and change nothing |
| `--keep` |  | keep a fix that was reopened for review, so it counts as done |
| `--list` |  | list the fixes proposed for this box; the same as no arguments |
| `--no-persist` |  | don't write compose.override.yaml; container limits then last until the container is made again |
| `--only` |  | apply the fix to these workloads only, by name, comma-separated |
| `--status` |  | show the fixes applied, being verified or watched |
| `--yes` |  | apply without asking first, for scripts |

## snugbox pause

Stop all fixes and all hub traffic until snugbox resume.

```text
snugbox pause
```

Pause is the kill switch of blueprint section 6.2. While it is in force no fix runs, no automatic undo runs, and nothing is sent to a hub. A fix being applied stops within a few seconds, and what it had changed is put back. Sampling goes on, so history has no gap.

Examples:

```sh
sudo snugbox pause
```

## snugbox plan

Show the plans that fit, from the price list built into this release.

```text
snugbox plan [flags]
```

Sample this box, then list the plans that fit across providers, cheapest first, with the move each one takes. Prices come from the catalogue inside this release; run snugbox version to see which one.

Examples:

```sh
sudo snugbox plan
sudo snugbox plan --provider hetzner
```

| Flag | Default | Does |
| --- | --- | --- |
| `--for` | `1m0s` | how long to sample first, from 10s to 1h |
| `--offline` |  | don't ask the cloud metadata service which provider this is |
| `--provider` |  | show only this provider: digitalocean, hetzner, linode, vultr or aws |

## snugbox report

Say what changed on this box and what it saved.

```text
snugbox report [flags]
```

snugbox report reads the watcher's history for the last 24 hours, or the last 7 days with --week: what the box needs now and at the start, the saving from a move to a cheaper plan, the fixes applied, undone or kept, and the out-of-memory kills, early warnings and memory.high hits. A saving counts only once the watcher has seen the box on its new plan.

Examples:

```sh
snugbox report
sudo snugbox report --week
```

| Flag | Default | Does |
| --- | --- | --- |
| `--week` |  | cover the last 7 days instead of the last 24 hours |

## snugbox resume

Let fixes and hub traffic run again after snugbox pause.

```text
snugbox resume
```

Examples:

```sh
sudo snugbox resume
```

## snugbox self-update

Update Snugbox to the newest release, verified through The Update Framework.

```text
snugbox self-update [flags]
```

snugbox self-update updates a Snugbox that came from the install script. It fetches the update repository's signed metadata, checks it against the root of trust built into this release, and installs a newer release only when its every byte matches that metadata. It never installs an older release. The watcher is restarted on the new version. A Snugbox from a .deb or .rpm package is updated by apt or dnf instead.

Examples:

```sh
sudo snugbox self-update --check
sudo snugbox self-update
sudo snugbox self-update --channel beta
```

| Flag | Default | Does |
| --- | --- | --- |
| `--channel` | `stable` | the release channel: stable, beta or nightly |
| `--check` |  | say whether a newer release is out, and change nothing |

## snugbox top

Live view of workloads, memory and pressure.

```text
snugbox top
```

Show every workload's working set, the memory it can't give back, its hot file cache, its limit and how much of the time it waits for memory, refreshed every 2 seconds. Keys: s changes the sort, up and down scroll, q quits.

Examples:

```sh
sudo snugbox top
```

## snugbox undo

Put a fix back.

```text
snugbox undo [--last] [<transaction>] [flags]
```

snugbox undo puts back what a fix changed, from the snapshot taken before it ran, and records that in the journal. A file someone edited since is left as it is, and swap is turned off only when the box has the free memory to take what is in it.

Examples:

```sh
sudo snugbox undo --last
sudo snugbox undo 20261031-103015-swap
```

| Flag | Default | Does |
| --- | --- | --- |
| `--last` |  | put back the newest fix that is still in place |

## snugbox version

Print the version.

```text
snugbox version
```

## snugbox watch

Run the watcher as a service, see how it is, or remove it.

```text
snugbox watch
```

snugboxd samples the box every 10 seconds, keeps 30 days of history and works out the snug size every 5 minutes. Confidence reaches medium after 7 days and high after 14.

## snugbox watch install

Install and start snugboxd and the helper that applies fixes.

```text
snugbox watch install
```

Examples:

```sh
sudo snugbox watch install
```

## snugbox watch status

Show whether snugboxd runs and what it has seen.

```text
snugbox watch status
```

## snugbox watch uninstall

Stop snugboxd and the helper and remove their services.

```text
snugbox watch uninstall [flags]
```

Removes the watcher and the helper. A fix still in place needs the helper to be undone, so uninstall stops if there is one, unless --keep-fixes says to leave the fixes as they are. The helper's journal and snapshots in /var/lib/snugbox-helper are always kept.

Examples:

```sh
sudo snugbox watch uninstall
sudo snugbox watch uninstall --purge
```

| Flag | Default | Does |
| --- | --- | --- |
| `--keep-fixes` |  | remove Snugbox even though fixes are in place, leaving them as they are |
| `--purge` |  | also delete the watcher's history |
