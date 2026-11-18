# Decision: work out PHP-FPM's pm.max_children and show it, never write it

Status: proposed
Date: 2026-10-31
Deciders: the owner, on the build's proposal

## Context

Blueprint section 3.7 lists PHP-FPM among the runtime heap caps: `pm.max_children` from the measured size of one child. Every other change the helper makes is a file of its own, such as a systemd drop-in, a unit, a tmpfiles line or a Compose override where none existed, and undo takes that file away. `pm.max_children` lives in a pool's section of PHP-FPM's own configuration, in a file that the distribution's package or the person owns: `/etc/php/8.3/fpm/pool.d/www.conf` on Debian and Ubuntu, `/etc/php-fpm.d/www.conf` on Fedora. PHP's manual documents the settings but not what happens when a second file defines the same pool again; people report that the later file wins, which is not something a root helper should rest on. Finding each pool's name and file would also mean reading PHP's configuration, which nothing else in Snugbox reads.

## Decision

Snugbox works the value out and shows it, and the helper never writes it. The collector counts PHP-FPM's workers, the php-fpm processes whose parent is the master, and reads each worker's proportional set size from `/proc/PID/smaps_rollup`, keeping the largest it has seen. Where the pool's service or container has a memory limit, set already or proposed, the advice is how many workers of that size fit in 90% of the limit, leaving a tenth for the master and for a request that grows, and how many run now. The heap caps proposal carries it as advice, as it carries a container's heap cap, so `snugbox fit` prints it and changes nothing for it.

## Options considered

- Edit the pool file in place, with a snapshot to restore. The only change that would rewrite a file someone else owns, and a PHP package upgrade could then ask about the edited file or replace it.
- Write a second file that defines the pool again with only `pm.max_children`. Rests on behaviour the manual doesn't document, and on knowing each pool's name and the include folder on every distribution.
- Leave PHP-FPM out, which drops what the blueprint asks for.

## Consequences

The person makes the change and reloads PHP-FPM. The worker size is the largest seen since the watcher started, or during a one-off audit, so a short audit at a quiet time can overstate how many workers fit; the report's confidence line says how long it watched.

## Reversal

Wrong if PHP documents how a later file overrides one setting of a pool, and that holds on the supported distributions. A drop-in in PHP-FPM's include folder could then become a fix with an undo.
