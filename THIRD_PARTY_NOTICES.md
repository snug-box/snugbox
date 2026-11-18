# Third-party notices

The Snugbox agent is built from the Go modules below. Each licence was read from the module's own LICENSE file on 31 October 2026, and those of the modules added for self-update on 1 November 2026. Every one is permissive; CI blocks copyleft modules from the agent. Modules marked "tests only" are not compiled into any binary.

| Module | Version | Licence | Used for |
| --- | --- | --- | --- |
| github.com/spf13/cobra | v1.10.2 | Apache-2.0 | The command line |
| github.com/spf13/pflag | v1.0.9 | BSD-3-Clause | Command-line flags, through cobra |
| github.com/inconshreveable/mousetrap | v1.1.0 | Apache-2.0 | Through cobra, Windows builds only |
| charm.land/bubbletea/v2 | v2.1.1 | MIT | snugbox top |
| github.com/charmbracelet/colorprofile | v0.4.3 | MIT | Through Bubble Tea |
| github.com/charmbracelet/ultraviolet | v0.0.0-20261008173134-6b8d4baf91b4 | MIT | Through Bubble Tea |
| github.com/charmbracelet/x/ansi | v0.11.9 | MIT | Through Bubble Tea |
| github.com/charmbracelet/x/term | v0.2.2 | MIT | Through Bubble Tea |
| github.com/charmbracelet/x/termios | v0.1.1 | MIT | Through Bubble Tea |
| github.com/charmbracelet/x/windows | v0.2.2 | MIT | Through Bubble Tea, Windows builds only |
| github.com/clipperhouse/displaywidth | v0.11.0 | MIT | Through Bubble Tea |
| github.com/clipperhouse/uax29/v2 | v2.7.0 | MIT | Through Bubble Tea |
| github.com/lucasb-eyer/go-colorful | v1.4.1 | MIT | Through Bubble Tea |
| github.com/mattn/go-runewidth | v0.0.30 | MIT | Through Bubble Tea |
| github.com/muesli/cancelreader | v0.2.2 | MIT | Through Bubble Tea |
| github.com/rivo/uniseg | v0.4.7 | MIT | Through Bubble Tea |
| github.com/xo/terminfo | v1.0.0 | MIT | Through Bubble Tea |
| modernc.org/sqlite | v1.61.0 | BSD-3-Clause | The watcher's history database |
| modernc.org/libc | v1.79.0 | BSD-3-Clause | Through modernc.org/sqlite |
| modernc.org/mathutil | v1.7.1 | BSD-3-Clause | Through modernc.org/sqlite |
| modernc.org/memory | v1.12.1 | BSD-3-Clause | Through modernc.org/sqlite |
| github.com/dustin/go-humanize | v1.1.0 | MIT | Through modernc.org/sqlite |
| github.com/google/uuid | v1.6.0 | BSD-3-Clause | Through modernc.org/sqlite |
| github.com/mattn/go-isatty | v0.0.24 | MIT | Through modernc.org/sqlite |
| github.com/ncruces/go-strftime | v1.1.0 | MIT | Through modernc.org/sqlite |
| github.com/remyoudompheng/bigfft | v0.0.0-20230129092748-24d4a6f8daec | BSD-3-Clause | Through modernc.org/sqlite |
| github.com/DataDog/sketches-go | v1.4.8 | Apache-2.0 | Percentile sketches in the history |
| github.com/theupdateframework/go-tuf/v2 | v2.4.2 | Apache-2.0 | snugbox self-update: The Update Framework's client |
| github.com/sigstore/sigstore | v1.10.6 | Apache-2.0 | Signature checks, through go-tuf |
| github.com/sigstore/protobuf-specs | v0.5.0 | Apache-2.0 | Through sigstore |
| github.com/secure-systems-lab/go-securesystemslib | v0.11.0 | MIT | Canonical JSON for TUF metadata, through go-tuf |
| github.com/cenkalti/backoff/v5 | v5.0.3 | MIT | Download retries, through go-tuf |
| github.com/google/go-containerregistry | v0.20.7 | Apache-2.0 | Through sigstore, its pkg/name package only |
| github.com/opencontainers/go-digest | v1.0.0 | Apache-2.0 | Through go-containerregistry |
| google.golang.org/genproto/googleapis/api | v0.0.0-20250825161204-c5933d9347a5 | Apache-2.0 | Through sigstore/protobuf-specs |
| golang.org/x/crypto | v0.58.0 | BSD-3-Clause | Through sigstore |
| google.golang.org/protobuf | v1.36.12 | BSD-3-Clause | The helper protocol, and percentile sketches through sketches-go |
| golang.org/x/sync | v0.24.0 | BSD-3-Clause | Through Bubble Tea |
| golang.org/x/sys | v0.49.0 | BSD-3-Clause | Linux system calls |
| golang.org/x/term | v0.47.0 | BSD-3-Clause | Telling a terminal from a pipe |
| pgregory.net/rapid | v1.3.0 | MPL-2.0 | Property tests, tests only |

The full licence texts ship inside each module and in the release's software bill of materials.

## Notices of the modules

Two of the modules above ship a NOTICE file, which the Apache License asks to pass on.

github.com/DataDog/sketches-go:

```text
Datadog sketches-go
Copyright 2021 Datadog, Inc.

This product includes software developed at Datadog (https://www.datadoghq.com/).
```

github.com/theupdateframework/go-tuf/v2:

```text
Copyright 2024 The Update Framework Authors

Apache 2.0 License
Copyright 2024 The Apache Software Foundation

This product includes software developed at
The Apache Software Foundation (/).

SPDX-License-Identifier: Apache-2.0
```

## Documents

CODE_OF_CONDUCT.md is the Contributor Covenant, version 3.0, from https://www.contributor-covenant.org/version/3/0/, read on 31 October 2026 from the `release` branch of github.com/EthicalSource/contributor_covenant (commit d379fc9491eebf313942b979dfc4ab3b0d11df78). It is stewarded by the Organization for Ethical Source and licensed under CC BY-SA 4.0, which also covers the copy here. Snugbox filled in how to report a problem and removed the note asking adopters to edit the enforcement section; the rest is unchanged.

## Images

The logo in `docs/images/snugbox-lockup.svg` and `snugbox-lockup-inverse.svg` is original to Snugbox. Its letters were drawn from the outlines of the Recursive font (https://github.com/arrowtype/recursive, version 1.085, SIL Open Font License 1.1); the font itself is not shipped. The terminal pictures in `docs/images/audit.svg` and `plan.svg` are drawn from the report's own test output, `agent/internal/report/testdata`, for a made-up server.
