# Installing Snugbox

Snugbox runs on Linux 5.4 or newer with cgroup v2, on x86-64 and Arm64. `sudo snugbox doctor` says what a box has and lacks.

## From source

On Linux with Go 1.27.4 or later, build the command and run a read-only audit:

```sh
git clone https://github.com/snug-box/snugbox.git
cd snugbox
go build -trimpath -o snugbox ./agent/cmd/snugbox
sudo ./snugbox audit --offline
```

For the watcher, build the system packages with GoReleaser 2.19 and Syft installed:

```sh
make snapshot
sudo apt install ./dist/snugbox_*_amd64.deb
sudo snugbox watch install
```

The example uses Debian or Ubuntu on x86-64. For Arm64, use the `arm64.deb` package. On Fedora and other RPM distributions, install the matching `.rpm` with `sudo dnf install ./dist/<package>.rpm`.

## With the install script

Use this command once the first signed release is listed on [GitHub Releases](https://github.com/snug-box/snugbox/releases).

```sh
curl -fsSL https://github.com/snug-box/snugbox/releases/latest/download/install.sh | sh
```

The script prints what it will do before it does it. It downloads the release archive for your machine, its `checksums.txt` and the signature of that file, and cosign 3.1.4, whose own SHA-256 it pins. It checks that `checksums.txt` was signed by Snugbox's release workflow for that exact version, then checks the archive against it, and stops at the first mismatch. It downloads and checks as you, and uses sudo or doas only to copy the three programs into `/usr/local/libexec/snugbox` and link `/usr/local/bin/snugbox`. It starts nothing.

Options, after `sh -s --`:

| Option | Does |
| --- | --- |
| `--version 0.1.0` | Installs that release instead of the latest |
| `--prefix /opt/snugbox` | Installs under another folder |
| `--dry-run` | Prints the plan and changes nothing |

To read the script before running it, download it and check it against the signed checksums of a release: `install.sh` is attached to every release and listed in its `checksums.txt`. [SECURITY.md](../SECURITY.md) shows how to check the signature.

## With a package

Each release has `.deb` and `.rpm` packages for both architectures. Install the one for your machine:

```sh
sudo apt install ./snugbox_0.1.0_amd64.deb
sudo dnf install ./snugbox-0.1.0-1.x86_64.rpm
```

The package puts the programs in `/usr/libexec/snugbox`, links `/usr/bin/snugbox`, installs the systemd units and the watcher's settings file, `/etc/snugbox/snugboxd.env`, and creates the `snugbox` user. Installing enables nothing. An apt and dnf repository signed with Snugbox's key is planned and not ready yet.

## First steps

```sh
sudo snugbox doctor          # what this box has
sudo snugbox audit           # a read-only report from a minute's sample
sudo snugbox watch install   # keep history, and turn on fixes
```

## Upgrading

Install the newer package, or, for a script install, run `sudo snugbox self-update`. A package upgrade restarts the watcher if it was running.

`sudo snugbox self-update` checks the update repository's signed metadata against the root of trust built into the release, downloads the newest release on its channel, checks it byte for byte against that metadata, and installs it only if it is newer than the one running. It then runs `snugbox watch install` from the new version, which updates the watcher's units and restarts it, when the watcher is installed. `--check` says whether a newer release is out and changes nothing, and `--channel beta` or `--channel nightly` follows another channel. It keeps the metadata it has trusted in `/var/lib/snugbox-update`, so a mirror can't later hand it older metadata. A Snugbox from a package is updated by apt or dnf, and `self-update` says so.

A release made before the update repository was set up carries no root of trust, and its `self-update` says so; run the install script again to update it. Either way the watcher's history, its settings and the helper's journal carry over.

## Removing

First put back any fixes you don't want to keep: `sudo snugbox undo` lists them. Then:

- for a package, `sudo apt remove snugbox` or `sudo dnf remove snugbox`. `sudo apt purge snugbox` also deletes the watcher's history, the saved reports and the settings; after dnf, delete `/var/lib/snugbox` by hand if you want them gone;
- for a script install, `sudo snugbox watch uninstall --purge`, then `sudo rm -r /usr/local/libexec/snugbox /usr/local/bin/snugbox /etc/snugbox /etc/sysusers.d/snugbox.conf /var/lib/snugbox`.

Both leave the `snugbox` user; `sudo userdel snugbox` removes it. `/var/lib/snugbox-helper`, which holds the helper's journal and snapshots, is never deleted, because a fix still in place needs it to be undone. Remove it by hand when you are sure no Snugbox fix is left: `grep -rl "Made by Snugbox" /etc/systemd/system /etc/tmpfiles.d` finds the files fixes wrote there, and a container fix may also have written `compose.override.yaml` in a Compose project's folder, with the same first line.
