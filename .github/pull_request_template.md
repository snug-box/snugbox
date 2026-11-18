## What changes and why

<!-- One or two sentences. Link the issue if there is one. -->

## How it was checked

<!-- The commands you ran and what they printed, or the test that covers it. -->

- [ ] `make check` passes: lint, tests with the race detector, the protobuf checks and the shell scripts
- [ ] New behaviour has a test that fails without the change
- [ ] The docs say what changed if a command, flag, file path, exit code or report line changed
- [ ] The commit messages follow Conventional Commits

## Code that runs as root

Changes under `agent/internal/{fix,helper,helperapi,journal,safefs,units}`, `agent/cmd/snugbox-helper`, `proto`
or `packaging` need two approving reviews. If this is one of them, say what the change can do to a server
that it could not do before, and how it is undone.
