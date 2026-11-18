# Decision: keep history in SQLite, with hourly sketches for each workload

Status: proposed
Date: 2026-10-31
Deciders: the owner, on the build's proposal

## Context

The blueprint keeps the server's series at 10 seconds for 48 hours and at one minute for 30 days, under 64 MiB of disk (sections 2 and 9.4). Fifty workloads at 10 seconds for 48 hours is about 56 MB on its own, so per-workload history can't follow the same plan. The model needs each workload's 95th, 99th and 99.9th percentiles over up to 30 days.

## Decision

The store is SQLite through modernc.org/sqlite, which is pure Go. The server's series are kept as the blueprint says; a minute row keeps the largest values of its minute. Each workload is kept at 10 seconds for 6 hours and, for 30 days, as hourly DDSketch sketches with 1% relative accuracy, merged into the database every 10 minutes. Events are kept for 400 days. Above 56 MiB the oldest fine-grained rows go first, and 64 MiB is the cap.

## Options considered

- Workloads at 10 seconds for 48 hours, as for the server. Simple, but over the disk budget with 50 workloads.
- A time-series database such as Prometheus's TSDB. Built for this, but heavy for an agent with a 24 MiB memory budget.
- Plain files with our own format. Small, but crash safety, pruning and queries would all be ours to write and test.
- Doing nothing, keeping history in memory. Lost at every restart, so confidence could never pass "low".

## Consequences

Percentiles from sketches are upper bounds within 1%, and minute rows keep maxima, so the long-window need errs larger. The schema has a version and migrations; version 2 added each workload's hourly pressure sketch for the fix verifier.

## Reversal

Wrong if the 64 MiB cap or the memory budget is broken on busy servers, which the budget test in CI would show. A different store would sit behind the same `store` package interface.
