# How to check memory usage on a Linux server

Use `free -h` to check available RAM, then look at memory pressure and the workloads running on the server. Take measurements during your busiest hours and scheduled jobs before choosing a smaller VPS.

## Check available memory and pressure

```sh
free -h
cat /proc/pressure/memory
```

In `free`, the **available** column estimates how much memory new programs can use without swapping. It includes memory the kernel expects to reclaim from caches. In the pressure file, `some` indicates periods in which at least one task stalled for memory. `full` indicates periods when all non-idle tasks stalled together. The averages cover 10, 60 and 300 seconds. Read the [Linux kernel's pressure documentation](https://docs.kernel.org/accounting/psi.html) for details.

## Find the processes and containers using RAM

```sh
ps -eo pid,comm,rss --sort=-rss | head -n 11
docker stats --no-stream
```

`ps` shows resident memory in KiB for each process. Several processes can share the same pages, so adding their RSS can overstate total use. Run `docker stats` on hosts with Docker to see running containers. Docker's Linux CLI subtracts some cache from its memory figure. See [Docker's stats reference](https://docs.docker.com/reference/cli/docker/container/stats/).

## Measure before resizing

If Snugbox is installed, start with an offline, read-only audit.

```sh
sudo snugbox audit --offline
```

A short audit shows which workloads are using memory now. Use [watch mode](watch.md) to see recurring peaks and [read the memory model](memory-model.md) before acting on a plan recommendation. Check disk usage and sustained CPU demand before switching plans.

[Install Snugbox](install.md) or [learn how to read an audit](audit.md).
