# Decision: sign every release file without a stored key, and check it in the installer with a pinned cosign

Status: proposed
Date: 2026-10-31
Deciders: the owner, on the build's proposal

## Context

People install Snugbox with `curl -fsSL https://snugbox.sh | sh` and run it as root, so a tampered release would reach root on every server that installs it. Blueprint section 6.3 asks for reproducible builds, cosign signatures through the workflow's identity with no long-lived keys, SBOMs, GitHub attestations, and slsa-github-generator provenance. Section 7.2 asks the install script to check the checksum and the signature against the release workflow's identity, not a key it carries.

## Decision

The release workflow runs only on a signed, annotated tag that is on `main`, and waits for a person in the `release` environment. GoReleaser builds with `-trimpath`, with the commit's own time for every file and archive member, and with one worker, because parallel builds add binaries to an archive in whatever order they finish; so a rebuild gives the same bytes, and CI checks that by building twice, in two folders, from a cold cache. cosign signs every release file through the workflow's OIDC identity, and the signatures go to Sigstore's public log. GitHub attestations cover every file in `checksums.txt`, and slsa-github-generator adds SLSA provenance. The install script downloads cosign v3.1.4, checks it against a SHA-256 pinned in the script, uses it to check that `checksums.txt` was signed by `.github/workflows/release.yml` at the tag being installed, and only then checks the archive. Downloads and checks run as the person, and only the copy runs as root.

## Options considered

- A signing key held by the owner. Works offline, but a key can leak, and rotating it breaks every old install script.
- Checksums only, over HTTPS. Protects against corruption, not against a changed release.
- Requiring cosign to be installed already. Cleaner, but most servers don't have it, so most people would skip the check.
- Doing nothing beyond GitHub's own storage. Trusts every account that can edit a release.

## Consequences

slsa-github-generator is the one workflow called by its version tag rather than a commit, because its builder refuses to run otherwise; the organisation-wide policy that requires commit pins must stay off. cosign's own version in the install script is updated by hand, with its checksum file's signature checked first, as docs/maintaining.md describes. Releases are drafts until a person publishes them.

## Reversal

Wrong if Sigstore's public service becomes unavailable for long; then a key held in hardware would replace keyless signing, and the install script would need a new release that carries it.
