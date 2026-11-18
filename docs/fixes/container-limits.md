# Container limits

Risk: medium. A container with no memory limit can take the whole box down with it. A limit keeps one container's growth from killing its neighbours, and Docker's soft limit tells the kernel which container to reclaim from first.

## What it changes

For each container it covers, live with `docker update`:

- the hard limit, `--memory`, at the larger of 1.3 times the 99.9th percentile of its working set and 1.2 times the highest memory it couldn't give back, rounded up to 64 MiB; containers with large or spiky network buffers get 1.5 times instead of 1.3;
- the soft limit, `--memory-reservation`, at the 95th percentile of its working set.

So the limits survive the container being made again, Snugbox also writes `compose.override.yaml` in the Compose project's folder, when the container belongs to a Compose project and the folder has no override yet. The file is written as the folder's owner, never as root, and never replaces a file. If the folder belongs to root, the project names its own list of Compose files, or an override exists already, the preview shows the lines to add yourself instead. `--no-persist` skips the override.

## When it is offered

- The container has run for 7 days, or you accept a limit worked out from less with `--accept-low-confidence`.
- It isn't a database. A hard limit on PostgreSQL, MySQL, Redis and the like risks a kill in the middle of a write, so databases get no automatic limit.
- A container whose shared memory or tmpfs is more than 10% of its working set gets a limit only on a box with swap, since that memory can't be reclaimed otherwise.
- The helper refuses a limit below 1.1 times what the container holds and can't give back right now.

## How it is undone

`docker update` can't take a limit away, only change it. A container that had no limit gets the largest value Docker accepts, which the kernel treats as no limit, and a soft limit of 6 MiB, the smallest Docker allows; the undo says so. A container that had limits gets its old values back. The override file is removed if it still holds what Snugbox wrote; if you edited it, it stays and the undo tells you.
