# Swap file plus zswap

Risk: low. A box with no swap has nowhere to put memory that is allocated and never used again, so it keeps it in RAM and, under pressure, the kernel kills a process sooner. A swap file gives cold pages somewhere to go. zswap compresses pages before they reach the disk, so most swapping costs a little CPU instead of disk reads.

## What it changes

- Makes `/var/lib/snugbox-helper/swapfile`, owned by root with mode 0600, sized at the smaller of half of RAM and 4 GiB, in whole MiB and at least 128 MiB. On ext4 and XFS the file is allocated with no holes and formatted with `mkswap`; on Btrfs it is made by `btrfs filesystem mkswapfile`, which also turns copy-on-write and compression off for it.
- Writes the systemd unit `/etc/systemd/system/var-lib-snugbox\x2dhelper-swapfile.swap` and enables it, so the swap file is on at every boot. The boot loader and `/etc/fstab` are never touched.
- When the kernel has zswap: turns it on with zstd, or lz4 when the box's CPUs are more than 70% busy, and on Linux 6.8 or newer turns on the zswap shrinker, which moves cold compressed pages on to the swap file. `/etc/tmpfiles.d/snugbox-zswap.conf` sets the same values at boot, the compressor first. If zswap is already on, its settings are kept.

## When it is offered

- The box is a full virtual machine or bare metal, not an LXC, OpenVZ or WSL container, whose kernel belongs to the host.
- It has no swap, no zram swap, no swap area listed in `/etc/fstab` and no swap unit of its own. A swap setup someone chose is left alone.
- `/var/lib/snugbox-helper` is on ext4, XFS or Btrfs, and has free the swap size plus 10% plus 2 GiB. ZFS is never used, because a swap file on it can deadlock. Btrfs needs btrfs-progs 6.1 or newer.

## How it is undone

`sudo snugbox undo <transaction>` turns zswap off or back to its old values, stops and removes the swap unit, and removes the swap file. Turning swap off moves everything in it back into RAM, so undo does it only when memory pressure is low and the free memory covers what is in swap with 20% to spare; otherwise it refuses and says so, and you can try again later.
