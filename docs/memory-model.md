# How Snugbox decides what a server needs

The memory model turns the kernel's own numbers into one figure, the snug size: the RAM this server needs, with room for its bursts and its file cache. Everything Snugbox advises follows from it, so it is written out here in full, with the constants the code uses, for anyone to check. The code is `agent/internal/model`, which reads no files and keeps no clock, so the same samples always give the same answer.

Every constant below is a starting value. An outside reviewer of Linux memory management is to check them before the watcher's fixes are offered widely, and they will be tuned from what real servers show.

## What is measured

A workload is a container, a systemd service, or a group of processes that belong to neither. Each has its own cgroup, so it is measured from its own files and never inferred from totals.

For every workload, Snugbox reads `memory.current`, `memory.stat`, `memory.events`, `memory.pressure` and, on Linux 5.19 or newer, `memory.peak`. For the server it reads `/proc/meminfo`, `/proc/vmstat`, `/proc/pressure/memory` and `/proc/stat`. Processes in the root cgroup are grouped by program name and measured from `/proc/PID/smaps_rollup`, which has no file cache or pressure figures.

A one-off `snugbox audit` samples every 2 seconds for 60 seconds by default. The watcher, `snugboxd`, samples every 10 seconds and keeps the server's series at 10 seconds for 48 hours and at one minute for 30 days, and each workload's at 10 seconds for 6 hours and as hourly percentile sketches for 30 days. A minute row keeps the largest values of its minute, and a sketch's percentiles are upper bounds within 1%, so a long window errs towards a bigger need.

## Two figures for each workload

```
hard        = anon + shmem + kernel_stack + pagetables + percpu
              + slab_unreclaimable + vmalloc + sock + zswap
working_set = hard + hot_file
hot_file    = active_file + min(inactive_file, file pages refaulted in the last 60 seconds)
```

`hard` is memory that can leave RAM only through swap, or not at all: program heaps and stacks, shared memory and tmpfs, the kernel memory charged to the workload that reclaim can't drop, and the compressed pages in zswap. Reclaimable slab is left out, because the kernel gives it back under pressure.

`hot_file` is the file cache the workload would read back if it were dropped. A refault is a page the kernel evicted and then had to read again, so recent refaults show cache that is really used. A workload that has never come under pressure has never had its cache tested, and its whole active file cache counts as hot. That overstates the need, and when such cache passes a quarter of the need, the confidence drops one level and the report says why.

Refaulted pages may already be back on the active list, so a few pages can count twice. The error is on the side of a bigger need.

## The server's need

Peaks of different workloads rarely land at the same moment, so the server is sized on sums at each moment, not on the sum of each workload's peak. A percentile alone would hide the worst minutes, and out-of-memory kills happen in exactly those minutes, so memory that can't be dropped is sized to its highest value.

```
kernel_other(t) = Slab + PageTables + KernelStack from /proc/meminfo,
                  minus the slab, page tables and stacks the cgroups already charge
hard_total(t)   = sum of hard(t) over all workloads + kernel_other(t)
total(t)        = sum of working_set(t) over all workloads + kernel_other(t)

need     = max(highest hard_total(t), 99.9th percentile of total(t))
headroom = max(10% of need, 256 MiB, 1.2 x the largest rise of total(t) within 60 seconds)
cache    = max(256 MiB, 0.5 x the 99th percentile of the server's hot_file(t))
snug     = need + headroom + cache
```

Percentiles use the nearest-rank method: the smallest value with at least that share of the values at or below it.

- The 60-second rise measures growth in memory use. The starting footprint and growth of a newly seen workload during its first minute count toward observed memory demand. They do not add a second allowance for the same startup in burst headroom. Growth in established workloads still counts, including while another workload starts.
- The cache term depends on how much file cache the server really uses, never on the size of its plan, so it doesn't shrink when Snugbox suggests a smaller plan.

### The memory.peak check

The kernel's `memory.peak` remembers the highest `memory.current` since each cgroup was created, including time before Snugbox watched. Snugbox sums those peaks for the cgroups present at the final sample. It compares that sum with the lower of the largest sampled total for peak-reporting cgroups and the sum of the surviving cgroups' individual sampled maxima. Both figures include all file cache. If the peaks exceed 1.2 times the comparison value, Snugbox adds the difference to headroom. Replaced cgroups still contribute to observed demand while they were running.

The sum of peaks is an upper bound, since the peaks need not have happened together. The check exists for short audits, where the kernel's memory goes further back than the sample.

## How sure Snugbox is

| Watched for | Gaps | Confidence | What the advice says |
| --- | --- | --- | --- |
| Under 7 days, or any window with under 90% of it sampled | any | Low | An early estimate, with a warning to keep watching before changing plans |
| 7 to 14 days | at least 90% sampled | Medium | Plan advice |
| 14 days or more | at least 90% sampled | High | Plan advice with the saving a month and a year |

A gap is a silence longer than three sampling intervals. Limits for `snugbox fit` worked out from under 7 days of history are early estimates, and `snugbox fit` asks before using them.

## When the answer is more, not less

Snugbox never suggests a smaller plan, and says the server needs more memory or a fix, when any of these holds:

- Full memory pressure, the share of time every task stalled on memory, averages over 1%. With 24 hours or more of watching the share is measured over the window; for a shorter look it comes from the kernel's totals since boot, when the server has been up an hour.
- An out-of-memory kill happened in the last 7 days. Kills the kernel counted but whose time the log no longer holds count as recent.
- Pages came back from swap in at least half of the sampling intervals.

## When a plan fits

```
usable RAM >= snug
vCPUs      >= the 95th percentile of CPU in use, in cores
disk       >= 1.3 x the disk in use
transfer   >= 1.2 x the monthly traffic, when the provider caps it
```

Usable RAM is the `MemTotal` a plan really gives, which is less than its advertised size because firmware and the kernel keep some. Until Snugbox has measured a plan, it assumes 93% of the advertised size, converted to bytes with the unit the provider uses. Monthly traffic is estimated from the network interfaces' counters since boot, scaled to 30.44 days, and counts inbound traffic only for providers that bill it.

Fitting plans are sorted by their monthly price, the IPv4 address included where a provider charges for it. Plans a provider no longer sells are listed apart and never ranked.

## A worked example

The numbers are invented to show the arithmetic. An 8 GB server runs a web app, a database and a worker, watched for 14 days:

| Figure | Value |
| --- | --- |
| Highest hard_total | 2.1 GiB |
| 99.9th percentile of total | 2.6 GiB |
| Largest rise within 60 seconds | 0.4 GiB |
| 99th percentile of hot file cache | 0.6 GiB |

- need is the larger of 2.1 and 2.6, so 2.6 GiB.
- headroom is the largest of 0.26 GiB (10% of need), 0.25 GiB (256 MiB) and 0.48 GiB (1.2 x 0.4), so 0.48 GiB.
- cache is the larger of 0.25 GiB and 0.3 GiB (half of 0.6), so 0.3 GiB.
- snug is 2.6 + 0.48 + 0.3 = 3.38 GiB.

A 4 GB plan gives an assumed 3.72 GiB, 93% of 4 GiB, so it fits; a 2 GB plan gives 1.86 GiB and doesn't. On DigitalOcean that is the step from $48 to $24 a month. `TestSizeWorkedExample` in `agent/internal/model` and `TestAdviseWorkedExample` in `agent/internal/plan` compute this example from the code on every change.

## What the model can't see

- Memory a program would use under load it hasn't had while Snugbox watched. A launch, a sale or a yearly batch job won't be in the history.
- What a database would do with less cache. For a server running PostgreSQL, MySQL, MariaDB, Redis, Valkey, MongoDB or ClickHouse, the advice names the setting to check before a smaller plan.
- Memory ballooning by the host. A change of `MemTotal` while the server runs is recorded, and everything that depends on RAM is worked out again.
- Transparent huge pages, which can make anonymous memory larger by up to 2 MiB a mapping. The report shows `AnonHugePages` and flags the setting for databases that ask for `madvise` or `never`.

## Questions for the outside review

- Is `hard` the right set of `memory.stat` fields across kernels 5.4 to 6.x, and is zswap's pool right to count as hard?
- Are 60 seconds of refaults the right window for hot file cache, and is the cap at `inactive_file` sound?
- Do the 99.9th and 99th percentiles, the 1.2 burst factor and the 256 MiB floors give enough room on small servers of 1 to 2 GiB?
- Is 93% of the advertised size a fair default for usable RAM until plans are measured?
