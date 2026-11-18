# Decision records

Each record states one decision, the options it was chosen over, and what would make it wrong. Records are not edited after they are accepted; a changed decision gets a new record, and the old one is marked superseded with a link to it.

All of these were proposed while building the first release from the blueprint and are implemented in the code. They are marked proposed until the owner has read them.

| Record | Decision |
| --- | --- |
| [0001](0001-two-repositories-and-vanity-paths.md) | Two repositories, with Go module paths under snugbox.dev |
| [0002](0002-own-kernel-parsers-and-docker-client.md) | Read kernel files and the Docker API with our own small code |
| [0003](0003-privilege-split.md) | An unprivileged watcher and a root helper started by its socket |
| [0004](0004-helper-state-folder.md) | Keep the helper's journal, snapshots and swap file in /var/lib/snugbox-helper |
| [0005](0005-service-limits-by-drop-in.md) | Apply service limits with a drop-in and a daemon reload |
| [0006](0006-container-undo-and-compose-overrides.md) | Undo a container limit by raising it to the maximum, and write Compose overrides as the folder's owner |
| [0007](0007-automatic-undo-under-pressure.md) | Let an automatic undo of a limit run under pressure and soon after a kill |
| [0008](0008-local-api.md) | The watcher's local API is read-only HTTP and JSON on a Unix socket |
| [0009](0009-history-store.md) | Keep history in SQLite, with hourly sketches for each workload |
| [0010](0010-package-layout.md) | Install the programs in /usr/libexec/snugbox and ship rendered units |
| [0011](0011-release-signing.md) | Sign every release file without a stored key, and check it in the installer with a pinned cosign |
| [0012](0012-php-fpm-advice-only.md) | Work out PHP-FPM's pm.max_children and show it, never write it |
| [0013](0013-container-runtime-is-protected.md) | Never limit or restart the services of Docker and containerd |
| [0014](0014-schedule-moves-timers-earlier.md) | Spread colliding jobs by moving a timer earlier, never later |
| [0015](0015-cron-jobs-get-advice.md) | Suggest a later schedule for a cron job that collides, and never edit a crontab |
| [0016](0016-self-update-through-tuf.md) | Update a script install with snugbox self-update, verified through The Update Framework |
| [0017](0017-service-limits-reach-the-running-service.md) | Put service limits on the running service with set-property when the reload doesn't, and remove what it leaves |
| [0018](0018-summaries-within-the-budget-on-a-full-store.md) | Keep the watcher within its budget on a full store: a cache of whole days, reads SQLite needn't sort, a smaller page cache |
