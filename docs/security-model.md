# Security model

Snugbox asks to run as root on servers, so this page says what each part may do, what stands between the parts, and what an attacker who took one part would get.

## The parts

| Part | Runs as | May do |
| --- | --- | --- |
| `snugbox`, the command | You, through sudo when it must | Read the box; ask the watcher and the helper |
| `snugboxd`, the watcher | The `snugbox` user, with `CAP_SYS_PTRACE` and `CAP_DAC_READ_SEARCH`, both only for reading, in a systemd sandbox | Read memory figures; keep history; ask the helper for container names, for the list of fixes, to move a fix on after watching it, and to undo a limit fix that is still being verified |
| `snugbox-helper` | Root, started by systemd when a request arrives, gone 60 seconds after the last; no network at all | Apply and undo fixes from the catalogue, write its journal, read Docker's container list and limits |

The watcher parses input from every process on the box all the time, so it holds no power to change anything. The helper changes the box, so it runs only when needed, takes typed requests only, and writes only where the fix catalogue needs: `/etc/systemd/system`, `/etc/tmpfiles.d`, zswap's parameters in `/sys`, `/run/systemd/system.control`, where it only removes the runtime settings its own `systemctl set-property` calls leave, and its own folder. A test fails if that list and the catalogue drift apart.

## Between the parts

- The helper's socket, `/run/snugbox/helper.sock`, can be opened by root and the `snugbox` group only.
- The helper checks the sender of every message with the kernel's credentials, not only who opened the connection, and refuses any message that carries file descriptors. Only root may apply a fix; the watcher's user gets a refusal.
- Requests are size-limited protobuf messages, and a message with fields the helper doesn't know is refused. Every number in a fix is checked against fixed bounds, such as a swap file of 128 MiB to half of RAM and 4 GiB, before anything happens.
- The command checks that the helper's socket is held by root before it sends anything.

## When the helper writes a file

It never follows a symbolic link: each folder is opened without following links, with `openat2` and `RESOLVE_NO_SYMLINKS` where the kernel has it; new files are made with `O_EXCL`; a replaced file is written to a temporary file in the same folder and renamed into place, after checking that the target is a regular file. A file in a folder that an ordinary user owns, such as a Compose project, is written as that user, through `systemd-run`, so a trick with links gains nothing. The helper's own folder, with the journal and the snapshots, is readable by root only.

## Records and brakes

- The journal chains every step with SHA-256, and the helper refuses to change anything if the chain is broken; see [the journal](journal.md).
- Every fix takes a snapshot first, and undo restores from it after checking its hash.
- `snugbox pause` stops every fix and every automatic undo, from a file only root can write.
- No fix runs under memory pressure or soon after an out-of-memory kill.

## If an attacker takes over a part

- The watcher: they can read memory figures, which the watcher's user could read anyway, ask the helper for container names, and undo or advance a fix being verified. They can't apply a fix, write a file of their choosing as root, or lift a pause.
- A user in the `snugbox` group: the same as the watcher.
- The helper: that is root, which is why the helper is kept small, about 3,500 lines of Go without blanks and comments, its protocol, journal and file writing included, and is the first thing the outside security review reads.

## Releases

Releases come only from a signed tag, through a workflow that a person approves. Every file is signed with cosign through the workflow's identity, with no stored key; GitHub records the build's provenance; builds are reproducible, which CI checks on every change; and the install script checks the signature before it trusts a checksum. See [SECURITY.md](../SECURITY.md) to check a release yourself, and to report a problem.

`snugbox self-update` trusts a root of trust built into the release, not the network: it installs a release only after the update repository's signed metadata checks out against that root and the release matches the metadata byte for byte, and it never installs an older version. The root's keys sit on two hardware keys kept apart. The metadata it has trusted stays in `/var/lib/snugbox-update`, which it refuses to use unless only root can write there, because a root planted there would be trusted. The watcher and the helper don't contain the update code.

## Not done yet

An outside security review of the agent and the helper is planned before Snugbox Cloud launches, and an outside review of the memory model before fixes are offered widely. Until then, the review is the code, the tests and this page.
