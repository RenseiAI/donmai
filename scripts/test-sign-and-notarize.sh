#!/usr/bin/env bash
set -euo pipefail

root_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
sign_script="${root_dir}/scripts/sign-and-notarize.sh"
run_goreleaser=false
case "${1:-}" in
  "") ;;
  --goreleaser) run_goreleaser=true ;;
  *)
    printf 'usage: %s [--goreleaser]\n' "$0" >&2
    exit 2
    ;;
esac
temp_dir=$(mktemp -d)
trap 'rm -rf "${temp_dir}"' EXIT

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}

fake_bin="${temp_dir}/fake-bin"
artifact_dir="${temp_dir}/dist/donmai_darwin_arm64_v8.0"
mkdir -p "${fake_bin}" "${artifact_dir}"

printf '%s\n' \
  '#!/usr/bin/env bash' \
  'set -euo pipefail' \
  'printf "security-find-identity\\n" >> "${SIGN_TEST_LOG}"' \
  'printf "1) TESTHASH \"Developer ID Application: Test Identity (TESTTEAM)\"\\n"' \
  > "${fake_bin}/security"

printf '%s\n' \
  '#!/usr/bin/env bash' \
  'set -euo pipefail' \
  'case "${1:-}" in' \
  '  --force)' \
  '    for arg in "$@"; do target=$arg; done' \
  '    printf "codesign-sign\\n" >> "${SIGN_TEST_LOG}"' \
  '    printf "\\nMOCK-DEVELOPER-ID-SIGNED\\n" >> "$target"' \
  '    ;;' \
  '  --verify)' \
  '    for arg in "$@"; do target=$arg; done' \
  '    printf "codesign-verify\\n" >> "${SIGN_TEST_LOG}"' \
  '    grep -Fq MOCK-DEVELOPER-ID-SIGNED "$target"' \
  '    ;;' \
  '  -dvvv)' \
  '    printf "codesign-inspect\\n" >> "${SIGN_TEST_LOG}"' \
  '    printf "Authority=Developer ID Application: Test Identity (TESTTEAM)\\n" >&2' \
  '    ;;' \
  '  *) exit 2 ;;' \
  'esac' \
  > "${fake_bin}/codesign"

printf '%s\n' \
  '#!/usr/bin/env bash' \
  'set -euo pipefail' \
  'printf "notary-submit\\n" >> "${SIGN_TEST_LOG}"' \
  'printf "  status: %s\\n" "${NOTARY_STATUS:-Accepted}"' \
  > "${fake_bin}/xcrun"

chmod 0755 "${fake_bin}/security" "${fake_bin}/codesign" "${fake_bin}/xcrun"

artifact="${artifact_dir}/donmai"
record="${temp_dir}/dist/donmai_1.2.3_darwin_arm64.tar.gz.codesign.txt"
operation_log="${temp_dir}/operations.log"
printf '#!/bin/sh\nexit 0\n' > "$artifact"
chmod 0755 "$artifact"

PATH="${fake_bin}:${PATH}" \
SIGN_TEST_LOG="$operation_log" \
APPLE_DEVELOPER_ID=test@example.com \
APPLE_PASSWORD=test-password \
APPLE_TEAM_ID=TESTTEAM \
  "$sign_script" "$artifact" "$record"

grep -Fq MOCK-DEVELOPER-ID-SIGNED "$artifact" || fail 'Darwin binary was not mutated before archiving'
grep -Fqx 'Notarization: Accepted' "$record" || fail 'notarization record does not prove acceptance'
grep -Fqx 'Archive: donmai_1.2.3_darwin_arm64.tar.gz' "$record" || fail 'notarization record does not name the final archive'

expected_log="${temp_dir}/expected.log"
printf '%s\n' \
  security-find-identity \
  codesign-sign \
  codesign-verify \
  notary-submit \
  codesign-inspect \
  > "$expected_log"
cmp -s "$expected_log" "$operation_log" || fail 'codesign/notarization operations ran out of order'

archive="${temp_dir}/dist/donmai_1.2.3_darwin_arm64.tar.gz"
printf 'archive\n' > "$archive"
archive_error="${temp_dir}/archive.error"
if "$sign_script" "$archive" "$record" > /dev/null 2> "$archive_error"; then
  fail 'archive input was accepted'
fi
grep -Fq 'requires a pre-archive Darwin binary' "$archive_error" || fail 'archive rejection did not explain the pre-archive contract'

invalid_artifact="${temp_dir}/dist/donmai_darwin_amd64_v1/donmai"
invalid_record="${temp_dir}/dist/donmai_1.2.3_darwin_amd64.tar.gz.codesign.txt"
mkdir -p "$(dirname "$invalid_artifact")"
printf '#!/bin/sh\nexit 0\n' > "$invalid_artifact"
chmod 0755 "$invalid_artifact"
invalid_error="${temp_dir}/invalid.error"
if PATH="${fake_bin}:${PATH}" \
   SIGN_TEST_LOG="$operation_log" \
   NOTARY_STATUS=Invalid \
   APPLE_DEVELOPER_ID=test@example.com \
   APPLE_PASSWORD=test-password \
   APPLE_TEAM_ID=TESTTEAM \
     "$sign_script" "$invalid_artifact" "$invalid_record" > /dev/null 2> "$invalid_error"; then
  fail 'non-Accepted notarization status was accepted'
fi
[[ ! -e "$invalid_record" ]] || fail 'failed notarization emitted an uploadable success record'

if [[ "$run_goreleaser" == true ]]; then
  ln -s "$sign_script" "${fake_bin}/donmai-sign-and-notarize"
  printf '%s\n' \
    '#!/usr/bin/env bash' \
    'set -euo pipefail' \
    'signature=' \
    'certificate=' \
    'artifact=' \
    'for arg in "$@"; do' \
    '  case "$arg" in' \
    '    --output-signature=*) signature=${arg#*=} ;;' \
    '    --output-certificate=*) certificate=${arg#*=} ;;' \
    '    --*) ;;' \
    '    sign-blob) ;;' \
    '    *) artifact=$arg ;;' \
    '  esac' \
    'done' \
    ': "${signature:?missing output signature}"' \
    ': "${certificate:?missing output certificate}"' \
    ': "${artifact:?missing artifact}"' \
    'digest=$(shasum -a 256 "$artifact")' \
    'digest=${digest%% *}' \
    'printf "%s\\n" "$digest" > "$signature"' \
    'printf "test certificate for %s\\n" "$(basename "$artifact")" > "$certificate"' \
    > "${fake_bin}/cosign"
  chmod 0755 "${fake_bin}/cosign"

  pipeline_log="${temp_dir}/goreleaser.log"
  if ! (
    cd "$root_dir"
    PATH="${fake_bin}:${PATH}" \
    SIGN_TEST_LOG="$operation_log" \
    APPLE_DEVELOPER_ID=test@example.com \
    APPLE_PASSWORD=test-password \
    APPLE_TEAM_ID=TESTTEAM \
    GORELEASER_PUBLISH_HOMEBREW=false \
    GORELEASER_MAKE_LATEST=false \
    GOWORK=off \
      goreleaser release --snapshot --clean > "$pipeline_log" 2>&1
  ); then
    cat "$pipeline_log" >&2
    fail 'GoReleaser signed snapshot failed'
  fi

  binary_sign_line=$(grep -nF '• signing binaries' "$pipeline_log" | head -1 | cut -d: -f1 || true)
  archive_line=$(grep -nF '• archives' "$pipeline_log" | head -1 | cut -d: -f1 || true)
  checksum_line=$(grep -nF '• calculating checksums' "$pipeline_log" | head -1 | cut -d: -f1 || true)
  artifact_sign_line=$(grep -nF '• signing artifacts' "$pipeline_log" | head -1 | cut -d: -f1 || true)
  [[ -n "$binary_sign_line" && -n "$archive_line" && -n "$checksum_line" && -n "$artifact_sign_line" ]] || fail 'GoReleaser omitted a required signing phase'
  (( binary_sign_line < archive_line )) || fail 'GoReleaser archived before Apple signing'
  (( archive_line < checksum_line )) || fail 'GoReleaser checksummed before final archive creation'
  (( checksum_line < artifact_sign_line )) || fail 'GoReleaser keyless-signed before final checksums'

  python3 - "${root_dir}/dist/artifacts.json" "$root_dir" <<'PY'
import json
import pathlib
import sys

artifacts = json.loads(pathlib.Path(sys.argv[1]).read_text())
root = pathlib.Path(sys.argv[2])
types = [artifact["type"] for artifact in artifacts]
expected = {"Archive": 4, "Checksum": 1, "Signature": 7, "Certificate": 5}
for artifact_type, count in expected.items():
    actual = types.count(artifact_type)
    if actual != count:
        raise SystemExit(f"expected {count} {artifact_type} artifacts, got {actual}")

codesign = [
    artifact for artifact in artifacts
    if artifact["type"] == "Signature" and artifact["name"].endswith(".codesign.txt")
]
if (
    len(codesign) != 2
    or any("_darwin_" not in artifact["name"] for artifact in codesign)
    or any(artifact.get("extra", {}).get("ID") != "macos-codesign-and-notarize" for artifact in codesign)
):
    raise SystemExit("codesign records are not exactly the two Darwin artifacts")

for artifact in artifacts:
    if artifact["type"] in {"Archive", "Checksum", "Signature", "Certificate"}:
        path = pathlib.Path(artifact["path"])
        if not path.is_absolute():
            path = root / path
        if not path.is_file():
            raise SystemExit(f"registered artifact is missing: {artifact['path']}")
PY

  (
    cd "${root_dir}/dist"
    shasum -a 256 -c checksums.txt
    for signature in *.sig; do
      signed_artifact=${signature%.sig}
      expected_digest=$(cat "$signature")
      actual_digest=$(shasum -a 256 "$signed_artifact" | awk '{print $1}')
      [[ "$actual_digest" == "$expected_digest" ]] || fail "signature was not made from final artifact: $signed_artifact"
    done
  )

  for archive_path in "${root_dir}"/dist/*_darwin_*.tar.gz; do
    extract_dir="${temp_dir}/extract-$(basename "$archive_path")"
    mkdir -p "$extract_dir"
    tar -xzf "$archive_path" -C "$extract_dir"
    grep -aFq MOCK-DEVELOPER-ID-SIGNED "$extract_dir/donmai" || fail "Darwin archive does not contain pre-archive signed binary: $archive_path"
  done

  printf 'GoReleaser signed pipeline test: PASS\n'
fi

printf 'sign-and-notarize tests: PASS\n'
