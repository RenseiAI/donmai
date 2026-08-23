#!/usr/bin/env bash
set -euo pipefail

root_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
subject="${root_dir}/scripts/verify-release-signing-key.sh"
temp_dir=$(mktemp -d)
trap 'rm -rf "${temp_dir}"' EXIT

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}

assert_contains() {
  local expected=$1
  local file=$2
  grep -Fq -- "${expected}" "${file}" || fail "missing '${expected}' in ${file}"
}

[[ -x "${subject}" ]] || fail 'release signing-key preflight is missing or not executable'

fixture_repo="${temp_dir}/repo"
fixture_bin="${temp_dir}/bin"
fixture_data="${temp_dir}/data"
mkdir -p "${fixture_bin}" "${fixture_data}"
git init -q "${fixture_repo}"
git -C "${fixture_repo}" config user.name 'Release Test'
git -C "${fixture_repo}" config user.email 'release-test@example.com'
git -C "${fixture_repo}" config gpg.format ssh

ssh-keygen -q -t ed25519 -N '' -f "${fixture_data}/release-key"
ssh-keygen -q -t ed25519 -N '' -f "${fixture_data}/other-key"
release_public_key=$(<"${fixture_data}/release-key.pub")
other_public_key=$(<"${fixture_data}/other-key.pub")
release_fingerprint=$(printf '%s\n' "${release_public_key}" | ssh-keygen -lf - -E sha256 | awk '{print $2}')
release_key_body=$(awk '{print $2}' <<<"${release_public_key}")

printf '{"login":"release-test"}\n' >"${fixture_data}/user.json"
jq -cn --arg key "${release_public_key}" '[{id:1,title:"release fixture",key:$key}]' >"${fixture_data}/release-keys.json"
jq -cn --arg key "${other_public_key}" '[{id:2,title:"other fixture",key:$key}]' >"${fixture_data}/other-keys.json"
printf '[]\n' >"${fixture_data}/no-keys.json"

cat >"${fixture_bin}/gh" <<'FAKE_GH'
#!/usr/bin/env bash
set -euo pipefail

[[ "${1:-}" == api ]] || exit 64
endpoint=${!#}
case "${endpoint}" in
  user)
    [[ "${GH_FIXTURE_USER_MODE:-valid}" != fail ]] || exit 1
    cat "${GH_FIXTURE_USER}"
    ;;
  users/*/ssh_signing_keys?per_page=100)
    [[ "${GH_FIXTURE_KEYS_MODE:-valid}" != fail ]] || exit 1
    printf '['
    cat "${GH_FIXTURE_KEYS}"
    printf ']\n'
    ;;
  *) exit 65 ;;
esac
FAKE_GH
chmod 0755 "${fixture_bin}/gh"

run_subject() {
  local keys_file=$1
  shift
  (
    cd "${fixture_repo}"
    PATH="${fixture_bin}:${PATH}" \
      GH_FIXTURE_USER="${fixture_data}/user.json" \
      GH_FIXTURE_KEYS="${keys_file}" \
      "${subject}" "$@"
  )
}

exercise_fingerprint_mismatch() {
  local output="${temp_dir}/mismatch-output"
  local error="${temp_dir}/mismatch-error"

  git -C "${fixture_repo}" config user.signingkey "${fixture_data}/release-key.pub"
  if run_subject "${fixture_data}/other-keys.json" >"${output}" 2>"${error}"; then
    fail 'unmatched GitHub SSH signing key was accepted'
  fi
  assert_contains "configured SSH signing key ${release_fingerprint} is not registered" "${error}"
  printf 'release signing-key fingerprint mismatch: PASS\n'
}

if [[ "${1:-}" == --case ]]; then
  [[ $# -eq 2 && "${2}" == fingerprint-mismatch ]] || fail 'unknown focused case'
  exercise_fingerprint_mismatch
  exit 0
fi
[[ $# -eq 0 ]] || fail 'usage: test-release-signing-key.sh [--case fingerprint-mismatch]'

# Public-key paths, private-key paths with .pub sidecars, and literal key::
# configuration all resolve to the same public identity. A path containing
# spaces and a shell-looking literal comment prove values are passed as data.
space_dir="${fixture_data}/path with spaces"
mkdir -p "${space_dir}"
cp "${fixture_data}/release-key" "${space_dir}/release key"
cp "${fixture_data}/release-key.pub" "${space_dir}/release key.pub"

for signing_value in \
  "${space_dir}/release key.pub" \
  "${space_dir}/release key" \
  "key::${release_public_key} comment with spaces; \$(false)"; do
  git -C "${fixture_repo}" config user.signingkey "${signing_value}"
  output="${temp_dir}/valid-output"
  run_subject "${fixture_data}/release-keys.json" >"${output}"
  assert_contains "release-signing-key: GREEN: Git SSH signing key ${release_fingerprint} is registered to GitHub account release-test" "${output}"
  if grep -Fq -- "${release_key_body}" "${output}"; then
    fail 'preflight output leaked public-key bytes instead of reporting only its fingerprint'
  fi
done

exercise_fingerprint_mismatch

git -C "${fixture_repo}" config gpg.format openpgp
if run_subject "${fixture_data}/release-keys.json" >"${temp_dir}/format-output" 2>"${temp_dir}/format-error"; then
  fail 'non-SSH signing format was accepted'
fi
assert_contains 'gpg.format must be exactly ssh' "${temp_dir}/format-error"
git -C "${fixture_repo}" config gpg.format ssh

git -C "${fixture_repo}" config user.signingkey 'key::not-a-public-key'
if run_subject "${fixture_data}/release-keys.json" >"${temp_dir}/invalid-output" 2>"${temp_dir}/invalid-error"; then
  fail 'invalid literal signing key was accepted'
fi
assert_contains 'invalid literal SSH public key' "${temp_dir}/invalid-error"

git -C "${fixture_repo}" config user.signingkey "${fixture_data}/release-key.pub"
if run_subject "${fixture_data}/no-keys.json" >"${temp_dir}/empty-output" 2>"${temp_dir}/empty-error"; then
  fail 'empty GitHub SSH signing-key set was accepted'
fi
assert_contains 'is not registered as an SSH signing key' "${temp_dir}/empty-error"

if GH_FIXTURE_USER_MODE=fail run_subject "${fixture_data}/release-keys.json" >"${temp_dir}/user-fail-output" 2>"${temp_dir}/user-fail-error"; then
  fail 'GitHub account readback failure was accepted'
fi
assert_contains 'could not resolve the active github.com account' "${temp_dir}/user-fail-error"

if GH_FIXTURE_KEYS_MODE=fail run_subject "${fixture_data}/release-keys.json" >"${temp_dir}/keys-fail-output" 2>"${temp_dir}/keys-fail-error"; then
  fail 'GitHub signing-key readback failure was accepted'
fi
assert_contains 'could not read SSH signing keys for GitHub account release-test' "${temp_dir}/keys-fail-error"

printf 'fixture\n' >"${fixture_repo}/fixture.txt"
git -C "${fixture_repo}" add fixture.txt
git -C "${fixture_repo}" commit -q -m fixture
git -C "${fixture_repo}" config user.signingkey "${fixture_data}/release-key"
git -C "${fixture_repo}" tag -s v1.2.3 -m v1.2.3 HEAD
git -C "${fixture_repo}" config gpg.ssh.allowedSignersFile "${fixture_data}/does-not-exist"
verified_output="${temp_dir}/verified-output"
run_subject "${fixture_data}/release-keys.json" --verify-tag v1.2.3 >"${verified_output}"
assert_contains 'release-signing-key: tag GREEN: refs/tags/v1.2.3 is trusted for GitHub account release-test' "${verified_output}"

git -C "${fixture_repo}" -c user.signingkey="${fixture_data}/other-key" tag -s v1.2.4 -m v1.2.4 HEAD
if run_subject "${fixture_data}/release-keys.json" --verify-tag v1.2.4 >"${temp_dir}/wrong-tag-output" 2>"${temp_dir}/wrong-tag-error"; then
  fail 'tag signed by a different key was accepted'
fi
assert_contains 'is not signed by the registered configured SSH key' "${temp_dir}/wrong-tag-error"

preflight_line=$(grep -nF './scripts/verify-release-signing-key.sh' "${root_dir}/RELEASING.md" | sed -n '1s/:.*//p')
tag_line=$(grep -nF "git tag -s \"\$tag\"" "${root_dir}/RELEASING.md" | sed -n '1s/:.*//p')
verify_line=$(grep -nF "./scripts/verify-release-signing-key.sh --verify-tag \"\$tag\"" "${root_dir}/RELEASING.md" | sed -n '1s/:.*//p')
push_line=$(grep -nF "git push origin \"refs/tags/\$tag\"" "${root_dir}/RELEASING.md" | sed -n '1s/:.*//p')
[[ -n "${preflight_line}" && -n "${tag_line}" && -n "${verify_line}" && -n "${push_line}" ]] ||
  fail 'release docs omit the signing-key preflight/tag verification sequence'
[[ "${preflight_line}" -lt "${tag_line}" && "${tag_line}" -lt "${verify_line}" && "${verify_line}" -lt "${push_line}" ]] ||
  fail 'release docs do not run preflight before tag creation and local verification before push'

printf 'release signing-key preflight tests: PASS\n'
