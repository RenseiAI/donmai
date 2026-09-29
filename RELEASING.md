# Releasing donmai

This document describes the release path for the `donmai` binary, its GitHub
release artifacts, the `ghcr.io/renseiai/donmai-worker` image, the E2B worker
template, and the generated Homebrew cask.

## Release outputs

A `v*` tag push starts three independent workflows:

- `.github/workflows/release.yml` signs, notarizes, packages, and publishes the
  `donmai` binary with GoReleaser. Every tag gets immutable release assets; only
  an automatic stable tag push may advance GitHub Latest or update the rolling
  stable Homebrew cask.
- `.github/workflows/worker-image.yml` publishes the immutable versioned worker
  image to GHCR. Only an automatic stable tag push also advances `latest`.
- `.github/workflows/e2b-template.yml` publishes an E2B target named
  `donmai-worker:<version>` with the release version embedded in the binary.
  Only an automatic stable tag push also advances the rolling
  `donmai-worker:default` target to the same build.

A prerelease tag such as `v1.2.3-rc.1` therefore publishes version-addressable
GitHub assets, a `ghcr.io/renseiai/donmai-worker:v1.2.3-rc.1` image, and a
`donmai-worker:v1.2.3-rc.1` E2B target without changing GitHub Latest, GHCR
`latest`, E2B `default`, or the stable Homebrew cask.

GoReleaser publishes these archives plus `checksums.txt` and signature-provenance
sidecars:

| OS | Architecture | Archive |
|---|---|---|
| macOS | amd64 | `donmai_<version>_darwin_amd64.tar.gz` |
| macOS | arm64 | `donmai_<version>_darwin_arm64.tar.gz` |
| Linux | amd64 | `donmai_<version>_linux_amd64.tar.gz` |
| Linux | arm64 | `donmai_<version>_linux_arm64.tar.gz` |

The version is derived from the tag and injected through
`.goreleaser.yaml`'s `main.version` linker flag. There is no separate runtime
version file to bump.

## Named release notes

Keep publishing versioned releases as changes become ready. Every two weeks,
add one named, themed note to the **existing latest stable GitHub Release** to
make the intervening releases readable as a group. This editorial cadence does
not delay daily releases, create a new tag, rerun a publisher, or change signed
tags and release assets. Continue maintaining a per-version `CHANGELOG.md`
section for each ordinary release; the themed note is an additional summary.

Draft the note in `release-notes/vX.Y.Z-<theme>.md`, where `vX.Y.Z` is the
existing release object that will carry it. State the exact UTC date and tag
range. Open with a short account of what changed for users, then group a few
representative changes under the theme with links to their public pull requests.
Link the full GitHub compare range so the summary never masquerades as an
exhaustive list. Do not promote an unreleased change into the note.

Check the public compare history before naming contributors: review merged
pull-request authors, direct commit authors, and `Co-authored-by` trailers in
the range. Name **every human outside contributor** whose work appears there,
using a public name or handle supported by that history and a link to the
contribution. Do not guess an account from a name or count repository bots as
people. If there are none, say so plainly. Scan the exact draft with
`bash scripts/guard-b-lint.sh <draft-path>` and run `make guard` before
publication.

After the tagged release and its assets are verified below, confirm the
release object's `target_commitish` still names the intended immutable commit.
Then update **that release object's title and body** from the reviewed note,
without touching its tag or assets:

```bash
tag=vX.Y.Z
note=release-notes/vX.Y.Z-<theme>.md
gh release view "$tag" --repo RenseiAI/donmai \
  --json tagName,targetCommitish,publishedAt,url
gh release edit "$tag" --repo RenseiAI/donmai \
  --title "vX.Y.Z: Theme name" --notes-file "$note"
gh release view "$tag" --repo RenseiAI/donmai \
  --json name,body,targetCommitish,url
```

Read back the rendered GitHub Release note, then add a linked entry to the
curated `donmai.dev/changelog` ledger. Verify the deployed page shows the same
story and contributor credit. A source change or a successful site build alone
does not establish public acceptance.

## Prerequisites

- A clean, current `main` checkout.
- Go 1.26.6 or newer. `go.mod` is the canonical toolchain floor used by CI,
  security scanning, E2B builds, and release builds.
- GoReleaser v2.17.1 for local dry-runs. The release workflow pins this exact
  version because its verified phase order is part of the signing contract.
- GitHub CLI authenticated to `RenseiAI/donmai`.
- A tag-signing key configured for Git (`user.signingkey` and the matching
  `gpg.format`), with its public key registered as a GitHub signing key.
  Release tags must be signed, not merely annotated. Once the tagging-identity
  pins below are set, only the pinned identity's key can sign a release tag.
- Repository release secrets:
  - `HOMEBREW_TAP_GITHUB_TOKEN`, with write access to
    `RenseiAI/homebrew-tap`.
  - `APPLE_DEVELOPER_ID_CERT_BASE64` and
    `APPLE_DEVELOPER_ID_CERT_PASSWORD`.
  - `APPLE_DEVELOPER_ID`, `APPLE_PASSWORD`, and `APPLE_TEAM_ID` for
    `xcrun notarytool`.
  - `E2B_API_KEY` for the release-triggered E2B template build.

## Prepare the release

1. Confirm the latest tags and choose the smallest valid semantic-version
   increment:

   ```bash
   git fetch origin --tags
   git tag --sort=-v:refname | head -3
   ```

2. Update `CHANGELOG.md`. Move all notable changes since the previous release
   into `## vX.Y.Z — YYYY-MM-DD`, grouped under `Features`, `Fixes`, and
   `Chores` as appropriate.

3. Run the repository gates from the release commit:

   ```bash
   make test
   make lint
   make guard
   make build
   make verify-generated
   make vuln
   make release-dry-run
   ```

   `make release-dry-run` runs GoReleaser in snapshot mode and explicitly skips
   the production signing pipes. It does not publish, tag, or modify the
   Homebrew tap. `bash scripts/test-release-workflows.sh` separately exercises
   the signing script's fail-closed behavior with local test doubles. Before a
   signing-pipeline change is merged, run `bash
   scripts/test-sign-and-notarize.sh --goreleaser` to build a locally mocked
   signed snapshot and prove the registered artifacts and final-input hashes.

4. Commit the release preparation and merge it to `main`. Do not release from
   an unmerged branch or a dirty checkout.

## Create the release tag

Tags are immutable release inputs. Publisher tags must be strict semantic
versions: `vMAJOR.MINOR.PATCH`, optionally followed by well-formed SemVer
prerelease identifiers such as `-rc.1`. Numeric identifiers cannot contain
leading zeroes. Build metadata, arbitrary suffixes, trailing dots, mutable
labels such as `latest`, and branch names are rejected.

### Tag authority and immutability

Every publisher performs a read-only GitHub preflight before any build. Its
least-privilege `github.token` requires exactly two independent active
repository rulesets for `refs/tags/v*`, with exact include/exclude scopes and
rule types:

- a creation-only ruleset whose single `always` bypass actor is the pinned
  tag creator (`OrganizationAdmin` until a dedicated tagging identity is
  pinned; see below); and
- a no-bypass ruleset containing only deletion, update, and non-fast-forward
  protection.

The split is load-bearing. GitHub can omit bypass actors from a
`contents:read` workflow response, so the publisher enforces structural policy
without treating an omitted actor field as an empty actor list. If GitHub does
return actor fields, the publisher also rejects any visible actor broader than
the exact policy and requires its workflow identity to have no bypass. An
administrator-visible audit separately proves that the creation bypass
identifies who may create a release tag and that nobody can bypass the second
ruleset to delete or retarget it. Do not add a privileged Actions secret to
bridge those evidence tiers.

An authorized repository administrator can apply the reviewed payloads once:

```bash
gh api --method POST repos/RenseiAI/donmai/rulesets \
  --header 'Accept: application/vnd.github+json' \
  --header 'X-GitHub-Api-Version: 2022-11-28' \
  --input - <<'JSON'
{
  "name": "Protect release tag immutability (v*)",
  "target": "tag",
  "enforcement": "active",
  "bypass_actors": [],
  "conditions": {
    "ref_name": {"include": ["refs/tags/v*"], "exclude": []}
  },
  "rules": [
    {"type": "deletion"},
    {"type": "update"},
    {"type": "non_fast_forward"}
  ]
}
JSON

gh api --method POST repos/RenseiAI/donmai/rulesets \
  --header 'Accept: application/vnd.github+json' \
  --header 'X-GitHub-Api-Version: 2022-11-28' \
  --input - <<'JSON'
{
  "name": "Authorize release tag creation (v*)",
  "target": "tag",
  "enforcement": "active",
  "bypass_actors": [
    {
      "actor_id": null,
      "actor_type": "OrganizationAdmin",
      "bypass_mode": "always"
    }
  ],
  "conditions": {
    "ref_name": {"include": ["refs/tags/v*"], "exclude": []}
  },
  "rules": [{"type": "creation"}]
}
JSON
```

Read back the full server-normalized rules, including bypass actors, without
changing them:

```bash
gh api --paginate 'repos/RenseiAI/donmai/rulesets?per_page=100' \
  --jq '.[] | select(.target == "tag") | .id' |
while read -r ruleset_id; do
  gh api "repos/RenseiAI/donmai/rulesets/${ruleset_id}" \
    --jq '{id,name,source_type,source,target,enforcement,conditions,bypass_actors,rules}'
done

GH_TOKEN="$(gh auth token)" \
  ./scripts/verify-release-authority.sh --audit-policy RenseiAI/donmai
```

The audit must report exact no-bypass immutability and a sole creation actor
equal to the pinned creator (`OrganizationAdmin(always)` by default). With the
default pin, the transient local `GH_TOKEN` above must read
`current_user_can_bypass=always` for creation; with a dedicated creator team,
the administrator is normally outside the team and reads `never`, and the exact
visible actor is the proof. Immutability must always read `never`. Pass the
same identity pins to the audit that the workflows use. Never copy that administrator token into an Actions secret. CI
also performs a read-only same-token visibility control: structural fields must
be visible, while actor field omission is recorded as the reason the
administrator audit remains required.

### Tagging identity pins

Release tags can be pinned to one dedicated tagging identity: a machine account
whose SSH signing key is held only by a central tagging workflow, so no
operator host keeps release-signing material. `scripts/verify-release-authority.sh`
reads three pins, which the release, worker-image, and E2B workflows pass from
Actions variables of the same names:

| Variable | Meaning | Unset |
|---|---|---|
| `RELEASE_TAG_CREATOR` | The creation ruleset's sole bypass actor: `OrganizationAdmin` or `Team:<numeric team id>` | `OrganizationAdmin` |
| `RELEASE_TAGGER_EMAIL` | The tagger email every release tag must carry | no email pin |
| `RELEASE_TAG_SIGNERS` | OpenSSH allowed-signers lines (public keys only) for the keys that may sign release tags | no signer pin |

With the signer pins set, every publisher requires, in addition to GitHub's own
verification of the tag signature:

- the tag object's tagger email to equal `RELEASE_TAGGER_EMAIL`;
- the signed payload GitHub returns to name this tag, its commit, and that
  tagger, so a signature lifted from another tag cannot pass; and
- `ssh-keygen -Y verify` of that payload in the `git` namespace against
  `RELEASE_TAG_SIGNERS`, honoring each line's `valid-after`/`valid-before`.

A tag signed by any other key, including a previously accepted operator host
key, is refused before any build. When GitHub shows the creation ruleset's
bypass actors to the workflow token, the actor must equal `RELEASE_TAG_CREATOR`;
otherwise that check stays with the administrator audit above.

With no pins set, the transitional policy is today's: an `OrganizationAdmin`
creator and any signature GitHub verifies, so administrator-signed tags keep
releasing. The pins fail closed when half-configured: `RELEASE_TAGGER_EMAIL`
and `RELEASE_TAG_SIGNERS` must be set together, and a `Team:` creator requires
both. A malformed creator value is refused. Switch over in one sitting: set the
three variables and move the creation ruleset's bypass to the tagging team, then
run `--audit-policy` with the same pins.

When rotating the signing key, keep the previous key's allowed-signers line for
one rotation so a manual retry of a tag signed with it still verifies, then
drop it.

Always create the tag at an explicit commit SHA, never from an implicit branch
ref. Before creating it, run the read-only signing-key preflight. It resolves
the active `github.com` account, derives the public key from Git's exact
`user.signingkey` setting (a public/private key path or a `key::` literal), and
compares its SHA-256 fingerprint with that account's public SSH signing keys.
It prints only the account login and fingerprint, never private-key bytes. A
GitHub authentication/readback error, a non-SSH Git format, an unreadable key,
or an unregistered fingerprint is a hard stop before a tag exists remotely.

After creating the local tag, run the same helper in verification mode. It
builds a one-key temporary `gpg.ssh.allowedSignersFile` and verifies the tag
through Git, so local SSH verification is deterministic even when the operator
has no global allowed-signers file (or has an unrelated one):

```bash
tag=vX.Y.Z
release_sha="$(git rev-parse HEAD)"
./scripts/verify-release-signing-key.sh
git tag -s "$tag" "$release_sha" -m "$tag"
./scripts/verify-release-signing-key.sh --verify-tag "$tag"
git show --no-patch --decorate "$tag^{commit}"
git push origin "refs/tags/$tag"
```

The tag push starts the release, worker-image, and E2B workflows. Before any
build, each workflow verifies both rulesets' exact structural shape and
GitHub's cryptographic verification of the annotated tag object. The prior
administrator audit is the authority proof for actor policy. The release
workflow then verifies that its checkout is the commit referenced by the
release tag. GoReleaser also sends that exact commit through
`release.target_commitish`; it never asks GitHub to target the moving default
branch. The shared verifier exposes both prerelease status and the rolling-alias
policy to every publisher. Automatic stable tags advance rolling aliases;
prerelease tags publish their immutable version targets only.

Do not move or reuse a published tag. If a release is bad, fix it and publish a
new patch version.

## Fast lane

A switchable fast lane lets a commit that already passed the repository gates
locally skip the release workflow's remote `harness-smoke` re-run. It needs two
keys, and both must hold:

1. The `FAST_LANE` Actions variable is exactly `on`.
2. The tag's exact commit carries a `local-verify` commit status whose latest
   state is `success`, recorded when that commit was verified locally before it
   landed on `main`.

The release workflow's `fast-lane` job reads both after `release-authority`
passes and writes its decision and the attestation (description, poster, time)
to the run summary. Only then is `harness-smoke` skipped. Everything else runs
in every mode: the signed and immutable tag checks, the tagging-identity pins,
Apple signing and notarization, Sigstore signatures, build provenance, the
Homebrew cask policy, and the worker-image and E2B workflows. CI still runs on
the push to `main`, and the `donmai-smokes` nightly run exercises the same smoke
suite against `main`; both are the fix-forward signal.

With the switch off, a missing or non-success attestation, or any readback
error, `harness-smoke` runs and gates the publish exactly as before. A commit
that landed through a reviewed pull request carries no attestation, so it keeps
the full release gate even while the switch is on. The attestation is a record,
not a proof: anyone with write access can post a commit status, so the trust
anchor remains the credential that landed the commit. Turn the lane off by
setting `FAST_LANE` to anything other than `on` or deleting it. `make ship` and
`make release` read the switch more strictly, as described below.

### Ship and release: two commands

While the lane is on, `make ship` and `make release` replace the
release-preparation pull request and the manual tag steps above. They split
landing from publishing on purpose: a change can land on `main` many times a
day, but a public release, with its Homebrew cask update, happens at most once
a day, so people installing `donmai` see one version per day rather than a
stream of them.

- **`make ship`** lands one change on `main`. It never tags or publishes.
- **`make release`** is the daily release train. It publishes whatever has
  landed since the last release.

Both commands are for an operator with admin rights on this repository. Both
refuse (exit 3) unless the lane is on. For these commands, on means both of
the following:

- The organization Actions variable `FAST_LANE` is exactly `on`.
- No repository variable of that name exists.

A repository variable, even one set to `on`, keeps both commands off. That
way, one repository cannot hold the lane open against the organization's
decision. Both commands read the switch again just before `main` moves, and
`make release` reads it once more just before it creates the tag.

### Fast-lane ship (`make ship`)

Run it from a linked worktree whose branch holds the change:

```bash
make ship DRY_RUN=1   # every read-only check and the guard; changes nothing
make ship             # land the branch on main
make ship FULL=1      # also make vuln and make release-dry-run
```

`scripts/fast-lane.sh ship` then:

1. **Refuses** before anything changes (exit 3) unless:
   - the lane is on;
   - the `gh` login is a repository admin;
   - it runs in a linked worktree on a branch, never the primary checkout or
     `main`, and the branch name passes guard-b;
   - the tree is clean, HEAD contains fresh `origin/main`, and the remote
     branch holds nothing HEAD lacks.

   A branch with nothing beyond `origin/main` is "nothing to ship" (exit 0).
2. **Composes** the squashed commit message: the open pull request's title
   and body, the single commit's message, or `TITLE=`. A guard-b violation in
   the message is a refusal.
3. **Runs the gates**, stopping at the first red:
   - `guard`:
     - the guard self-tests;
     - the vendored-guard drift check, so a branch cannot weaken its own
       guard;
     - `guard-b-lint.sh` over every file that differs from `origin/main`
       (`--staged` would see only the index);
     - the attach-path listener check.

     Each step's failure fails the gate.
   - `release-contracts`: `scripts/test-release-workflows.sh`, whenever the
     change touches `scripts/`, `.github/workflows/` or `.goreleaser.yaml`.
     The release workflows run those files, so their contract tests gate
     them.
   - `make lint` (gofumpt included) and `make test-tagged`.
   - `go test -race ./...` in a Linux podman container
     (`scripts/podman-go-test.sh`, also `make test-podman`), so the daemon
     install tests never touch the host's service.
   - `make build`.

   A red gate lands nothing.
4. **Lands** exactly the tested tree as one commit on `origin/main`:
   - The commit carries `Local-Verify-Platform` (an OS and architecture such
     as `darwin/arm64`, never the host name), `Local-Verify-Gates`,
     `Local-Verify-Duration` and `Fast-Lane: on` trailers, and passes
     guard-b.
   - It is pushed to the branch. The `local-verify` status is posted on that
     SHA and read back, and the gate lines go in a comment on the open pull
     request.
   - The switch is read again. Then `main` is fast-forwarded
     (`git push origin <sha>:refs/heads/main`, never forced). If `main` moved
     during the gates, the command stops without landing; run it again.

Add user-facing notes under `## [Unreleased]` in `CHANGELOG.md` as part of the
change. The release train moves them into the version section.

### Daily release train (`make release`)

The policy is one release a day, at midnight `America/New_York`, and only when
there is unreleased work: `main` has commits since the latest `v*` tag. Run
the train from a scheduler (a launchd job or cron) on the operator's host.
Update the checkout first: the train refuses to run with a copy of its scripts,
or a `fast-lane.sh`, that differs from `origin/main`. Fetch and fast-forward,
which also works on a worktree branch with no upstream:

```bash
git -C <checkout> fetch --quiet origin main &&
  git -C <checkout> merge --ff-only --quiet origin/main &&
  make -C <checkout> release
```

For a manual run:

```bash
make release DRY_RUN=1        # every read-only check, the composed CHANGELOG, the guard
make release                  # next patch version
make release VERSION=v0.73.0  # a minor release
make release NO_WATCH=1       # stop after the tag
```

Only values given on the `make` command line count; a `VERSION` or `TAGGER`
left in the environment is ignored. The command is non-interactive. It exits 0
when it released, or when there was nothing to release and the latest release
is complete. It exits non-zero with a `FAILED:` or `REFUSED:` line on anything
else. `scripts/fast-lane.sh release` then:

1. **Preflight**, read-only (exit 3 on a refusal):
   - the lane is on;
   - the `gh` login is an admin;
   - this checkout's `scripts/`, and the running `fast-lane.sh`, equal
     `origin/main`'s.

   There is nothing new to release when either of these holds:
   - `main` has no commits since the latest `origin` tag;
   - the only new commits are `docs`, `test` or `ci` commits, and
     `CHANGELOG.md` has no Unreleased entries.

   Then the train finishes the latest release instead: it runs the same watch
   as step 6 for the latest tag. A run that already finished answers at once.
   It prints "nothing to release" and exits 0 only when all of these are
   complete:
   - the tag's `release.yml`, `worker-image.yml` and `e2b-template.yml` runs
     succeeded;
   - the GitHub release is published;
   - the cask is at that version.

   Otherwise it exits 1 with a `FAILED:` line naming what is unfinished. A
   release that failed after its tag therefore fails every run until it is
   fixed. It is never reported as "nothing to release".

   Then the version and the tag signer:
   - The version is the next patch from the latest `origin` tag, or
     `VERSION=` as the smallest minor or major increment. When `main` already
     holds a `chore(release): prepare vX.Y.Z` commit since that tag, left by
     a run that failed after landing it, the default is that version. It must
     have no tag and no GitHub release yet.
   - With the local signer:
     - the signing-key preflight passes;
     - `verify-release-authority.sh --audit-policy` proves that this login
       may create `v*` tags under the exact rulesets
       (`current_user_can_bypass=always`).
   - With the central signer, the tagging workflow must exist.
2. **Composes** the release preparation in a scratch worktree at
   `origin/main`. The caller's checkout and branch are never released. The
   `## vX.Y.Z — YYYY-MM-DD` section comes from, in order:
   - an existing section;
   - the `## [Unreleased]` entries, leaving the `No unreleased changes.`
     placeholder behind;
   - one composed from the subjects landed since the last tag.

   guard-b runs over the section, the commit message and those subjects.
   GoReleaser turns the subjects into the release notes. A violation is a
   refusal.
3. **Runs the same gates** in that worktree. `release-contracts` runs when
   anything under `scripts/`, `.github/workflows/` or `.goreleaser.yaml`
   changed since the last tag.
4. **Lands** `chore(release): prepare vX.Y.Z` through the ship's attested
   path:
   - It pushes the commit to `release-train/vX.Y.Z` and posts and reads back
     `local-verify`.
   - It reads the switch again and checks that the version is still free.
   - It fast-forwards `main`, then deletes the `release-train/vX.Y.Z` branch.
5. **Tags** `vX.Y.Z` at that SHA, after reading the switch and checking the
   version once more:
   - Until the tagging-identity pins are set, the operator's registered key
     signs the tag, as the transitional signer.
   - Once the pins are set, an operator-signed tag would be refused. The
     train then uses the central tagging workflow (`TAGGER=central`): it
     dispatches that workflow with the repository, SHA and version, and waits
     for the tag. A tag that appears on another SHA fails at once. The
     workflow's repository is named only by the `RELEASE_TAGGER_REPOSITORY`
     Actions variable, and its file by `RELEASE_TAGGER_WORKFLOW` (default
     `tag-release.yml`). The central path is refused while
     `RELEASE_TAGGER_REPOSITORY` is unset.
   - Either way, GitHub must report the tag signature verified.
6. **Watches** each publisher to success:
   - the `release.yml`, `worker-image.yml` and `e2b-template.yml` runs for the
     tag;
   - the published GitHub release;
   - `Casks/donmai.rb` moving to the new version.

   The release run's fast-lane job skips `harness-smoke`, because the SHA is
   attested.

Re-running after a failure is safe:
- Nothing lands unless every earlier step passed.
- If `main` already carries the version's section, from a run that failed
  after landing, the train tags `main` as it is, under that version.
- A failed tag push removes its local tag. The train records each local tag it
  makes until the tag is pushed, so it replaces its own tag that a killed run
  left behind. It refuses any other local-only tag of that version, which may
  be another operator's.
- A failure after the tag, such as a publisher run, the release or the cask,
  is picked up by the next run, as described in step 1.

A published tag is never moved. If a publisher run fails after the tag exists,
re-run its failed jobs with `gh run rerun <run-id> --failed`. That keeps the
tag-push policy that advances Latest and the cask; a manual retry through
`workflow_dispatch` (below) skips the cask, and the train does not count it.
Alternatively, release the next patch. If the signature check fails, the fix
is the next patch.

## Retry a release workflow

Each publisher supports a manual retry only for an existing explicit release
tag:

```bash
gh workflow run release.yml --ref main -f tag=vX.Y.Z
gh workflow run worker-image.yml --ref main -f tag=vX.Y.Z
gh workflow run e2b-template.yml --ref main -f tag=vX.Y.Z
```

The `--ref` value selects the workflow definition. Each workflow's required
`tag` input selects a namespace-qualified, detached checkout of
`refs/tags/vX.Y.Z`, so a same-name branch cannot win ref resolution. The current
workflow's verifier, signing script, E2B wrapper, and GoReleaser policy are
staged outside the workspace before that tag checkout, allowing retries of
signed tags that carry older release automation. Before signing, building, or
publishing, the authority preflight re-reads both rulesets and the signed tag
from GitHub. The shared checkout verifier then enforces the release-tag grammar,
proves detached `HEAD`, proves that the tag object contains a signature, and
proves that `HEAD` equals the tag's peeled commit. The verified tag is exported
as `GORELEASER_CURRENT_TAG` and is also the only source for published image,
template, and embedded binary versions.

A manual binary retry sets GoReleaser's supported `release.make_latest` policy
to false, so replaying an older release cannot replace the repository's current
GitHub Latest release. Prereleases use the same no-latest policy. Only automatic
stable tag pushes retain the normal make-latest behavior. A manual retry or
prerelease worker-image publication writes only the versioned image and leaves
`latest` unchanged. The corresponding E2B path creates or updates only
`donmai-worker:vX.Y.Z[-prerelease]` and leaves `donmai-worker:default` unchanged;
automatic stable tag pushes assign both the version tag and `default` to the new
E2B build. Neither E2B path writes back to the repository.

The staged GoReleaser configuration also consumes the verifier's explicit
Homebrew policy through `homebrew_casks[].skip_upload`. Only an automatic stable
tag push sets that policy to publish. Every manual retry, including a retry of
the current highest stable tag, republishes immutable GitHub assets while
skipping the Homebrew publisher; older-tag retries therefore cannot roll
`Casks/donmai.rb` backward. Prereleases also skip the cask so they cannot replace
the stable cask. If only the cask publication failed for the current stable
release, use the focused tap-repair path below instead of replaying GoReleaser.

Do not pass a branch name, `latest`, or any other mutable label as the `tag`
input. Do not use a branch-derived `GITHUB_REF_NAME` as a release target.

## Verify the GitHub release

```bash
tag=vX.Y.Z
gh release view "$tag" --repo RenseiAI/donmai
gh api "repos/RenseiAI/donmai/releases/tags/$tag" \
  --jq '{tag_name, target_commitish, draft, prerelease}'
gh api "repos/RenseiAI/donmai/releases/latest" --jq '.tag_name'
```

Confirm:

- `target_commitish` is the immutable release commit SHA, not `main`.
- The release is neither a draft nor a prerelease unless intentionally planned.
- After a prerelease publication or manual retry of an older tag,
  `/releases/latest` still returns the previously current stable release rather
  than the prerelease or retried tag.
- All four archives and `checksums.txt` are attached, each with a Sigstore
  `.sig` + `.pem` pair, plus a `.codesign.txt` notarization record for the
  darwin archives.

  The `.pem` files must appear as GitHub assets. `cosign
  --output-certificate` creating a local file is insufficient on its own;
  `.goreleaser.yaml` must declare `certificate:` so GoReleaser registers and
  uploads each certificate.

  Note the rename: `.sig` used to be a five-line text file — for linux it
  literally read `Signature: none` — while being described as "provenance".
  `.sig` is now a real detached Sigstore signature, and the human-readable
  notarization record moved to `.codesign.txt`. Do not treat the two as
  interchangeable.
- Archive names follow the `donmai_<version>_<os>_<arch>.tar.gz` template.
- Release notes cover the final `CHANGELOG.md` entry.

Download the assets and verify their checksums:

```bash
gh release download "$tag" --repo RenseiAI/donmai --dir "dist/$tag"
cd "dist/$tag"
shasum -a 256 -c checksums.txt
```

### Verify the Sigstore signatures

Every archive and `checksums.txt` is signed keyless: there is no private key to
store or rotate. The signer identity is the release workflow itself, recorded in
the public Rekor transparency log. Verify one with:

```bash
archive="donmai_${tag#v}_linux_amd64.tar.gz"
cosign verify-blob \
  --certificate "$archive.pem" \
  --signature   "$archive.sig" \
  --certificate-identity-regexp '^https://github\.com/RenseiAI/donmai/' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  "$archive"
```

A signature that verifies against any *other* identity is a failure, not a pass —
always pass both `--certificate-identity-regexp` and `--certificate-oidc-issuer`.
Omitting them accepts a signature from anyone.

### Verify build provenance

Separate claim from the signature. The signature says "this workflow signed this
blob"; the attestation says "this artifact was built from this source, at this
commit, by this workflow":

```bash
gh attestation verify "$archive" --repo RenseiAI/donmai
```

## Verify the Homebrew cask

On an automatic stable tag push, `.goreleaser.yaml` writes the generated cask
directly to `RenseiAI/homebrew-tap/Casks/donmai.rb` using
`HOMEBREW_TAP_GITHUB_TOKEN`. Manual retries and prereleases set the supported
`homebrew_casks[].skip_upload` policy and cannot modify the tap. The normal
stable release path does not open a tap pull request. The generated commit
message is `Brew cask update for donmai version vX.Y.Z`.

```bash
brew update
brew upgrade --cask RenseiAI/tap/donmai
donmai --version
brew cat --cask RenseiAI/tap/donmai
```

Confirm that the cask version, four platform URLs, and SHA-256 values match the
GitHub release. If a daemon from an older cask is resident, restart it after the
upgrade:

```bash
brew services restart donmai
```

If the automated tap write fails, open a focused change in
`RenseiAI/homebrew-tap` that updates `Casks/donmai.rb` from the published
release assets. Do not hand-edit the generated cask in this repository.

## macOS signing and notarization

The release job runs on macOS and imports the Developer ID Application
certificate into an ephemeral keychain. GoReleaser v2.17.1 runs the relevant
phases in this order:

```text
build -> binary_signs -> archives -> checksums -> signs -> publish
```

The Darwin and Linux builds have separate IDs. `binary_signs` selects only the
Darwin ID and invokes the staged current `scripts/sign-and-notarize.sh` before
any archive exists. For each Darwin binary, the script:

1. Rejects archive or non-Darwin input.
2. Signs the binary with hardened runtime and a timestamp.
3. Verifies the embedded signature before the notary service round trip.
4. Wraps the binary in a temporary ZIP accepted by `notarytool`.
5. Requires `notarytool` to return `status: Accepted`.
6. Verifies the exact binary GoReleaser will archive has a Developer ID
   Application authority and is not only linker-signed.
7. Emits the archive-named `.codesign.txt` record registered by `binary_signs`.

GoReleaser then packages the already signed binaries and computes checksums over
those final archives. The remaining `signs` entries only read their disjoint
final inputs: one keyless-signs archives and one keyless-signs `checksums.txt`.
Both declare `certificate:`, so their `.sig` and `.pem` outputs are registered
for publication. Linux binaries are not Apple code-signed and do not receive a
`.codesign.txt` record; their final archives still receive Sigstore signatures
and certificates.

A `.tar.gz` archive cannot carry a stapled notarization ticket. Gatekeeper
checks the accepted notarization ticket online when a quarantined binary first
runs. Do not use `stapler validate` as an acceptance check for these release
archives.

After extracting a Darwin release archive, verify its signature independently:

```bash
codesign --verify --verbose=2 ./donmai
codesign -dvvv ./donmai 2>&1 | grep '^Authority='
```

Expected evidence includes a Developer ID Application authority. The release
job's successful `notarytool submit --wait` result remains the direct
notarization record.

Exercise Gatekeeper with the same quarantined first-launch path a user gets from
a browser download:

1. Download the Darwin archive from the GitHub release in Safari or another
   quarantine-aware browser, then extract it with Archive Utility. `gh release
   download` and `curl` normally do not attach the quarantine metadata, so they
   are useful for checksum verification but do not exercise Gatekeeper.
2. Confirm the extracted binary still has quarantine metadata. If the chosen
   browser or extractor did not propagate it, add equivalent test metadata
   explicitly before installation:

   ```bash
   xattr -p com.apple.quarantine ./donmai || \
     xattr -w com.apple.quarantine \
       "0081;$(printf '%x' "$(date +%s)");Safari;$(uuidgen)" ./donmai
   ```

3. Move the binary into a clean versioned install directory without removing
   that attribute, confirm it is still present, and launch it. Do not use
   `xattr -d`, Finder's **Open Anyway**, or a prior allow decision for this copy.

   ```bash
   install_dir="$HOME/.local/libexec/donmai-${tag#v}"
   mkdir -p "$install_dir"
   mv ./donmai "$install_dir/donmai"
   xattr -p com.apple.quarantine "$install_dir/donmai"
   "$install_dir/donmai" --version
   ```

A successful first launch with network access is the consumer-side Gatekeeper
check for this bare CLI artifact. Repeat with a freshly downloaded copy when
retesting; Gatekeeper can remember a prior decision.

`spctl --assess --type execute --verbose=4 "$install_dir/donmai"` is useful
supplementary diagnostics, but it is not the acceptance gate. On current macOS
versions, `spctl` can reject a valid signed and notarized bare Mach-O executable
because it is not an app bundle, installer package, or disk image. If it does
accept the binary, `source=Notarized Developer ID` is useful corroborating
evidence; a bare-CLI rejection does not override successful `codesign`, the
accepted `notarytool` submission, and the quarantined first launch.

## Smoke-test checklist

Run on at least one supported macOS host and one Linux environment:

```text
[ ] donmai --version reports vX.Y.Z
[ ] donmai --help lists the expected top-level commands
[ ] donmai status exits successfully or reports the expected disconnected state
[ ] donmai agent list exits successfully or reports the expected auth state
[ ] donmai governor start --help renders usage
[ ] donmai linear --help renders usage
[ ] donmai dashboard --help renders usage
[ ] checksums.txt verifies all downloaded archives
[ ] Darwin binary passes codesign and a fresh quarantined first launch
[ ] Homebrew cask installs the same version and SHA-256 values
[ ] Versioned GHCR worker image exists
[ ] E2B target donmai-worker:vX.Y.Z exists for the release build
[ ] Automatic stable release moved donmai-worker:default to that same E2B build
[ ] Prerelease publication left GitHub Latest, GHCR latest, E2B default, and the stable Homebrew cask unchanged
```

## Failure and rollback

- **Release workflow failed before publication:** fix the cause and manually
  rerun the same existing tag. The tag still identifies the same commit; the
  retry intentionally skips the Homebrew publisher, so repair a missing current
  stable cask through the focused tap path described above.
- **Published binary is broken:** do not move or recreate the tag. Revert or fix
  on `main` and publish the next patch version.
- **Generated Homebrew cask is broken:** revert the generated cask commit in
  `RenseiAI/homebrew-tap`, then publish a corrected patch release. Users can
  install the prior GitHub release archive while the cask is corrected.
- **Signing or notarization failed:** do not publish unsigned Darwin artifacts
  as the same version. Correct the certificate, keychain, or notary credentials
  and rerun the unchanged tag only if no inconsistent release assets were
  published; otherwise issue a new patch version.
- **Worker image or E2B build failed independently:** rerun its workflow for the
  same existing tag. Do not move the tag to pick up unrelated fixes.
- **An unsigned or lightweight tag was created:** do not delete, replace, or
  retarget it. Preserve that evidence, fix the release preparation on `main`,
  and publish the next patch version as a new signed tag.

For a hotfix, branch from the affected tag, apply the minimal fix, merge the fix
back to `main`, and release the next normal patch version such as `v0.53.1`.
