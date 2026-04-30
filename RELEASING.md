# Releasing agentfactory-tui

This document covers the full release process for the `af` binary: version bump, changelog, goreleaser, GitHub release creation, Homebrew tap update, smoke-test checklist, and rollback.

## Table of Contents

- [Overview](#overview)
- [Cross-compile matrix](#cross-compile-matrix)
- [Prerequisites](#prerequisites)
- [Step-by-step release process](#step-by-step-release-process)
  - [1. Prepare the release branch](#1-prepare-the-release-branch)
  - [2. Update CHANGELOG.md](#2-update-changelogmd)
  - [3. Bump the version](#3-bump-the-version)
  - [4. Commit and tag](#4-commit-and-tag)
  - [5. Push and trigger goreleaser](#5-push-and-trigger-goreleaser)
  - [6. Verify the GitHub release](#6-verify-the-github-release)
  - [7. Verify the Homebrew tap](#7-verify-the-homebrew-tap)
  - [8. Smoke-test checklist](#8-smoke-test-checklist)
- [Homebrew tap details](#homebrew-tap-details)
- [macOS signing](#macos-signing)
- [Rollback procedure](#rollback-procedure)
- [Hotfix releases](#hotfix-releases)

---

## Overview

`agentfactory-tui` ships a single binary, `af`, as an open-source release under `github.com/RenseiAI/agentfactory-tui`. Releases are fully automated via [goreleaser](https://goreleaser.com) triggered by a `v*` tag push. The goreleaser config is `.goreleaser.yaml` at the repo root.

Consumers install via Homebrew:

```bash
brew install RenseiAI/tap/af
```

Or by downloading a tarball directly from the [GitHub Releases page](https://github.com/RenseiAI/agentfactory-tui/releases).

---

## Cross-compile matrix

goreleaser builds the following targets (see `.goreleaser.yaml`):

| OS     | Arch  | Archive name                  |
|--------|-------|-------------------------------|
| darwin | amd64 | `af_<version>_darwin_amd64.tar.gz` |
| darwin | arm64 | `af_<version>_darwin_arm64.tar.gz` |
| linux  | amd64 | `af_<version>_linux_amd64.tar.gz`  |
| linux  | arm64 | `af_<version>_linux_arm64.tar.gz`  |

`CGO_ENABLED=0` is set for all targets, producing fully static binaries. A `checksums.txt` file containing SHA-256 digests for all archives is published alongside the release.

---

## Prerequisites

- Go 1.23+ (`go version`)
- [goreleaser](https://goreleaser.com/install/) v2+ (`goreleaser --version`)
- GitHub CLI (`gh auth status`)
- `GITHUB_TOKEN` env var with `repo` + `write:packages` scopes (goreleaser uses this automatically)
- `HOMEBREW_TAP_GITHUB_TOKEN` env var with write access to `RenseiAI/homebrew-tap`
- For darwin signing (REN-1412): `APPLE_DEVELOPER_ID_CERT_BASE64`, `APPLE_DEVELOPER_ID_CERT_PASSWORD`, `APPLE_DEVELOPER_ID`, `APPLE_PASSWORD`, `APPLE_TEAM_ID` configured as org-level GitHub Actions secrets — see [macOS signing](#macos-signing). Local dry-runs (`goreleaser release --snapshot`) skip signing per goreleaser convention.
- All tests passing locally: `make test && make lint`

---

## Step-by-step release process

### 1. Prepare the release branch

Start from a clean, up-to-date `main`:

```bash
git checkout main
git pull --ff-only
make test
make lint
```

Resolve any failures before continuing.

### 2. Update CHANGELOG.md

Open `CHANGELOG.md` and add a new section at the top for the upcoming version. Follow the existing format:

```markdown
## vX.Y.Z — YYYY-MM-DD

### Features

- **Feature name** — One-line description of what was added and why.

### Fixes

- **Bug name** — One-line description of what was fixed and the impact.

### Chores

- Dependency bumps, CI changes, internal refactors with no user-visible effect.
```

Commit guidelines:
- Use the bolded short label pattern: `**Label** — description`
- Group under `Features`, `Fixes`, `Chores` (omit empty sections)
- Keep entries user-focused — describe impact, not implementation

### 3. Bump the version

goreleaser derives the version from the git tag. There is no separate version file to edit unless `main.version` is injected via ldflags — check `.goreleaser.yaml` under `builds[].ldflags` if applicable.

For patch releases (bug fixes): increment the `Z` in `vX.Y.Z`.
For minor releases (new features, backward compatible): increment the `Y`, reset `Z` to 0.
For major releases (breaking changes): increment the `X`, reset `Y` and `Z` to 0.

### 4. Commit and tag

```bash
git add CHANGELOG.md
git commit -m "chore(release): prepare vX.Y.Z"

git tag -a vX.Y.Z -m "vX.Y.Z"
```

### 5. Push and trigger goreleaser

```bash
git push origin main
git push origin vX.Y.Z
```

The `v*` tag push triggers the CI release workflow (`.github/workflows/release.yml` or equivalent), which runs:

```bash
goreleaser release --clean
```

goreleaser will:
1. Cross-compile all targets from the matrix above
2. Package each binary into a `tar.gz` archive
3. Generate `checksums.txt`
4. Create a GitHub release with all artifacts attached
5. Update the Homebrew cask at `RenseiAI/homebrew-tap/Casks/af.rb`

To run a dry-run locally (no publish):

```bash
goreleaser release --snapshot --clean
```

Artifacts appear in `dist/`.

### 6. Verify the GitHub release

```bash
gh release view vX.Y.Z --repo RenseiAI/agentfactory-tui
```

Confirm:
- All four platform archives are attached
- `checksums.txt` is attached
- Release notes match CHANGELOG.md entry
- Release is not marked as a draft

### 7. Verify the Homebrew tap

```bash
brew update
brew upgrade RenseiAI/tap/af
af --version
```

Expected output: `af version vX.Y.Z` (or the injected ldflags version string).

Check the tap formula directly:

```bash
brew cat RenseiAI/tap/af
```

Confirm the `version` field and `sha256` in the cask match the published release.

---

## Homebrew tap details

goreleaser automatically opens a PR against `renseiai/homebrew-tap` (repo: `github.com/RenseiAI/homebrew-tap`) updating `Casks/af.rb` with the new version URL and SHA-256. The token used is `HOMEBREW_TAP_GITHUB_TOKEN`.

**Manual update** (if goreleaser tap update fails):

1. Clone `RenseiAI/homebrew-tap`
2. Edit `Casks/af.rb`: update `version` and the two `sha256` entries (darwin/linux, or whichever the cask tracks)
3. Open a PR; merge once CI passes

The tap formula lives at:
`https://github.com/RenseiAI/homebrew-tap/blob/main/Casks/af.rb`

---

## macOS signing

Per REN-1412, all darwin builds of `af` are **signed** with the Apple Developer ID Application certificate (Yuisei-iOS enrollment) and **notarized** via Apple's `notarytool` service. The notarization ticket is **stapled** to each archive so binaries are Gatekeeper-clean offline (a fresh Mac with no network can launch `af` without a callback to Apple's servers).

This eliminates the Gatekeeper popup that previously required users to approve `af` in System Settings → Privacy & Security after `brew install RenseiAI/tap/af`.

### How it works

The release pipeline runs on `macos-latest` (required for `codesign` + `xcrun notarytool`). The `notarize.macos` block in `.goreleaser.yaml` drives signing + notarization in one pass:

1. The base64 `.p12` cert is decoded into an ephemeral keychain on the runner.
2. `codesign --options runtime --timestamp --force` signs each darwin binary with the Developer ID Application identity (hardened runtime is mandatory for notarization).
3. The signed archive is submitted to `notarytool submit --wait`. Apple typically returns a verdict in 5–15 min.
4. On success, `xcrun stapler staple` attaches the notarization ticket to the archive.

Cross-compiled linux binaries skip signing entirely (the `notarize.macos` block is darwin-only).

### Required GitHub Actions secrets

These are configured at the **org level** so both `agentfactory-tui` and `rensei-tui` pick them up. Per the AC, each has a fine-grained PAT scope where applicable.

| Secret | Description | How to derive |
|--------|-------------|---------------|
| `APPLE_DEVELOPER_ID_CERT_BASE64` | The Developer ID Application `.p12` cert, base64-encoded | Export the cert from Keychain Access as a `.p12`, then `base64 -i developer-id.p12 \| pbcopy` |
| `APPLE_DEVELOPER_ID_CERT_PASSWORD` | The export password set when generating the `.p12` | Set during the Keychain export dialog |
| `APPLE_DEVELOPER_ID` | The Apple ID email used for the certificate | The Apple ID that owns the Yuisei-iOS Developer enrollment |
| `APPLE_PASSWORD` | An **app-specific password** for `notarytool` (NOT the regular Apple ID password) | Generate at https://appleid.apple.com → Sign-In and Security → App-Specific Passwords |
| `APPLE_TEAM_ID` | The 10-character Apple Developer Team ID | Apple Developer portal → Membership → Team ID |

### Verifying a signed/notarized release

After a release ships, on a fresh Mac:

```bash
brew install RenseiAI/tap/af

# 1. Confirm Gatekeeper accepts it without popup intervention.
spctl --assess --verbose /opt/homebrew/bin/af
# Expected output:
#   /opt/homebrew/bin/af: accepted
#   source=Notarized Developer ID

# 2. Confirm the binary is signed with the right identity.
codesign -dvv /opt/homebrew/bin/af 2>&1 | grep "Authority="
# Expected:
#   Authority=Developer ID Application: <Yuisei-iOS team name>
#   Authority=Developer ID Certification Authority
#   Authority=Apple Root CA

# 3. Confirm the notarization ticket is stapled.
stapler validate /opt/homebrew/bin/af
# Expected: "The validate action worked!"

# 4. Confirm hardened runtime is enabled.
codesign -d --entitlements - /opt/homebrew/bin/af
# Expected: hardened runtime flag visible in code-signing flags
```

If any check fails, the release was **not** correctly notarized — file a regression issue and roll back.

### Certificate renewal cadence

Apple Developer ID Application certificates are valid for **5 years**. Track the expiry date in REN's calendar (the original Yuisei-iOS cert was issued at enrollment time — check the cert in Keychain Access for the exact `Not Valid After` date). Set a reminder ~3 months before expiry to:

1. Generate a new `.p12` from the same Apple Developer enrollment.
2. Update the `APPLE_DEVELOPER_ID_CERT_BASE64` and `APPLE_DEVELOPER_ID_CERT_PASSWORD` org secrets.
3. Cut a small patch release to verify the new cert works end-to-end (run all four `verify` steps above).

### Local dry-run convention

`goreleaser release --snapshot --clean` skips the `signs` and `notarize` blocks entirely (per goreleaser convention). Local snapshot builds produce **unsigned** binaries — useful for testing the build matrix, but they will trigger Gatekeeper popups on macOS. Real signing only fires on tag-pushed CI runs that have all five `APPLE_*` secrets set.

---

## Smoke-test checklist

Run these checks after installing the new binary from Homebrew or a direct download:

```
[ ] af --version                          prints vX.Y.Z
[ ] af --help                             lists all top-level subcommands
[ ] af status                             exits 0 (or expected error if not connected)
[ ] af agent list                         exits 0 or expected auth error
[ ] af governor start --help              shows usage
[ ] af linear --help                      shows usage
[ ] af dashboard --help                   shows usage (if applicable)
[ ] Binary runs on darwin/amd64           tested locally or on Intel Mac
[ ] Binary runs on darwin/arm64           tested locally or on Apple Silicon Mac
[ ] Binary runs on linux/amd64            tested in Docker: docker run --rm -v $(pwd)/dist:/dist ubuntu /dist/af_vX.Y.Z_linux_amd64/af --version
[ ] Binary runs on linux/arm64            tested in Docker or ARM VM
[ ] checksums.txt SHA-256 verified        sha256sum -c checksums.txt (from release page)
[ ] spctl --assess --verbose /opt/homebrew/bin/af  → accepted, source=Notarized Developer ID
[ ] stapler validate /opt/homebrew/bin/af  → ticket stapled (Gatekeeper-offline-clean)
[ ] codesign -dvv /opt/homebrew/bin/af  → Authority=Developer ID Application
[ ] No System Settings → Privacy & Security popup on first launch (REN-1412 regression check)
```

---

## Rollback procedure

### If the release is broken before Homebrew propagates

1. Delete the GitHub release (keeps the tag):
   ```bash
   gh release delete vX.Y.Z --repo RenseiAI/agentfactory-tui --yes
   ```
2. Delete the tag remotely and locally:
   ```bash
   git push origin --delete vX.Y.Z
   git tag -d vX.Y.Z
   ```
3. Fix the issue in a new commit on `main`.
4. Re-tag and re-release with the same version number (if the bug was pre-publish) or a new patch version (if users already pulled the release).

### If the Homebrew cask is broken

1. Revert the cask PR in `RenseiAI/homebrew-tap` (use GitHub's "Revert" button on the merged PR).
2. This restores the previous formula. Users who already ran `brew upgrade` can pin:
   ```bash
   brew pin RenseiAI/tap/af
   ```
   and manually install the previous version from its tarball.

### If a binary is functionally broken post-release

Issue a patch release (`vX.Y.Z+1`) using the standard flow. Announce in release notes that the prior patch is broken and should be upgraded.

---

## Hotfix releases

For urgent fixes to a released version (when `main` has moved forward):

```bash
git checkout vX.Y.Z
git checkout -b hotfix/vX.Y.Z+1
# apply fix
git add .
git commit -m "fix: <description>"
git tag -a vX.Y.Z+1 -m "vX.Y.Z+1 hotfix"
git push origin hotfix/vX.Y.Z+1 vX.Y.Z+1
# merge hotfix branch back to main
```

Then follow the standard release flow from step 5 onward.
