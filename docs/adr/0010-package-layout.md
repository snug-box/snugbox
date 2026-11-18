# Decision: install the programs in /usr/libexec/snugbox and ship rendered units

Status: proposed
Date: 2026-10-31
Deciders: the owner, on the build's proposal

## Context

Only `snugbox` is a command people type. `snugboxd` and `snugbox-helper` are started by systemd, and `snugbox watch install` must find them beside itself. The units are generated from templates in `agent/internal/units`, with the binary paths filled in, so the script install (into `/usr/local`) and the packages (into `/usr`) get the right paths. GoReleaser's open-source edition can't template files into packages.

## Decision

Packages put the three programs in `/usr/libexec/snugbox` and link `/usr/bin/snugbox` to the first; the install script does the same under `/usr/local`. `snugbox watch install` resolves its own path through the link to find the other two. The units rendered for `/usr/libexec/snugbox` are committed in `packaging/systemd`, and `TestPackagedUnitsAreCurrent` fails when they differ from the templates; `make units` rewrites them. Installing a package enables nothing: the watcher starts only after `sudo snugbox watch install`. An upgrade restarts what was running, removal stops and disables the units, and a purge deletes `/var/lib/snugbox` but never `/var/lib/snugbox-helper`.

## Options considered

- All three programs in `/usr/bin`. Simpler, but puts two programs on everyone's PATH that nobody should run by hand.
- Generating the units in a package script at install time. Avoids committed copies, but a package that runs our code at install is harder to audit than one that ships plain files.
- Enabling the watcher on install. One step fewer, but the blueprint's audit is read-only and starting a service on install would surprise people.

## Consequences

The committed unit copies are generated files that must be kept in step, which the test enforces. The package test in CI installs, upgrades, removes and purges the `.deb` and checks each result.

## Reversal

Cheap before the first release. After it, moving the programs needs the old paths kept as links for a release, because units written by `watch install` on script installs point at them.
