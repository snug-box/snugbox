# Decision: keep the watcher within its budget on a full store

Status: proposed
Date: 2026-11-04
Deciders: the owner, on the build's proposal
Amends: [0009](0009-history-store.md)

## Context

Blueprint section 9.4 holds snugboxd to a working set under 24 MiB at the 99th percentile and under 0.5% of one CPU core, with 50 workloads. CI measured that only on a new box's empty store. On 3 November 2026 the soak test filled a store the way 30 days of 50 workloads do (14.6 MiB), and on VPS A (Oracle, 2 vCPUs, 1 GiB) snugboxd on it used 1.365% of a core. With the early warnings' two hours full, its working set reached 25.9 MiB.

Three things cost the most:

- Every 5 minutes the summary read all 30 days row by row: 283 ms and 50 MB allocated on a desktop. The workload tables' keys start with the workload, so a time range was a full scan.
- Every minute the early warnings read two hours of every workload's 10-second points from the store: 126 ms and 14 MB.
- modernc.org/memory, the C allocator under the pure-Go SQLite driver, keeps freed memory for reuse, up to 4 MiB for each size class, and gives each 4 KiB page of SQLite's page cache an 8 KiB slot. One sort for a GROUP BY or an ORDER BY, or the temporary table of a window function, took a megabyte or two and stayed resident after it was freed. The allocator held 11.9 MiB while SQLite used 2.1 MiB.

## Decision

- Whole UTC days that ended at least an hour before the window's end are summed once and kept in memory, so a summary reads only the rows of the days at the window's two ends. Prune clears the cache after a trim reaches into those days.
- Schema version 6 adds indexes on the two workload tables' times.
- The summary's reads take their rows in an order a key or an index gives. Hourly rows come by the time index and are summed in Go; the gaps between minute rows are counted in Go from their times; the warnings' points come in time order. `TestWindowReadsNeedNoSort` fails on any of these plans that has a temporary B-tree.
- SQLite's page cache is 512 KiB instead of 2 MiB.
- The early warnings read their two hours from the store once, then keep them up from each sample, with times as Unix seconds.
- CI and `make live-tests` measure the budgets on a month of history that the soak test writes first.

## Options considered

- Daily rollup tables in the store. They would survive a restart, but they mean a new schema and a second write path for sums the cache rebuilds in about 300 ms.
- Summaries less often than every 5 minutes. The blueprint asks for every 5 minutes.
- A larger budget. A tool that sells memory savings has to use almost none; the budget is the product's own claim.
- Memory-mapped reads (`PRAGMA mmap_size`) in place of the page cache. Pages would come from the kernel's file cache, but an I/O error on a mapped page kills the process.
- SQLite's soft heap limit. This build of SQLite turns memory statistics off, and the limit isn't enforced without them.

## Consequences

On the same store with the early warnings' window full, the working set's 99th percentile on VPS A fell from 25.9 MiB to 17.1 MiB over 12 minutes, with SQLite using 0.58 MiB and its allocator holding 3.1 MiB. A 30-day summary from the cache takes 12 ms on a desktop, and the first one after a start, which fills the cache, about 300 ms. The indexes add 1.7 MiB to a full store. A summary reads at most two days of rows every 5 minutes, and all 30 days once a day when the cache moves on.

## Reversal

Wrong if a summary from the cache ever differs from one read row by row. `TestSummaryMatchesReference` and `TestSummaryMatchesReferenceOverDays` compare the two exactly, on windows that cut through days, a gap across midnight and a trim. Dropping the cache is a change inside the store package, and a later migration can drop the indexes.
