# Releasing donmai

This document describes the release path for the `donmai` binary, its GitHub
release artifacts, the `ghcr.io/renseiai/donmai-worker` image, the E2B worker
template, and the generated Homebrew cask.

## Release outputs

A `v*` tag push starts three independent workflows:

- `.github/workflows/release.yml` signs, notarizes, packages, and publishes the
  `donmai` binary with GoReleaser.
- `.github/workflows/worker-image.yml` publishes the versioned and `latest`
  worker images to GHCR.
- `.github/workflows/e2b-template.yml` publishes an E2B target named
  `donmai-worker:<version>` with the release version embedded in the binary and,
  on automatic tag pushes only, advances the rolling `donmai-worker:default`
  target to the same build.

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

## Prerequisites

- A clean, current `main` checkout.
- Go 1.25.12 or newer. `go.mod` is the canonical toolchain floor used by CI,
  security scanning, E2B builds, and release builds.
- GoReleaser v2 for local dry-runs.
- GitHub CLI authenticated to `RenseiAI/donmai`.
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
   the production signing pipe. It does not publish, tag, or modify the
   Homebrew tap.

4. Commit the release preparation and merge it to `main`. Do not release from
   an unmerged branch or a dirty checkout.

## Create the release tag

Tags are immutable release inputs. Publisher tags must be strict semantic
versions: `vMAJOR.MINOR.PATCH`, optionally followed by well-formed SemVer
prerelease identifiers such as `-rc.1`. Numeric identifiers cannot contain
leading zeroes. Build metadata, arbitrary suffixes, trailing dots, mutable
labels such as `latest`, and branch names are rejected.

Always create the tag at an explicit commit SHA, never from an implicit branch
ref:

```bash
tag=vX.Y.Z
release_sha="$(git rev-parse HEAD)"
git tag -a "$tag" "$release_sha" -m "$tag"
git show --no-patch --decorate "$tag^{commit}"
git push origin "refs/tags/$tag"
```

The tag push starts the release, worker-image, and E2B workflows. The release
workflow verifies that its checkout is the commit referenced by the release
tag. GoReleaser also sends that exact commit through
`release.target_commitish`; it never asks GitHub to target the moving default
branch.

Do not move or reuse a published tag. If a release is bad, fix it and publish a
new patch version.

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
workflow's verifier, E2B wrapper, and GoReleaser latest policy are staged outside
the workspace before that tag checkout, allowing retries of tags that predate
the hardening. Before signing, building, or publishing, the shared verifier
enforces the release-tag grammar, proves detached `HEAD`, and proves that `HEAD`
equals the tag's peeled commit. The verified tag is exported as
`GORELEASER_CURRENT_TAG` and is also the only source for published image,
template, and embedded binary versions.

A manual binary retry sets GoReleaser's supported `release.make_latest` policy
to false, so replaying an older release cannot replace the repository's current
GitHub Latest release. Automatic tag pushes retain the normal make-latest
behavior. A manual worker-image retry republishes only the versioned image and
leaves `latest` unchanged. A manual E2B retry creates or updates only
`donmai-worker:vX.Y.Z` and leaves `donmai-worker:default` unchanged; automatic
tag pushes assign both the version tag and `default` to the new E2B build.
Neither E2B path writes back to the repository.

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
- After a manual retry of an older tag, `/releases/latest` still returns the
  previously current release rather than the retried tag.
- All four archives, `checksums.txt`, and expected `.sig` provenance files are
  attached.
- Archive names follow the `donmai_<version>_<os>_<arch>.tar.gz` template.
- Release notes cover the final `CHANGELOG.md` entry.

Download the assets and verify their checksums:

```bash
gh release download "$tag" --repo RenseiAI/donmai --dir "dist/$tag"
cd "dist/$tag"
shasum -a 256 -c checksums.txt
```

## Verify the Homebrew cask

`.goreleaser.yaml` writes the generated cask directly to
`RenseiAI/homebrew-tap/Casks/donmai.rb` using
`HOMEBREW_TAP_GITHUB_TOKEN`. The normal release path does not open a tap pull
request. The generated commit message is `Brew cask update for donmai version
vX.Y.Z`.

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
certificate into an ephemeral keychain. GoReleaser invokes
`scripts/sign-and-notarize.sh` for each archive.

For each Darwin archive, the script:

1. Extracts the archive.
2. Signs each executable with hardened runtime and a timestamp.
3. Wraps each signed executable in a temporary ZIP accepted by `notarytool`.
4. Requires `notarytool` to return `status: Accepted`.
5. Repacks the signed executable into the original tarball.
6. Verifies that the repacked binary has a Developer ID Application authority.

Linux archives are not code-signed and receive a provenance sidecar explaining
that fact.

A `.tar.gz` archive cannot carry a stapled notarization ticket. Gatekeeper
checks the accepted notarization ticket online when the binary first runs. Do
not use `stapler validate` as an acceptance check for these release archives.

After extracting a Darwin release archive, verify it with:

```bash
codesign --verify --verbose=2 ./donmai
codesign -dvvv ./donmai 2>&1 | grep '^Authority='
spctl --assess --verbose ./donmai
```

Expected evidence includes a Developer ID Application authority and
`source=Notarized Developer ID`.

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
[ ] Darwin binary passes codesign and spctl checks
[ ] Homebrew cask installs the same version and SHA-256 values
[ ] Versioned GHCR worker image exists
[ ] E2B target donmai-worker:vX.Y.Z exists for the release build
[ ] Automatic release moved donmai-worker:default to that same E2B build
```

## Failure and rollback

- **Release workflow failed before publication:** fix the cause and manually
  rerun the same existing tag. The tag still identifies the same commit.
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

For a hotfix, branch from the affected tag, apply the minimal fix, merge the fix
back to `main`, and release the next normal patch version such as `v0.53.1`.
