# Early out-of-memory protection

Risk: medium, because it stops processes earlier than the kernel would. When memory runs out, the kernel's own out-of-memory killer acts late: by then the box may have spent minutes thrashing, unreachable over SSH. An early killer acts on pressure or low free memory, while you can still log in.

Snugbox doesn't install packages. It turns on systemd-oomd if it is installed, or earlyoom if systemd-oomd isn't.

## With systemd-oomd

- Writes `50-snugbox-oomd.conf` drop-ins that set `ManagedOOMMemoryPressure=kill` on `system.slice` and `user.slice` only, so systemd-oomd stops the workload under the most memory pressure there.
- Writes a drop-in with `ManagedOOMPreference=omit` for `ssh.service` or `sshd.service`, whichever exists, so it never stops the way in. Snugbox's own units carry the same setting.
- Enables and starts `systemd-oomd.service`. Swap-based killing, `ManagedOOMSwap=kill`, is never turned on.

Offered when the box uses cgroup v2, systemd-oomd is installed, no early out-of-memory killer runs already, and neither slice has a `ManagedOOMMemoryPressure` setting of its own; the helper refuses the fix when one does. Fedora's `systemd-oomd-defaults` package gives `system.slice` such a setting and runs systemd-oomd from the start, so on a Fedora box where someone stopped it the report names the file that sets the policy and says how to start systemd-oomd again, rather than offering a fix.

## With earlyoom

- Writes `/etc/systemd/system/earlyoom.service.d/50-snugbox-earlyoom.conf`, which replaces earlyoom's arguments: it acts when free memory and free swap are both under its default of 10%, logs a memory report hourly rather than every second, and never picks sshd, systemd, D-Bus or Snugbox. The distribution's own files for earlyoom stay as they are.
- Enables and starts `earlyoom.service`.

Offered when earlyoom is installed, systemd-oomd isn't, and no early out-of-memory killer runs already.

## How it is undone

`sudo snugbox undo <transaction>` removes the drop-ins, reloads systemd, and puts the daemon back as it was: stopped and disabled if it was before.
