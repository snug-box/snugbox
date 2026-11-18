# Security policy

Snugbox runs as root on servers, so a security problem in it matters. Thank you for reporting one.

## How to report

Use GitHub's private vulnerability reporting: open [Report a vulnerability](https://github.com/snug-box/snugbox/security/advisories/new) on this repository. Only the maintainers see the report.

Please don't open a public issue, and test only on servers you own or may test.

A useful report says:

- what you found, and what someone could do with it;
- the version, from `snugbox version`;
- the steps to reproduce it, or a proof of concept;
- whether you have told anyone else.

## What happens next

- Within 3 working days we confirm we have the report.
- Within 10 working days we say whether we accept it and how severe we judge it, using CVSS 4.0.
- We aim to release a fix within 30 days for a critical or high severity problem, and within 90 days for the rest, and agree a publication date with you.
- The advisory credits you unless you would rather it didn't.

## Supported versions

Snugbox has no release yet. From 0.1.0, security fixes go into the latest minor release; earlier ones get none. A fix ships as a new version, never as a change to an old one.

## Checking a release

Every release's `checksums.txt` lists the SHA-256 of each of its files, `install.sh` included, and the release workflow signs it with cosign through its own identity, with no long-lived key. To check a download by hand with cosign 3.1.4 or newer, putting the release's tag in place of v0.1.0:

```sh
cosign verify-blob --bundle checksums.txt.sigstore.json \
  --certificate-identity https://github.com/snug-box/snugbox/.github/workflows/release.yml@refs/tags/v0.1.0 \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com checksums.txt
sha256sum --ignore-missing -c checksums.txt
```

To check every file of a release at once, download them all into one folder and run `sh test/release/verify.sh --provenance --attestations <folder> v0.1.0` from a checkout of the same tag. Besides the checksums, it checks each file's own signature, the SPDX and CycloneDX bills of materials of each archive, the SLSA provenance with slsa-verifier, and GitHub's artifact attestations with `gh attestation verify`. The release workflow runs the same script on every draft before anyone publishes it.

## Scope

In scope is everything in this repository: `snugbox`, `snugboxd`, `snugbox-helper`, the install script, the packages, and the build and release workflows. The parts we most want examined are the helper, which runs as root; the socket between the watcher and the helper; the journal and the snapshots that undo uses; and the files the helper writes.

Out of scope:

- something that needs root on the server already, such as replacing the helper's binary;
- reports from automated scanners with no shown impact;
- denial of service by a local user who can already exhaust the server's memory.

The hub and the web dashboard live in their own repository, which has its own policy.

## Checking a release

Every file of a release is signed with cosign by the release workflow, without a stored key, and the signatures are in Sigstore's public log. To check the checksums and then a file:

```sh
cosign verify-blob --bundle checksums.txt.sigstore.json \
  --certificate-identity https://github.com/snug-box/snugbox/.github/workflows/release.yml@refs/tags/v0.1.0 \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com checksums.txt
sha256sum --ignore-missing -c checksums.txt
```

`gh attestation verify <file> --repo snug-box/snugbox` checks the build provenance GitHub recorded, and `slsa-verifier verify-artifact` checks the SLSA provenance attached to the release. The install script runs the first two steps for you, with a cosign whose own SHA-256 it pins.
