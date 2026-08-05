#!/usr/bin/env bash
#
# Sign + notarize one Darwin binary before GoReleaser archives it.
#
# Invoked by GoReleaser's `binary_signs:` block (see .goreleaser.yaml). Native
# codesign remains necessary because Go's strict x509 parser rejects Apple's
# Developer ID critical extension; native notarytool also supports the Apple ID
# credential shape used by this release workflow.
#
# The pipeline contract is deliberate and fail-closed:
#   1. GoReleaser builds a Darwin binary.
#   2. This command signs and notarizes that binary in place.
#   3. This command verifies the exact binary that GoReleaser will package.
#   4. GoReleaser creates the archive, calculates its final checksum, and only
#      then keyless-signs the final archive and checksum manifest.
#
# Archive input is rejected. This prevents a future config regression from
# reintroducing stale checksums or concurrent in-place archive mutation.
# Tar archives cannot carry a stapled notarization ticket; Gatekeeper checks the
# accepted ticket online when the quarantined binary first runs.

set -euo pipefail

binary="${1:?usage: $0 <darwin-binary-path> <record-path>}"
record="${2:?usage: $0 <darwin-binary-path> <record-path>}"

case "$binary" in
  *.tar|*.tar.gz|*.tgz|*.zip)
    echo "::error::sign-and-notarize requires a pre-archive Darwin binary, not $binary" >&2
    exit 1
    ;;
  *_darwin_*/donmai) ;;
  *)
    echo "::error::sign-and-notarize rejected non-Darwin build artifact: $binary" >&2
    exit 1
    ;;
esac

case "$record" in
  *.tar.gz.codesign.txt) ;;
  *)
    echo "::error::sign-and-notarize record must end in .tar.gz.codesign.txt: $record" >&2
    exit 1
    ;;
esac

: "${APPLE_DEVELOPER_ID:?APPLE_DEVELOPER_ID is required}"
: "${APPLE_PASSWORD:?APPLE_PASSWORD is required}"
: "${APPLE_TEAM_ID:?APPLE_TEAM_ID is required}"

echo "sign-and-notarize: processing $binary"

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

binname="$(basename "$binary")"
identifier="com.renseiai.${binname}"

echo "sign-and-notarize: codesigning $binary (identifier=$identifier)"
codesign --force \
  --options=runtime \
  --timestamp \
  --identifier "$identifier" \
  --sign "$IDENTITY" \
  "$binary"

# Verify before paying for a notary service round trip.
codesign --verify --verbose=2 "$binary"

zip_path="${tmpdir}/notarize-${binname}.zip"
echo "sign-and-notarize: zipping $binary -> $zip_path for notarytool submit"
(cd "$(dirname "$binary")" && zip -j -q "$zip_path" "$binname")

echo "sign-and-notarize: submitting $zip_path to notarytool..."
notarize_log="${tmpdir}/notarytool.log"
if ! xcrun notarytool submit "$zip_path" \
      --apple-id "$APPLE_DEVELOPER_ID" \
      --password "$APPLE_PASSWORD" \
      --team-id "$APPLE_TEAM_ID" \
      --wait \
      --timeout 20m 2>&1 | tee "$notarize_log"; then
  echo "::error::notarytool submit failed for $binary"
  exit 1
fi

if ! grep -Eiq '^[[:space:]]*status:[[:space:]]*Accepted[[:space:]]*$' "$notarize_log"; then
  echo "::error::notarytool did not report status: Accepted for $binary"
  echo "----- notarytool output -----"
  cat "$notarize_log"
  echo "-----------------------------"
  exit 1
fi

echo "sign-and-notarize: notarization Accepted for $binary"

# Confirm the binary that GoReleaser will package carries a Developer ID
# Application signature and is not only linker-signed. Capture output first so
# pipefail cannot turn grep's early success into a spurious SIGPIPE failure.
verify_log="${tmpdir}/codesign.log"
echo "sign-and-notarize: verifying $binary"
codesign -dvvv "$binary" > "$verify_log" 2>&1
if ! grep -qE '^Authority=Developer ID Application:' "$verify_log"; then
  echo "::error::signed binary $binary is missing Developer ID Application signature"
  cat "$verify_log"
  exit 1
fi
if grep -q 'linker-signed' "$verify_log"; then
  echo "::error::signed binary $binary is still linker-signed"
  cat "$verify_log"
  exit 1
fi

# binary_signs registers this archive-named record as an uploadable signature
# artifact. The Apple signature itself remains embedded in the binary.
{
  echo "Signature: embedded (codesign + notarytool)"
  echo "Identity: $IDENTITY"
  echo "Notarization: Accepted"
  echo "Archive: $(basename "${record%.codesign.txt}")"
  echo "Verify: tar -xzf <archive>; codesign -dvvv <binary>"
} > "$record"

echo "sign-and-notarize: done — $binary"
