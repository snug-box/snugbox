# Maintaining Snugbox

This page is for maintainers: setting up the GitHub repository, cutting a release, and the few pins Renovate can't update. Contributors want [CONTRIBUTING.md](../CONTRIBUTING.md) instead.

## Setting up the repository

The workflows assume these settings. None of them can live in the repository, so they are listed here to be set once, by hand or with the `gh` commands below. Steps 2 to 5 need the repository to be public, or a paid plan; [while the repository is private](#while-the-repository-is-private) says what that changes.

1. The public repository is `snug-box/snugbox`. Keep the release workflow identity, installer and verification instructions in sync when this changes.
2. Turn on private vulnerability reporting, secret scanning with push protection, and Dependabot alerts (Settings, Code security). Leave CodeQL's default setup off: `.github/workflows/codeql.yml` is the advanced setup.
3. Install the Renovate GitHub app for this repository only. It reads `.github/renovate.json`.
4. Create the `release` environment (Settings, Environments) with a required reviewer and deployment limited to tags matching `v*`. The release workflow waits there for a person.
5. Protect `main` with a ruleset: pull requests only, one approving review from a code owner, stale approvals dismissed, the checks below required, no force pushes, no deletion, and signed commits.

The required checks are the job names of `ci.yml`, `codeql.yml` and `two-reviews.yml`:

| Check | Workflow |
| --- | --- |
| Lint | ci |
| Protobuf | ci |
| Test | ci |
| Fuzz | ci |
| Vulnerabilities, licences and secrets | ci |
| Dependency review | ci |
| Rebuilt elsewhere from a cold cache, the release is the same | ci |
| Snapshot release, installed and tested | ci |
| CodeQL (go) | codeql |
| CodeQL (actions) | codeql |
| Two reviews for code that runs as root | two-reviews |

Do not turn on the organisation policy that requires every action to be pinned to a commit. The release workflow calls `slsa-github-generator` by its version tag, which that project requires: its builder fails when called by commit. Every other action is pinned to a commit.

## While the repository is private

The repository was created private, on a personal account. There GitHub Free gives a private repository no rulesets or branch protection, no environments, no code scanning, dependency review or artifact attestations, and $12 of Actions time a month, about 2,000 minutes on the standard Linux runner, with 500 MB for artifacts. Sources, checked on 1 November 2026: [About rulesets](https://docs.github.com/en/repositories/configuring-branches-and-merges-in-your-repository/managing-rulesets/about-rulesets), [Managing environments for deployment](https://docs.github.com/en/actions/reference/environments) and [GitHub Actions billing](https://docs.github.com/en/billing/concepts/product-billing/github-actions).

The workflows run what they can:

- `codeql.yml` and `scorecard.yml` ask GitHub whether the repository is public, and skip their work when it isn't. `nightly.yml` does the same on its schedule, and still runs when started by hand.
- `ci.yml` skips dependency review and keeps the snapshot artifact for 1 day instead of 7.
- `release.yml` leaves out the artifact attestation and the SLSA provenance.
- `two-reviews.yml` asks for as many approvals as the `ROOT_REVIEWS` repository variable says, 2 when it isn't set. With one maintainer no one else can approve, so set it to 0 with `gh variable set ROOT_REVIEWS --body 0`, and back to 2 when a second maintainer joins.

Nothing stops a push to `main`, so changes go through pull requests by habit, merged with a merge commit once every check is green.

Don't tag a release while the repository is private. `install.sh` downloads from the release's public URLs, which a private repository answers only with a token, and cosign's keyless signing writes the repository's name and the release workflow's identity into Sigstore's public transparency log.

Before making it public, run `gitleaks git` over the whole history and read `git log` for anything that should stay private. Then change the visibility in Settings, General, and do steps 2 to 5 above.

## Cutting a release

1. Check that `main` is green and the changelog since the last tag reads right: `git log --oneline v0.1.0..main`.
2. Tag the release, signed, from an up-to-date `main`:

   ```sh
   git switch main && git pull --ff-only
   git tag -s v0.2.0 -m "Snugbox 0.2.0"
   git push origin v0.2.0
   ```

3. The release workflow checks that the tag is signed, annotated and on `main`, then waits for approval in the `release` environment.
4. After approval it builds, signs every file with cosign, attests the provenance, writes SBOMs, attaches SLSA provenance and leaves a draft release.
5. Read the draft. Edit the notes into something a person would want to read, check the files listed against the checksums, and publish.

A bad release is fixed by publishing a newer one. Don't delete a published release or move a tag.

## The update repository

`snugbox self-update` trusts one TUF repository, whose first root is built into each release from `agent/internal/update/trust/`. Until that folder holds `root.json` and `repository.json`, releases can't update themselves, so set the repository up before tagging the first release that should. [ADR 0016](adr/0016-self-update-through-tuf.md) has the design; the parts only the owner can do are these:

1. Make the repository with [tuf-on-ci](https://github.com/theupdateframework/tuf-on-ci), the TUF project's repository tool, in a repository of its own, such as `snug-box/snugbox-updates`, published with GitHub Pages or under `https://snugbox.dev/updates/`. tuf-on-ci signs with hardware keys and signs the short-lived snapshot and timestamp online from its workflow, so no signing key is stored anywhere.
2. Give the root and targets roles the two hardware keys of blueprint section 1, item 8, with a threshold of one, so either key can sign and the other is kept apart as the spare.
3. Copy the repository's `1.root.json` to `agent/internal/update/trust/root.json`, and write `agent/internal/update/trust/repository.json` with its two URLs, `metadata_url` for the metadata folder and `targets_url` for the targets folder, both https. Commit both in a pull request; every release after it embeds them.

To publish a release on a channel, after the GitHub release is published, add two files to the update repository's `targets/` folder in a tuf-on-ci signing event: `stable/version`, one line with the version, such as `0.2.0`, and `stable/snugbox_linux_amd64.tar.gz` and `stable/snugbox_linux_arm64.tar.gz`, copied from the release after checking them against its signed `checksums.txt`. Sign the event with a hardware key. Boxes see the release at their next `snugbox self-update`. A box never installs an older version, so a bad release is fixed by publishing a newer one there too.

The client and this layout are tested against a repository made by `go run ./agent/internal/update/repotest/tufrepo`, with test keys on disk; `test/update/run.sh` updates a script install on a test machine that way.

## Pins that are updated by hand

Renovate updates every action, Go module and tool version except these, because each sits beside a SHA-256 that has to be checked by a person:

| What | Where | How to update |
| --- | --- | --- |
| cosign for the install script | `COSIGN_VERSION` and the two `COSIGN_SHA256_*` values in `packaging/install.sh`; `COSIGN_SHA256` and the URL in the `release` job of `ci.yml` | Download `cosign_checksums.txt` and its bundle from the new cosign release, check the bundle with the cosign you have (identity `keyless@projectsigstore.iam.gserviceaccount.com`, issuer `https://accounts.google.com`), and copy the two linux lines |
| gitleaks | `GITLEAKS_VERSION` and `GITLEAKS_SHA256` in the `security` job of `ci.yml` | Copy the `linux_x64` line from the release's checksums file |
| slsa-verifier | `SLSA_VERIFIER_VERSION` and `SLSA_VERIFIER_SHA256` in the `verify` job of `release.yml` | Copy the `slsa-verifier-linux-amd64` line for the new version from `SHA256SUM.md` in the slsa-verifier repository, and check it against the downloaded file |

The cosign that the release workflow uses to sign, set by `cosign-release` in `release.yml`, is updated by Renovate; it doesn't have to match the one the install script uses to check.

## Commands for the ruleset

```sh
gh api -X PUT repos/snug-box/snugbox/private-vulnerability-reporting
gh api -X POST repos/snug-box/snugbox/rulesets --input - <<'EOF'
{
  "name": "main",
  "target": "branch",
  "enforcement": "active",
  "conditions": {"ref_name": {"include": ["~DEFAULT_BRANCH"], "exclude": []}},
  "rules": [
    {"type": "deletion"},
    {"type": "non_fast_forward"},
    {"type": "required_signatures"},
    {"type": "pull_request", "parameters": {
      "required_approving_review_count": 1,
      "require_code_owner_review": true,
      "dismiss_stale_reviews_on_push": true,
      "require_last_push_approval": true,
      "required_review_thread_resolution": true
    }},
    {"type": "required_status_checks", "parameters": {
      "strict_required_status_checks_policy": true,
      "required_status_checks": [
        {"context": "Lint"}, {"context": "Protobuf"}, {"context": "Test"}, {"context": "Fuzz"},
        {"context": "Vulnerabilities, licences and secrets"}, {"context": "Dependency review"},
        {"context": "Rebuilt elsewhere from a cold cache, the release is the same"},
        {"context": "Snapshot release, installed and tested"},
        {"context": "CodeQL (go)"}, {"context": "CodeQL (actions)"},
        {"context": "Two reviews for code that runs as root"}
      ]
    }}
  ]
}
EOF
```

These commands are written against GitHub's REST API documentation and have not been run yet: rulesets need the repository to be public, or a paid plan. Check the ruleset in Settings, Rules after creating it.

Signed commits and signed tags need a signing key that GitHub knows. An SSH key works: `git config gpg.format ssh`, `git config user.signingkey ~/.ssh/id_ed25519.pub`, `git config commit.gpgsign true`, and add the same key on GitHub as a signing key.
