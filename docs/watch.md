# Watch mode

`sudo snugbox watch install` starts `snugboxd`, the watcher, and the helper's socket. The watcher samples the box every 10 seconds, keeps 30 days of history in `/var/lib/snugbox`, works out the snug size every 5 minutes, warns before memory runs out, and checks every fix after it is applied. Confidence reaches medium after 7 days of watching and high after 14.

| Command | Does |
| --- | --- |
| `sudo snugbox watch install` | Installs and starts the watcher and the helper's socket, from the package's units when Snugbox came from a package |
| `snugbox watch status` | Says how long it has watched, how much history it keeps, and its latest estimate |
| `sudo snugbox watch uninstall` | Stops and removes both; it refuses while a fix is in place, unless `--keep-fixes`, and keeps the history unless `--purge` |

With the watcher running, `snugbox audit` answers at once from its history instead of sampling for a minute, and `snugbox fit` becomes available.

Reading the watcher needs root or membership of the `snugbox` group: `sudo usermod -aG snugbox <you>` lets you run `snugbox watch status`, `snugbox audit` and `snugbox fit --status` without sudo.

## What the watcher costs

It runs as the `snugbox` user with two capabilities that only read, inside a systemd sandbox, and systemd caps its memory at 64 MiB. Its budget, checked on every change in CI with a month of history for 50 workloads in its store, is a working set under 24 MiB at the 99th percentile and under 0.5% of one CPU core. History stays under 64 MiB on disk.

When a sample takes longer than a second, because the box is very busy, the watcher samples less often, up to once a minute, and goes back to 10 seconds when it can.

## Early warnings

Every minute the watcher looks two hours back and 30 minutes ahead. It warns when the box's total working set, or a workload's working set against its own memory limit, is still rising over the last 10 minutes and is heading over the limit within 30 minutes. The forecast follows the two-hour trend; for the whole box it also takes the highest level the same half hour reached on any of the last 7 days. A warning about the whole box names the workload growing fastest. The box and each workload are judged only once the watcher has 10 minutes of their points, because a service that has just started climbs to its usual size within its first minute, and a trend from that climb alone would reach any limit.

A warning goes to the watcher's log and its event history, and `snugbox report` counts them. To read each one, ask the watcher's socket as root: `curl --unix-socket /run/snugbox/snugboxd.sock http://snugboxd/v1/events`. The same target is warned at most once an hour, and an hour after each warning the watcher records what followed: a kill, memory pressure, or nothing.

With a webhook set, as below, each warning is also posted there once, as JSON:

```json
{
  "box": "web-1",
  "time": "2026-10-31T14:05:00Z",
  "forecast_bytes": 8542224384,
  "limit_bytes": 8343552000,
  "message": "web-1 may run out of memory within 24 minutes: app.service is growing 40 MiB a minute."
}
```

`workload` is added when the warning is about one workload's limit. A webhook that fails or answers with an error is logged and not tried again; the warning stays in the event history.

## Settings

The watcher reads its settings from `/etc/snugbox/snugboxd.env`, which every install puts there with each setting commented out. Only root can read the file, because a webhook address can carry a secret token. After a change, run `sudo systemctl restart snugboxd`.

| Setting | Does |
| --- | --- |
| `SNUGBOX_WEBHOOK_URL=https://...` | Posts each early warning to this address. It must be HTTPS; any other address is refused, and the watcher's log says so when it starts |
| `SNUGBOX_OFFLINE=true` | Never asks the cloud metadata service which provider and region this is. A value that is neither true nor false also counts as offline, with a warning in the log |

An upgrade keeps an edited settings file. `snugbox watch uninstall` leaves it in place; a package purge deletes it.

## Pause

`sudo snugbox pause` is the kill switch. While it is in force, no fix is applied, no automatic undo runs, and nothing goes to a hub. Sampling goes on, so the history has no gap. `sudo snugbox resume` lifts it.

A fix that is being applied when the pause comes stops within a few seconds: the helper looks for the pause every second, stops the step that is running, and puts back what the fix had changed. An undo already under way is left to finish, since it only puts things back.

The pause is a file, `/etc/snugbox/paused`, in a folder only root can write, so the watcher can't lift it, and it survives a reboot. A fix that was being verified when you paused is held: its trouble is noted in `snugbox fit --status`, and acted on after you resume.

## When the RAM changes

Most providers resize a server while it is off. The watcher remembers the box's RAM and CPU count, so when it starts on a box whose size changed while it was stopped, or when a host changes a running virtual machine's memory, it records the resize in its events, with the catalogue's plans for the box before and after when the provider is known, and works the advice out again with the new size.

## What changed: snugbox report

`snugbox report` says what happened in the last 24 hours, and `snugbox report --week` in the last 7 days: what the box needs now and needed at the start, what it saved, the fixes applied, undone or kept, and the out-of-memory kills, early warnings and memory.high hits.

A saving counts only when the box moved to a cheaper plan and the watcher saw it there, as blueprint section 7.5 asks: the monthly price of the plan that matched the box before the resize, less the price of the one that matches it after, from the price list in the release that saw the resize. A fix on its own saves nothing until the plan changes, so the report doesn't count one. When the provider isn't known, because the watcher runs offline or the metadata service gave no answer, a resize is listed without a saving.
