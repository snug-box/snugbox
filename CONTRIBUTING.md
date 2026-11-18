# Contributing to Snugbox

Thanks for helping. This page covers how to build and test the agent, what a pull request needs, and the extra rules for code that runs as root. Everyone taking part follows the [code of conduct](CODE_OF_CONDUCT.md).

For anything bigger than a small fix, open an issue first to agree the approach. A new fix in the catalogue especially needs that conversation: it has to say what it changes, how it is checked before it runs, and how `snugbox undo` puts it back.

## Building

You need Go 1.27; `go.mod` pins the exact toolchain and Go downloads it if yours is older. For the full set of checks you also need golangci-lint 2.15, buf 1.75 and shellcheck.

```sh
go build ./...
make check        # lint, unit tests with the race detector, protobuf and shell checks
make help         # every target
```

The agent runs on Linux. It also builds on Windows and macOS, and most of its tests pass on Windows, which is handy for development; the tests of real sockets, file safety and the helper's protocol run on Linux only.

## Tests

- `make test` runs the unit tests with the race detector. They use made-up servers built from files (`agent/internal/testbox`), so they need no root and touch nothing.
- `make fuzz` runs every fuzz target for 30 seconds. A failing input lands in the package's `testdata/fuzz` folder; commit it with the fix.
- `make release-check COSIGN=/path/to/cosign` builds the snapshot and runs `test/release/run.sh`: `test/release/verify.sh`, which checks a downloaded release, must pass a made-up release and the snapshot, both signed with a throwaway key, and fail on each fault a release can have. It needs cosign 3.1.4.
- `make live-tests` installs the package, starts the watcher, drives the real helper through its socket, has the watcher undo by itself a limit that hurts a service (`test/autoundo`), measures the memory and CPU budgets, and removes everything again. The budgets are measured on a month of history for 50 workloads, which the soak test writes first in about five minutes (`SNUGBOX_SOAK=1 SNUGBOX_SOAK_DB=dist/history.db go test -run TestThirtyDaysOfFiftyWorkloads ./agent/internal/store`); an empty store hides most of what the watcher costs. It also counts with nftables every packet the watcher and an audit send, which must go only to the cloud metadata service and, offline, nowhere (`test/net`), and plants made-up secrets in a service, a crontab and a container to check that nothing Snugbox writes or logs holds them (`test/secrets`; it runs gitleaks too when it is installed). It needs root and changes the machine, so run it only on a disposable virtual machine. Give that machine no swap in `/etc/fstab`: the swap fix leaves alone a box whose fstab lists swap, even swap that is off. The `release` job in `.github/workflows/ci.yml` shows how a runner is prepared.
- `make vm-tests` boots each distribution in blueprint section 9.7 as a throwaway KVM virtual machine and runs `test/vm/run.sh` in it: `snugbox doctor`, an audit, the package installed and removed the way `docs/install.md` says, every live test that fits the machine, and a reboot in the middle of a fix. It needs `/dev/kvm`, QEMU and `cloud-localds` from cloud-image-utils, and downloads each cloud image once, checked against the vendor's checksum file. `make vm-tests DISTROS="debian13 alma9"` runs only those; `test/vm/host.sh` lists the names. A full run takes about 7 minutes a distribution.

New behaviour needs a test that fails without the change. A bug fix needs a test that reproduces the bug.

## Writing code

- `golangci-lint` runs gofmt and goimports, with `snugbox.dev/snugbox` as the local import prefix.
- Comments say why, not what. A reader should learn from a comment something the code doesn't show.
- Errors carry context and are wrapped with `%w`; nothing panics on bad input.
- Messages to people are full sentences in plain words, and say what to do next when there is something to do.
- Code that reads a kernel file gets a fuzz target, because its input comes from the kernel and its output decides limits.

## Commits and pull requests

- Commit messages follow [Conventional Commits](https://www.conventionalcommits.org/en/v1.0.0/), such as `fix(helper): refuse a drop-in folder that is a link`. Release notes are written from them.
- Keep a pull request to one change. Fill in the template, including how you checked it.
- CI must pass: lint, tests, fuzzing, the protobuf checks, the vulnerability, licence and secret scans, and the snapshot release that is installed and tested on the runner.
- A code owner reviews every pull request.

By contributing, you agree that your contribution is licensed under the Apache License 2.0, as section 5 of the licence describes.

## Code that runs as root

The helper is the one part of Snugbox that changes a server, and it runs as root. Changes to `agent/internal/{fix,helper,helperapi,journal,safefs,units}`, `agent/cmd/snugbox-helper`, `proto` and `packaging` need two approving reviews, which the `two-reviews` check counts. These changes also need:

- every input from a caller checked against a bound before it is used;
- every change recorded in the journal, with a snapshot taken before it;
- an undo that is tested, including an undo after a crash in the middle of applying;
- a live test in `agent/internal/helper/live_integration_test.go` when the change touches systemd, the kernel or Docker;
- no new path in the helper unit's `ReadWritePaths` without a reason in the pull request. `TestUnitWritePaths` fails until the list matches the fix catalogue.

## Security problems

Report them privately, as [SECURITY.md](SECURITY.md) says, not in an issue or a pull request.
