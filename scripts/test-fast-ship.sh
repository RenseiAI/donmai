#!/usr/bin/env bash
# Tests for scripts/fast-ship.sh (`make ship`).
#
# Drives the real script against throwaway repositories with a local bare
# origin and a fake gh, and pins two properties: every refusal happens before
# anything changes (no status, push, tag or worktree edit), and a real run lands
# exactly the tested tree as a fast-forward with its attestation and a signed
# tag. Run from scripts/test-release-workflows.sh (CI: release-contracts).
set -euo pipefail

root_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
subject="${root_dir}/scripts/fast-ship.sh"
temp_dir=$(mktemp -d)
trap 'rm -rf "${temp_dir}"' EXIT

for tool in git jq make ssh-keygen; do
  command -v "${tool}" >/dev/null 2>&1 || { printf 'FAIL: %s is required\n' "${tool}" >&2; exit 1; }
done
[[ -x "${subject}" ]] || { printf 'FAIL: scripts/fast-ship.sh is missing or not executable\n' >&2; exit 1; }

failures=0
passes=0
fail_case() {
  printf 'FAIL [%s]: %s\n' "${case_name}" "$1" >&2
  [[ -z "${2:-}" ]] || sed 's/^/    /' "$2" >&2
  failures=$((failures + 1))
}

# Tracker IDs and brand words are assembled at run time so this file itself
# passes the guard it is testing.
tracker_id="$(printf 'R%sN-%s' E 1234)"
brand_word="$(printf 'Ren%si' se)"

ssh-keygen -q -t ed25519 -N '' -f "${temp_dir}/signing-key"
cat >"${temp_dir}/gitconfig" <<GITCONFIG
[user]
	name = Ship Fixture
	email = ship@example.com
	signingkey = ${temp_dir}/signing-key
[gpg]
	format = ssh
[init]
	defaultBranch = main
[advice]
	detachedHead = false
GITCONFIG
mkdir -p "${temp_dir}/home"
export HOME="${temp_dir}/home"
export GIT_CONFIG_GLOBAL="${temp_dir}/gitconfig"
export GIT_CONFIG_NOSYSTEM=1
unset MAKEFLAGS GOFLAGS

mkdir -p "${temp_dir}/bin"
cat >"${temp_dir}/bin/gh" <<'FAKE_GH'
#!/usr/bin/env bash
set -euo pipefail
d="${FAKE_GH_DIR:?}"
printf '%s\n' "$*" >>"$d/calls"
not_found() { echo 'gh: Not Found (HTTP 404)' >&2; exit 1; }
if [[ "$1" == pr ]]; then
  [[ -f "$d/pr.json" ]] || { echo 'no pull requests found' >&2; exit 1; }
  if [[ "$2" == view ]]; then cat "$d/pr.json"; fi
  exit 0
fi
[[ "$1" == api ]] || { echo "UNEXPECTED gh $*" >>"$d/unexpected"; exit 64; }
shift
method=GET
fields=()
endpoint=''
while [[ $# -gt 0 ]]; do
  case "$1" in
    --method) method="$2"; shift ;;
    --paginate) ;;
    -f) fields+=("$2"); shift ;;
    *) endpoint="$1" ;;
  esac
  shift
done
case "${method} ${endpoint}" in
  "GET repos/"*"/actions/organization-variables"*)
    [[ ! -f "$d/org-vars.fail" ]] || { echo 'HTTP 403' >&2; exit 1; }
    cat "$d/org-vars.json" ;;
  "GET repos/"*"/actions/variables"*) cat "$d/repo-vars.json" ;;
  "GET repos/RenseiAI/donmai") cat "$d/repo.json" ;;
  "GET user") echo '{"login":"operator"}' ;;
  "GET repos/RenseiAI/donmai/releases/tags/"*)
    [[ -f "$d/release-exists" ]] || not_found
    echo '{"draft":false,"tag_name":"x"}' ;;
  "GET repos/RenseiAI/donmai/git/ref/tags/"*)
    object="$(git --git-dir "${FAKE_ORIGIN:?}" rev-parse "refs/tags/${endpoint##*/}" 2>/dev/null)" || not_found
    jq -cn --arg sha "$object" '{object:{sha:$sha,type:"tag"}}' ;;
  "GET repos/RenseiAI/donmai/git/tags/"*)
    if [[ -f "$d/tag-unverified" ]]; then
      echo '{"verification":{"verified":false,"reason":"unknown_key"}}'
    else
      echo '{"verification":{"verified":true,"reason":"valid"}}'
    fi ;;
  "GET repos/RenseiAI/release-tagger/actions/workflows/"*)
    [[ -f "$d/tagger-exists" ]] || not_found
    echo '{"state":"active"}' ;;
  "POST repos/"*"/statuses/"*)
    printf '%s\n' "${fields[@]}" >"$d/status-${endpoint##*/}"
    echo '{}' ;;
  "GET repos/"*"/commits/"*"/status")
    sha="${endpoint%/status}"; sha="${sha##*/}"
    if [[ -f "$d/status-$sha" ]]; then
      state="$(sed -n 's/^state=//p' "$d/status-$sha")"
      context="$(sed -n 's/^context=//p' "$d/status-$sha")"
      jq -cn --arg sha "$sha" --arg s "$state" --arg c "$context" '{sha:$sha,statuses:[{context:$c,state:$s}]}'
    else
      jq -cn --arg sha "$sha" '{sha:$sha,statuses:[]}'
    fi ;;
  *) echo "UNEXPECTED gh api ${method} ${endpoint}" >>"$d/unexpected"; exit 64 ;;
esac
FAKE_GH
chmod +x "${temp_dir}/bin/gh"
export PATH="${temp_dir}/bin:${PATH}"

# new_fixture: a primary clone and a linked worktree on feat/widget holding one
# work commit, with the fake gh answering FAST_LANE=on and admin.
new_fixture() {
  fx="${temp_dir}/case-$((passes + failures + 1))-$RANDOM"
  origin="${fx}/origin.git"
  primary="${fx}/primary"
  wt="${fx}/wt"
  export FAKE_GH_DIR="${fx}/gh"
  export FAKE_ORIGIN="${origin}"
  mkdir -p "${FAKE_GH_DIR}"
  git init --quiet --bare "${origin}"
  git clone --quiet "${origin}" "${primary}" 2>/dev/null
  mkdir -p "${primary}/scripts"
  cp "${root_dir}/scripts/guard-b-lint.sh" "${primary}/scripts/"
  for stub in guard-b-lint-selftest.sh guard-b-diff-gate-selftest.sh check-no-inbound-attach.sh; do
    printf '#!/usr/bin/env bash\necho ok\n' >"${primary}/scripts/${stub}"
  done
  printf '#!/usr/bin/env bash\necho "release-signing-key: fixture key registered"\nexit "${FAKE_SIGNING_KEY_EXIT:-0}"\n' \
    >"${primary}/scripts/verify-release-signing-key.sh"
  cat >"${primary}/scripts/podman-go-test.sh" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
if [[ -n "${FAKE_MOVE_MAIN:-}" ]]; then
  d="$(mktemp -d)"
  git clone --quiet "${FAKE_MOVE_MAIN}" "$d/c"
  git -C "$d/c" commit --quiet --allow-empty -m 'concurrent landing'
  git -C "$d/c" push --quiet origin HEAD:main
fi
echo ok
STUB
  chmod +x "${primary}"/scripts/*.sh
  printf 'lint:\n\t@test -z "$(FAKE_LINT_FAIL)" || { echo "lint: red"; exit 1; }\n\t@echo lint ok\ntest-tagged:\n\t@echo vet ok\nbuild:\n\t@echo build ok\nvuln:\n\t@echo vuln ok\nrelease-dry-run:\n\t@echo snapshot ok\n' \
    >"${primary}/Makefile"
  printf '# Changelog\n\nFormat: `## vX.Y.Z — YYYY-MM-DD`.\n\n---\n\n## [Unreleased]\n\n## v0.1.0 — 2026-09-01\n\n### Features\n\n- First release.\n' \
    >"${primary}/CHANGELOG.md"
  git -C "${primary}" add -A
  git -C "${primary}" commit --quiet -m 'chore(release): prepare v0.1.0'
  git -C "${primary}" tag v0.1.0
  git -C "${primary}" push --quiet origin main --tags
  git -C "${primary}" commit --quiet --allow-empty -m 'fix(host): keep the stable copy on restart (#40)'
  git -C "${primary}" push --quiet origin main
  git -C "${primary}" worktree add --quiet -b feat/widget "${wt}" origin/main
  printf 'widget\n' >"${wt}/widget.txt"
  git -C "${wt}" add widget.txt
  git -C "${wt}" commit --quiet -m 'feat(cli): add the widget command (#9)'
  set_vars '{"variables":[{"name":"FAST_LANE","value":"on"}]}' '{"variables":[]}'
  printf '{"permissions":{"admin":true}}\n' >"${FAKE_GH_DIR}/repo.json"
  unset FAKE_SIGNING_KEY_EXIT FAKE_LINT_FAIL FAKE_MOVE_MAIN
}

set_vars() {
  printf '%s\n' "$1" >"${FAKE_GH_DIR}/org-vars.json"
  printf '%s\n' "$2" >"${FAKE_GH_DIR}/repo-vars.json"
}

snapshot() {
  printf '%s\n%s\n%s\n%s\n' \
    "$(git --git-dir "${origin}" rev-parse refs/heads/main)" \
    "$(git --git-dir "${origin}" tag --list | tr '\n' ' ')" \
    "$(git -C "${wt}" rev-parse HEAD 2>/dev/null || true)" \
    "$(git -C "${wt}" status --porcelain 2>/dev/null || true)"
  cat "${wt}/CHANGELOG.md"
}

# ship <dir> [args...]: runs the subject, leaving its exit code in $code and
# its output in ${fx}/out.
ship() {
  local dir=$1
  shift
  code=0
  (cd "${dir}" && "${subject}" "$@") >"${fx}/out" 2>&1 || code=$?
}

assert_unchanged() {
  if [[ "$(snapshot)" != "$1" ]]; then
    fail_case 'the repository changed' "${fx}/out"
    return 1
  fi
  if grep -Eq -- '--method POST|workflow run' "${FAKE_GH_DIR}/calls" 2>/dev/null; then
    fail_case 'a refused or dry run wrote to GitHub' "${FAKE_GH_DIR}/calls"
    return 1
  fi
  if [[ -f "${FAKE_GH_DIR}/unexpected" ]]; then
    fail_case 'unexpected gh calls' "${FAKE_GH_DIR}/unexpected"
    return 1
  fi
}

# expect_refusal <name> <exit> <message> <setup-function> [args...]: runs the
# case in real and dry-run mode; both must refuse the same way and change
# nothing.
expect_refusal() {
  local name=$1 want_code=$2 want=$3 setup=$4 before dir mode
  shift 4
  for mode in real dry; do
    case_name="${name} (${mode})"
    new_fixture
    dir="${wt}"
    "${setup}"
    before="$(snapshot)"
    if [[ "${mode}" == dry ]]; then
      ship "${dir}" "$@" --dry-run
    else
      ship "${dir}" "$@"
    fi
    if [[ "${code}" != "${want_code}" ]]; then
      fail_case "exit ${code}, want ${want_code}" "${fx}/out"
      continue
    fi
    if ! grep -Fq -- "${want}" "${fx}/out"; then
      fail_case "output does not say: ${want}" "${fx}/out"
      continue
    fi
    assert_unchanged "${before}" && passes=$((passes + 1))
  done
}

none() { :; }
switch_unset() { set_vars '{"variables":[]}' '{"variables":[]}'; }
switch_off() { set_vars '{"variables":[{"name":"FAST_LANE","value":"off"}]}' '{"variables":[]}'; }
switch_upper() { set_vars '{"variables":[{"name":"FAST_LANE","value":"ON"}]}' '{"variables":[]}'; }
switch_repo_off() { set_vars '{"variables":[{"name":"FAST_LANE","value":"on"}]}' '{"variables":[{"name":"FAST_LANE","value":"off"}]}'; }
vars_unreadable() { : >"${FAKE_GH_DIR}/org-vars.fail"; }
not_admin() { printf '{"permissions":{"admin":false,"push":true}}\n' >"${FAKE_GH_DIR}/repo.json"; }
in_primary() { dir="${primary}"; }
detached() { git -C "${wt}" checkout --quiet --detach; }
dirty() { printf 'x\n' >"${wt}/stray.txt"; }
behind() {
  git -C "${primary}" commit --quiet --allow-empty -m 'someone else landed'
  git -C "${primary}" push --quiet origin main
}
remote_diverged() {
  git -C "${primary}" push --quiet origin main:refs/heads/feat/widget
  git -C "${primary}" commit --quiet --allow-empty -m 'pushed from elsewhere'
  git -C "${primary}" push --quiet origin HEAD:refs/heads/feat/widget
  git -C "${primary}" reset --quiet --soft HEAD~1
}
local_tag() { git -C "${primary}" tag v0.1.1; }
release_exists() { : >"${FAKE_GH_DIR}/release-exists"; }
pins_set() { set_vars '{"variables":[{"name":"FAST_LANE","value":"on"},{"name":"RELEASE_TAG_SIGNERS","value":"bot ssh-ed25519 AAAA"}]}' '{"variables":[]}'; }
key_not_ready() { export FAKE_SIGNING_KEY_EXIT=1; }
two_commits() { git -C "${wt}" commit --quiet --allow-empty -m 'fix(cli): second change'; }
tracker_title() { git -C "${wt}" commit --quiet --amend -m "feat(cli): add the widget command (${tracker_id})"; }
brand_changelog() {
  sed -i.bak "s/^## \\[Unreleased\\]\$/## [Unreleased]\\
\\
### Fixes\\
\\
- Works with ${brand_word} hosts./" "${wt}/CHANGELOG.md"
  rm -f "${wt}/CHANGELOG.md.bak"
  git -C "${wt}" commit --quiet --amend --no-edit -a
}
tracker_branch() { git -C "${wt}" branch --quiet -m "feat/$(printf 'r%sn-%s' e 77)-widget"; }

expect_refusal 'switch unset' 3 "FAST_LANE is not on (it is 'unset')" switch_unset
expect_refusal 'switch off' 3 "FAST_LANE is not on (it is 'off')" switch_off
expect_refusal 'switch not exactly on' 3 "FAST_LANE is not on (it is 'ON')" switch_upper
expect_refusal 'repository variable overrides the switch' 3 "FAST_LANE is not on (it is 'off')" switch_repo_off
expect_refusal 'switch unreadable' 3 'cannot read the organization Actions variables' vars_unreadable
expect_refusal 'not an admin' 3 'is not an admin' not_admin
expect_refusal 'primary checkout' 3 'this is the primary checkout' in_primary
expect_refusal 'detached HEAD' 3 'HEAD is detached' detached
expect_refusal 'uncommitted changes' 3 'uncommitted or untracked changes' dirty
expect_refusal 'behind origin/main' 3 'HEAD does not contain origin/main' behind
expect_refusal 'remote branch diverged' 3 'origin/feat/widget has commits this worktree does not' remote_diverged
expect_refusal 'version leap' 3 'is not the smallest increment from v0.1.0' none --version v0.3.0
expect_refusal 'local tag exists' 3 'tag v0.1.1 already exists' local_tag
expect_refusal 'release exists' 3 'RenseiAI/donmai already has a v0.1.1 release' release_exists
expect_refusal 'pins set, local signing' 3 'use --tagger central' pins_set --tagger local
expect_refusal 'pins set, no central tagger' 3 'does not exist, and the tagging-identity pins are set' pins_set
expect_refusal 'central tagger missing' 3 'does not exist yet; use --tagger local' none --tagger central
expect_refusal 'signing key not ready' 3 'the local signing key is not ready' key_not_ready
expect_refusal 'several commits, no title' 3 "pass TITLE='...'" two_commits
expect_refusal 'tracker ID in the commit message' 3 'guard-b blocked the ship commit message' tracker_title
expect_refusal 'brand word in the CHANGELOG section' 3 'guard-b blocked the changelog section' brand_changelog
expect_refusal 'tracker slug in the branch name' 3 'the branch name fails guard-b' tracker_branch
expect_refusal 'malformed version' 2 '--version must be vX.Y.Z' none --version 0.1.1
expect_refusal 'unknown tagger' 2 '--tagger must be auto, local or central' none --tagger host
expect_refusal 'unknown flag' 2 'unknown argument: --force' none --force

# Dry run: composes the section from landed subjects and changes nothing.
case_name='dry run composes and changes nothing'
new_fixture
before="$(snapshot)"
ship "${wt}" --dry-run
if [[ "${code}" != 0 ]]; then
  fail_case "exit ${code}" "${fx}/out"
else
  ok=true
  for want in 'CHANGELOG v0.1.1 section: composed' '| - Add the widget command' '| - Keep the stable copy on restart' \
    'guard-b: the commit message, CHANGELOG section and release-note subjects are clean' \
    'would fast-forward main' 'would sign and push tag v0.1.1' 'dry run complete: nothing was changed'; do
    grep -Fq -- "${want}" "${fx}/out" || { fail_case "output missing: ${want}" "${fx}/out"; ok=false; break; }
  done
  if [[ "${ok}" == true ]] && grep -Eq '^    \|.*\(#[0-9]+\)' "${fx}/out"; then
    fail_case 'the composed section kept a PR number' "${fx}/out"
    ok=false
  fi
  [[ "${ok}" == false ]] || { assert_unchanged "${before}" && passes=$((passes + 1)); }
fi

# Real run: one squashed fast-forward, attested, tagged and signed.
case_name='lands the tested tree and tags it'
new_fixture
old_main="$(git --git-dir "${origin}" rev-parse refs/heads/main)"
ship "${wt}" --no-watch
if [[ "${code}" != 0 ]]; then
  fail_case "exit ${code}" "${fx}/out"
else
  sha="$(git --git-dir "${origin}" rev-parse refs/heads/main)"
  message="$(git -C "${wt}" log -1 --format=%B "${sha}")"
  if [[ "$(git -C "${wt}" rev-parse HEAD)" != "${sha}" ]]; then
    fail_case 'origin main is not the worktree HEAD' "${fx}/out"
  elif [[ "$(git -C "${wt}" rev-parse "${sha}^")" != "${old_main}" ]]; then
    fail_case 'the landing is not one commit on the old main' "${fx}/out"
  elif [[ -n "$(git -C "${wt}" status --porcelain)" ]]; then
    fail_case 'the worktree is not clean after landing' "${fx}/out"
  elif ! grep -q '^Local-Verify-Gates: guard lint test-tagged test-podman build$' <<<"${message}" ||
    ! grep -q '^Fast-Lane: on$' <<<"${message}" || ! grep -q '^Local-Verify-Host: ' <<<"${message}"; then
    fail_case "trailers missing: ${message}" "${fx}/out"
  elif ! git -C "${wt}" show "${sha}:CHANGELOG.md" | grep -q '^## v0.1.1 — '; then
    fail_case 'the landed CHANGELOG has no v0.1.1 section' "${fx}/out"
  elif ! grep -qx 'state=success' "${FAKE_GH_DIR}/status-${sha}" || ! grep -qx 'context=local-verify' "${FAKE_GH_DIR}/status-${sha}"; then
    fail_case 'no local-verify=success status on the landed SHA' "${fx}/out"
  elif [[ "$(git --git-dir "${origin}" rev-parse 'v0.1.1^{commit}')" != "${sha}" ]]; then
    fail_case 'tag v0.1.1 does not point at the landed SHA' "${fx}/out"
  elif ! git --git-dir "${origin}" cat-file tag v0.1.1 | grep -q -- '-----BEGIN SSH SIGNATURE-----'; then
    fail_case 'tag v0.1.1 is not SSH-signed' "${fx}/out"
  elif [[ "$(git --git-dir "${origin}" rev-parse refs/heads/feat/widget)" != "${sha}" ]]; then
    fail_case 'the work branch was not pushed at the landed SHA' "${fx}/out"
  else
    passes=$((passes + 1))
  fi
fi

case_name='a tag GitHub cannot verify fails the ship'
new_fixture
: >"${FAKE_GH_DIR}/tag-unverified"
ship "${wt}" --no-watch
if [[ "${code}" != 1 ]] || ! grep -Fq 'GitHub does not verify the v0.1.1 signature (false unknown_key)' "${fx}/out"; then
  fail_case "exit ${code}, want 1 with an unverified tag" "${fx}/out"
else
  passes=$((passes + 1))
fi

case_name='a red gate restores and lands nothing'
new_fixture
before="$(snapshot)"
export FAKE_LINT_FAIL=1
ship "${wt}" --no-watch
unset FAKE_LINT_FAIL
if [[ "${code}" != 1 ]] || ! grep -Fq 'gate lint is red; nothing was committed or pushed' "${fx}/out"; then
  fail_case "exit ${code}, want 1 with a red lint gate" "${fx}/out"
elif ! grep -Fq 'restored CHANGELOG.md after the failure' "${fx}/out"; then
  fail_case 'the composed CHANGELOG was not restored' "${fx}/out"
else
  assert_unchanged "${before}" && passes=$((passes + 1))
fi

case_name='main moving during the gates stops the landing'
new_fixture
old_main="$(git --git-dir "${origin}" rev-parse refs/heads/main)"
export FAKE_MOVE_MAIN="${origin}"
ship "${wt}" --no-watch
unset FAKE_MOVE_MAIN
now_main="$(git --git-dir "${origin}" rev-parse refs/heads/main)"
if [[ "${code}" != 1 ]] || ! grep -Fq 'main moved while the gates ran' "${fx}/out"; then
  fail_case "exit ${code}, want 1 because main moved" "${fx}/out"
elif [[ "${now_main}" == "${old_main}" || "${now_main}" == "$(git -C "${wt}" rev-parse HEAD)" ]]; then
  fail_case 'main should hold only the concurrent landing' "${fx}/out"
elif [[ -n "$(git --git-dir "${origin}" tag --list v0.1.1)" ]]; then
  fail_case 'a tag was pushed after a failed landing' "${fx}/out"
else
  passes=$((passes + 1))
fi

# make ship passes only explicit inputs.
case_name='make ship flags'
make_line="$(make -s -n -C "${root_dir}" ship VERSION=v1.2.3 TITLE='feat: widget' TAGGER=central FULL=1 DRY_RUN=1 NO_WATCH=1)"
if [[ "${make_line}" != './scripts/fast-ship.sh --version "v1.2.3" --title "feat: widget" --tagger "central" --full --dry-run --no-watch' ]]; then
  printf '%s\n' "${make_line}" >"${temp_dir}/make.out"
  fail_case 'make ship did not pass the inputs through' "${temp_dir}/make.out"
elif [[ "$(make -s -n -C "${root_dir}" ship | sed 's/[[:space:]]*$//')" != './scripts/fast-ship.sh' ]]; then
  fail_case 'a plain make ship passed flags nobody asked for' ''
else
  passes=$((passes + 1))
fi

printf 'fast-ship tests: %d passed, %d failed\n' "${passes}" "${failures}"
[[ "${failures}" -eq 0 ]]
