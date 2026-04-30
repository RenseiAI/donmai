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
#      runtime (--options=runtime, mandatory for notarization) and a stable
#      bundle identifier (--identifier com.renseiai.<binary>).
#   3. Wrap each signed binary in a temp .zip and submit to Apple's
#      notarytool service. notarytool only accepts .zip / .pkg / .dmg —
#      submitting a .tar.gz directly fails. Apple ID + app-specific
#      password is used (matches our existing secret shape — not App Store
#      Connect API keys).
#   4. Assert notarytool reports `status: Accepted`. Anything else fails
#      the script and the release job.
#   5. Repack the signed binaries into the original .tar.gz path so
#      downstream archive consumers (checksums.txt, GitHub release upload)
#      see the signed artifact.
#   6. Verify the final archive by extracting and running `codesign -dvvv`
#      — must show a Developer ID Authority chain (NOT linker-signed).
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

# Sign + notarize each executable inside the archive.
#
# notarytool only accepts .zip / .pkg / .dmg, so we zip each signed binary
# individually for submission. The .tar.gz repack happens after notarization
# completes (step 5). Bundle identifier is derived from the binary's basename
# (e.g. `af` -> `com.renseiai.af`, `rensei` -> `com.renseiai.rensei`).
while IFS= read -r -d '' bin; do
  binname="$(basename "$bin")"
  identifier="com.renseiai.${binname}"

  echo "sign-and-notarize: codesigning $bin (identifier=$identifier)"
  codesign --force \
    --options=runtime \
    --timestamp \
    --identifier "$identifier" \
    --sign "$IDENTITY" \
    "$bin"

  # Sanity-check the signature locally before paying for a notarytool round trip.
  codesign --verify --verbose=2 "$bin"

  zip_path="$(mktemp -t "notarize-${binname}").zip"
  rm -f "$zip_path"
  echo "sign-and-notarize: zipping $bin -> $zip_path for notarytool submit"
  (cd "$(dirname "$bin")" && zip -j -q "$zip_path" "$binname")

  echo "sign-and-notarize: submitting $zip_path to notarytool..."
  notarize_log="$(mktemp)"
  if ! xcrun notarytool submit "$zip_path" \
        --apple-id "$APPLE_DEVELOPER_ID" \
        --password "$APPLE_PASSWORD" \
        --team-id "$APPLE_TEAM_ID" \
        --wait \
        --timeout 20m 2>&1 | tee "$notarize_log"; then
    echo "::error::notarytool submit failed for $bin"
    rm -f "$zip_path" "$notarize_log"
    exit 1
  fi

  # notarytool prints `  status: Accepted` on success. Anything else
  # (Invalid, In Progress timeout, etc.) is a hard failure.
  if ! grep -Eiq '^[[:space:]]*status:[[:space:]]*Accepted[[:space:]]*$' "$notarize_log"; then
    echo "::error::notarytool did not report status: Accepted for $bin"
    echo "----- notarytool output -----"
    cat "$notarize_log"
    echo "-----------------------------"
    rm -f "$zip_path" "$notarize_log"
    exit 1
  fi

  echo "sign-and-notarize: notarization Accepted for $bin"
  rm -f "$zip_path" "$notarize_log"
done < <(find "$tmpdir" -type f -perm -111 -print0)

# Repack with the original name now that the binaries are signed + notarized.
(cd "$tmpdir" && tar -czf "$archive.signed" .)
mv "$archive.signed" "$archive"

# Final verification: re-extract the repacked archive and confirm the
# binaries carry a Developer ID Application signature (NOT linker-signed).
verify_dir="$(mktemp -d)"
tar -xzf "$archive" -C "$verify_dir"
while IFS= read -r -d '' bin; do
  echo "sign-and-notarize: verifying $bin"
  if ! codesign -dvvv "$bin" 2>&1 | tee /tmp/codesign-verify.txt | grep -qE '^Authority=Developer ID Application:'; then
    echo "::error::repacked archive binary $bin is missing Developer ID Application signature"
    cat /tmp/codesign-verify.txt
    rm -rf "$verify_dir"
    exit 1
  fi
  if grep -q 'linker-signed' /tmp/codesign-verify.txt; then
    echo "::error::repacked archive binary $bin is still linker-signed"
    cat /tmp/codesign-verify.txt
    rm -rf "$verify_dir"
    exit 1
  fi
done < <(find "$verify_dir" -type f -perm -111 -print0)
rm -rf "$verify_dir"

echo "sign-and-notarize: done — $archive"
