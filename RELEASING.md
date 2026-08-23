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

## Prerequisites

- A clean, current `main` checkout.
- Go 1.26.6 or newer. `go.mod` is the canonical toolchain floor used by CI,
  security scanning, E2B builds, and release builds.
- GoReleaser v2.17.1 for local dry-runs. The release workflow pins this exact
  version because its verified phase order is part of the signing contract.
- GitHub CLI authenticated to `RenseiAI/donmai`.
- A tag-signing key configured for Git (`user.signingkey` and the matching
  `gpg.format`), with its public key registered as a GitHub signing key.
  Release tags must be signed, not merely annotated.
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

- a creation-only ruleset with one `OrganizationAdmin` `always` bypass; and
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

The audit must report exact no-bypass immutability and a sole
`OrganizationAdmin(always)` creation actor. The transient local `GH_TOKEN`
above must read `current_user_can_bypass=always` for creation and `never` for
immutability. Never copy that administrator token into an Actions secret. CI
also performs a read-only same-token visibility control: structural fields must
be visible, while actor field omission is recorded as the reason the
administrator audit remains required.

Always create the tag at an explicit commit SHA, never from an implicit branch
ref:

```bash
tag=vX.Y.Z
release_sha="$(git rev-parse HEAD)"
git tag -s "$tag" "$release_sha" -m "$tag"
git tag -v "$tag"
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
