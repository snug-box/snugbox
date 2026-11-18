# Decision: snugbox self-update through The Update Framework, for script installs only

Status: proposed
Date: 2026-11-01
Deciders: the owner, on the build's proposal

## Context

Blueprint section 8.1 lists `snugbox self-update`, and section 6.3 says updates installed by the script use The Update Framework through go-tuf, with the root keys on the owner's two hardware keys, while packages from apt or dnf are signed with the repository key. Section 9.6 adds three channels, nightly, beta and stable, a rollout to 5%, 25% and then every box over 72 hours that stops on its own when updated agents crash or undo more, and that a bad release is fixed by a newer one, since TUF refuses an older one.

The script install puts the programs in `/usr/local/libexec/snugbox`; the packages put them in `/usr/libexec/snugbox`, where the package manager owns them.

## Decision

`snugbox self-update` updates a script install only, from a TUF repository whose first root is built into the release (`agent/internal/update/trust/root.json` and `repository.json`). The client is go-tuf 2.4.2's updater, with consistent snapshots and the metadata trusted so far kept in `/var/lib/snugbox-update`, a folder only root may write, so a mirror can't later serve older metadata or an older root.

Each channel is two plain files: `<channel>/version`, holding the newest version, and `<channel>/snugbox_linux_<arch>.tar.gz`, the release's tarball. The client reads the version file, installs nothing that isn't newer than itself, downloads the tarball and checks its length and hash against the signed metadata, writes the three programs beside the old ones, runs the new `snugbox version` and checks that it names the version it should, and only then renames each over the old one. A script install that runs the watcher gets `snugbox watch install` from the new version, which updates the units and restarts the watcher.

A build without a root in the trust folder says so and points at the install script. The trust folder stays empty until the owner has made the root on the two hardware keys.

## Options considered

- Custom metadata on the tarball's target for the version. go-tuf reads it, but tuf-on-ci, the TUF project's repository tool with hardware-key signing, treats custom metadata as a manual step, so two plain files keep the repository to any tool.
- One tarball name per release, such as `snugbox_0.2.0_linux_amd64.tar.gz`, with the client listing targets to find the newest. go-tuf's updater has no listing call for delegated targets, and the newest would have to be worked out from names.
- Checking the release's cosign signature, as the install script does, instead of TUF. Signatures alone don't stop a mirror from serving an old signed release or freezing a box on one; TUF's versions and expiry do.
- Updating package installs too. The package manager owns those files, and two updaters fighting over them would leave neither in charge.

## Consequences

The staged rollout of section 9.6 is not in this client. Its stop condition needs the crash and undo rates of updated agents, which only the hub has, so rollout comes with the hub; until then a person running `snugbox self-update` gets the newest release at once. A release built before the trust folder is filled can't update itself; its users run the install script once more. The update code adds go-tuf and the parts of sigstore it needs to the `snugbox` command only; the watcher and the helper don't link it.

## Reversal

Wrong if the repository has to live where hash-prefixed files can't be served; then `PrefixTargetsWithHash` off, with each channel's files at fixed names, is the next step, at the cost of a window in which a mirror serves a new tarball before its metadata.
