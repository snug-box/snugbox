# Runtime heap caps

Risk: medium to high, because it restarts the service. Some runtimes size their heap from the box's RAM rather than from what the workload needs, or keep memory they no longer use. A cap tells the runtime its real budget, so it collects garbage before the limit instead of being killed at it.

## What it changes

A heap cap is a share of a memory limit, so it is offered only for a workload that has a limit, set already or proposed in the same `snugbox fit` list. Java and Node.js get 75% of the limit, in whole MiB; the rest is for the runtime's own memory, thread stacks and buffers. Go gets 90%, because `GOMEMLIMIT` already counts all the memory the Go runtime manages, and [the Go team's guide](https://go.dev/doc/gc-guide) suggests leaving 5 to 10% for the rest.

For a systemd service, a drop-in, `/etc/systemd/system/<unit>.d/50-snugbox-heap.conf`, sets one environment variable, and the service is restarted at once so it takes effect:

| Runtime | Setting for a service |
| --- | --- |
| Java | `JAVA_TOOL_OPTIONS=-Xmx<n>m`. Outside a container the JVM sizes its heap from the host's RAM, not from the service's `MemoryMax`, so the cap is a fixed size |
| Node.js | `NODE_OPTIONS=--max-old-space-size=<n>` |
| Go 1.19 or newer | `GOMEMLIMIT=<n>MiB`, a soft limit: the Go runtime collects garbage harder as it nears it, rather than stopping there |
| Python and Ruby with 4 threads or more | `MALLOC_ARENA_MAX=2`, since glibc keeps up to eight malloc arenas per CPU for threaded programs, each holding freed memory |

For a container, Snugbox shows the setting to put in its environment instead, since that means making the container again, which Snugbox leaves to you. Java in a container is told `-XX:MaxRAMPercentage=75`, because there the JVM reads the container's limit.

PHP-FPM gets advice only, for a service or a container: the `pm.max_children` line for its pool's file, such as `www.conf`. Snugbox counts the workers under PHP-FPM's master and measures each one's proportional set size, which splits the pages they share, OPcache's among them, so one worker's size is what one more worker costs. The line says how many workers of the largest size seen fit in 90% of the limit, leaving a tenth for the master and for a request that grows. Snugbox never edits PHP's own files; [decision record 0012](../adr/0012-php-fpm-advice-only.md) says why.

## When it is offered

- The workload's runtime is recognised from its program's name, or, for a Go program, from the build information Go writes into every executable. Snugbox reads that only for system services and containers. Java and Node.js programs started with heap flags of their own are left alone.
- The heap would be at least the smallest the helper accepts.
- The service isn't one a fix may never touch: sshd, D-Bus, systemd itself, Docker, containerd or Snugbox.
- Because it restarts the service, it runs only when you apply it, never on its own.

## How it is undone

`sudo snugbox undo <transaction>` removes the drop-in, reloads systemd and restarts the service, so it runs as it did before.
