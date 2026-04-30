#!/usr/bin/env bash
#
# Sign + notarize a darwin tar.gz archive containing macOS binaries.
#
# Invoked by GoReleaser's `signs:` block (see .goreleaser.yaml). Replaces
# the broken built-in `notarize:` block which fails with
# `failed to verify certificate chain: x509: unhandled critical extension`
# (Go's strict x509 parser rejects Apple's critical extension
# 1.2.840.113635.100.6.1.13).
#
# Skips non-darwin archives. For darwin archives:
#   1. Look up the Developer ID Application identity in the keychain
#      (set up by the `Import Apple Developer ID cert to keychain` step
#      in .github/workflows/release.yml).
#   2. Extract the archive, codesign each executable inside with hardened
#      runtime (--options=runtime, mandatory for notarization).
#   3. Repack with the original archive name.
#   4. Submit to Apple's notarytool service via Apple ID + app-specific
#      password (matches our existing secret shape — not App Store Connect
#      API keys).
#
# Note: tar.gz archives can't be stapled. Binaries get notarization tickets
# via online check on first run (Gatekeeper hits Apple's CDN).
#
# Required env vars (provided by .github/workflows/release.yml):
#   APPLE_DEVELOPER_ID   — Apple ID email for the developer account
#   APPLE_PASSWORD       — app-specific password (NOT the account password)
#   APPLE_TEAM_ID        — Apple Developer team ID

set -euo pipefail

archive="${1:?usage: $0 <archive-path>}"

case "$archive" in
  *_darwin_*) ;;
  *)
    echo "sign-and-notarize: skipping non-darwin archive: $archive"
    exit 0
    ;;
esac

echo "sign-and-notarize: processing $archive"

IDENTITY="$(security find-identity -v -p codesigning | awk -F'"' '/Developer ID Application/{print $2; exit}')"
if [ -z "${IDENTITY:-}" ]; then
  echo "::error::Developer ID Application identity not found in keychain"
  echo "Available identities:"
  security find-identity -v -p codesigning || true
  exit 1
fi
echo "sign-and-notarize: using identity: $IDENTITY"

tmpdir="$(mktemp -d)"
trap 'rm -rf "$tmpdir"' EXIT

tar -xzf "$archive" -C "$tmpdir"

# Sign every executable file inside the archive (hardened runtime + timestamp).
while IFS= read -r -d '' bin; do
  echo "sign-and-notarize: codesigning $bin"
  codesign --force --options=runtime --timestamp --sign "$IDENTITY" "$bin"
done < <(find "$tmpdir" -type f -perm -111 -print0)

# Repack with the original name.
(cd "$tmpdir" && tar -czf "$archive.signed" .)
mv "$archive.signed" "$archive"

echo "sign-and-notarize: submitting to notarytool..."
xcrun notarytool submit "$archive" \
  --apple-id "$APPLE_DEVELOPER_ID" \
  --password "$APPLE_PASSWORD" \
  --team-id "$APPLE_TEAM_ID" \
  --wait \
  --timeout 20m

echo "sign-and-notarize: done — $archive"
