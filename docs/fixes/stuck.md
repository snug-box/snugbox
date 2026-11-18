# When the helper is stuck

The helper stops changing anything when it can't be sure of its own record. Each case below comes with the message you see, what it means, and what to do. Until it is resolved, no fix and no undo runs, automatic or not; the watcher keeps measuring.

## "the journal ... fails its check"

The journal at `/var/lib/snugbox-helper/journal` was edited, or the disk damaged it. The message names the first line that fails and why: the line was changed, a line before it is missing or moved, or the line is not a journal entry.

1. Copy the journal somewhere safe, as root: `cp -a /var/lib/snugbox-helper/journal /root/snugbox-journal-$(date +%Y%m%d)`.
2. Find out what changed it. A backup restore, a manual edit or a disk fault are the usual causes. The [journal page](../journal.md) shows how to check it yourself.
3. If a backup of the journal passes the check, put it back.
4. If not, note which fixes are in place: every file a fix wrote starts with `# Made by Snugbox for transaction`, so `grep -rl "Made by Snugbox" /etc/systemd/system /etc/tmpfiles.d` finds them, apart from any `compose.override.yaml` a container fix wrote in a Compose project's folder. Then move the journal aside: `mv /var/lib/snugbox-helper/journal /var/lib/snugbox-helper/journal.broken`. The helper starts a new journal, but it no longer knows those fixes, so `snugbox undo` can't take them out. Leave them in place, or take each out by hand: delete the file and run `sudo systemctl daemon-reload`, then check [the fix's page](README.md) for anything else it changed, such as the swap file.

## "can't write the journal, so nothing more is changed"

The helper couldn't append to its journal, usually because the disk is full or mounted read-only. Free space on the filesystem that holds `/var/lib/snugbox-helper`, then run the command again. A transaction that was under way is finished or put back the next time the helper starts.

## "the snapshot ... was changed after it was saved"

`/var/lib/snugbox-helper/undo/<transaction>/manifest.json` no longer matches the SHA-256 the journal recorded, so the helper won't restore from it. The journal's `snapshot` line for that transaction holds the list of changes with their old values: read it with `grep '"tx":"<transaction>"' /var/lib/snugbox-helper/journal`, put the old values back by hand, and remove the files the fix wrote.

## "the change failed ... and putting it back failed too"

A step of a fix failed, the helper tried to undo the steps that had run, and one of those failed as well. The message says which and names `/var/lib/snugbox-helper/undo/<transaction>`, which holds the old values. Put back what the message lists, then check with `sudo snugbox fit --status` that nothing else is in place. The transaction is recorded as failed, so no automatic undo will try it again.
