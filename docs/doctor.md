# What `snugbox doctor` reports

`sudo snugbox doctor` checks whether a box has what Snugbox reads, one line per check, marked ok, info, warn or fail. A fail means the audit can't run here, and the command exits with 1. Run it with sudo; without root, the checks of root-only files can't say anything.

| Line | What it means | What to do |
| --- | --- | --- |
| Couldn't read the kernel version | `/proc/sys/kernel/osrelease` is missing or unreadable | Check that `/proc` is mounted |
| Linux X is older than 5.4 | The kernel is older than the oldest Snugbox audits | Upgrade the kernel or the distribution |
| memory.peak and active probing need 5.19 or newer | Audits work, but the kernel keeps no per-workload peak, so a short audit knows less | Nothing is needed; a newer kernel helps |
| No cgroup v2 | The box uses the old cgroup hierarchy, which Snugbox doesn't read | Boot with `systemd.unified_cgroup_hierarchy=1`, or upgrade; current distributions use v2 |
| the memory controller is off | cgroup v2 is mounted without memory accounting | Turn the memory controller on for the root cgroup; it is on by default under systemd |
| Couldn't read /proc/pressure/memory | No pressure stall information, so audits miss pressure data and no fix can run, since fixes wait for a calm box. The kernel is older than 4.20, was booted with `psi=0`, or turns it off by default, as RHEL 9 and its rebuilds such as AlmaLinux and Rocky Linux 9 do | Boot with `psi=1`. On RHEL and its rebuilds: `sudo grubby --update-kernel=ALL --args=psi=1`, then reboot. Elsewhere, add `psi=1` to `GRUB_CMDLINE_LINUX` in `/etc/default/grub`, run `sudo update-grub` or `sudo grub2-mkconfig -o /boot/grub2/grub.cfg`, and reboot |
| Swap, and zswap lines | What swap and compression the box has; for information | See [the swap fix](fixes/swap.md) |
| This is WSL | Audits and service limits work; swap and zswap belong to Windows | Nothing |
| This box is a container | The kernel belongs to the host, so only container and service limits can be fixed | Nothing |
| systemd is not running | Services are audited as process groups, and service limits need systemd | Nothing, unless you want service limits |
| No early out-of-memory protection is running | Neither systemd-oomd, earlyoom nor nohang runs | See [early out-of-memory protection](fixes/oom-protection.md) |
| Not running as root | The root-only checks were skipped | Run it with sudo |
| Couldn't read /proc/1/smaps_rollup | Processes outside cgroups can't be measured | Run as root |
| Couldn't read /dev/kmsg | Kills are counted, but their times are unknown | Run as root |
| No Docker socket | No Docker here, or its socket is somewhere else | Nothing, if the box has no containers |
| Docker engine is older than 25.0 | Snugbox talks to Docker Engine 25 or newer only, so containers show their IDs and can't be given limits; the rest of the audit works | Upgrade Docker |
| Docker didn't answer | The socket is there but the engine didn't reply; containers will show their IDs instead of names | Check `systemctl status docker` |
