#!/usr/bin/env bash
# Tests for scripts/fast-lane.sh (`make ship` and `make release`).
#
# Drives the real script against throwaway repositories with a local bare
# origin and a fake gh, and pins these properties:
#   - every refusal happens before anything changes (no status, push, tag,
#     worktree or checkout edit), in real and dry-run mode;
#   - ship lands exactly the tested tree as one fast-forward with its
#     attestation and never tags;
#   - release publishes only when main has unreleased work, lands the
#     release preparation through the same attested path, tags that SHA,
#     watches every publisher, and survives a re-run after a failure;
#   - with no new work, release says "nothing to release" only when the
#     latest tag's publishers, GitHub release and cask are all complete, and
#     otherwise resumes the watch and fails;
#   - each gate, switch re-read and version re-check stops the run when red.
# Run from scripts/test-release-workflows.sh (CI: release-contracts).
set -euo pipefail

root_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
subject="${root_dir}/scripts/fast-lane.sh"
temp_dir=$(mktemp -d)
trap 'rm -rf "${temp_dir}"' EXIT

for tool in git jq make ssh-keygen base64; do
  command -v "${tool}" >/dev/null 2>&1 || { printf 'FAIL: %s is required\n' "${tool}" >&2; exit 1; }
done
[[ -x "${subject}" ]] || { printf 'FAIL: scripts/fast-lane.sh is missing or not executable\n' >&2; exit 1; }

failures=0
passes=0
fail_case() {
  printf 'FAIL [%s]: %s\n' "${case_name}" "$1" >&2
  [[ -z "${2:-}" ]] || sed 's/^/    /' "$2" >&2
  failures=$((failures + 1))
}
pass_case() { passes=$((passes + 1)); }

# Tracker IDs and brand words are assembled at run time so this file itself
# passes the guard it is testing.
tracker_id="$(printf 'R%sN-%s' E 1234)"
brand_word="$(printf 'Ren%si' se)"
platform="$(uname -s | tr '[:upper:]' '[:lower:]')/$(uname -m)"

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
unset MAKEFLAGS GOFLAGS GH_TOKEN
export FAST_LANE_POLL_SECONDS=1 FAST_LANE_RUN_WAIT_SECONDS=2 FAST_LANE_CASK_WAIT_SECONDS=2

mkdir -p "${temp_dir}/bin"
cat >"${temp_dir}/bin/gh" <<'FAKE_GH'
#!/usr/bin/env bash
set -euo pipefail
d="${FAKE_GH_DIR:?}"
origin="${FAKE_ORIGIN:?}"
printf '%s\n' "$*" >>"$d/calls"
not_found() { echo 'gh: Not Found (HTTP 404)' >&2; exit 1; }
switch_off() { printf '{"variables":[{"name":"FAST_LANE","value":"off"}]}\n' >"$d/org-vars.json"; }
tag_commit() { git --git-dir "${origin}" rev-parse --verify --quiet "refs/tags/$1^{commit}"; }
case "$1" in
  auth) echo fake-token; exit 0 ;;
  pr)
    [[ -f "$d/pr.json" ]] || { echo 'no pull requests found for branch' >&2; exit 1; }
    [[ "$2" != view ]] || cat "$d/pr.json"
    exit 0 ;;
  workflow)
    # The central tagger: tags the requested SHA, or tagger-sha when set.
    sha='' version=''
    for arg in "$@"; do
      case "${arg}" in sha=*) sha="${arg#sha=}" ;; version=*) version="${arg#version=}" ;; esac
    done
    [[ ! -f "$d/tagger-sha" ]] || sha="$(cat "$d/tagger-sha")"
    git --git-dir "${origin}" tag -a "${version}" -m "${version}" "${sha}"
    exit 0 ;;
  run)
    if [[ "$2" == list ]]; then
      [[ ! -f "$d/run-list.fail" ]] || { echo 'HTTP 502: Bad Gateway' >&2; exit 1; }
      workflow=''
      while [[ $# -gt 0 ]]; do [[ "$1" != --workflow ]] || workflow="$2"; shift; done
      case "${workflow}" in release.yml) id=101 ;; worker-image.yml) id=102 ;; *) id=103 ;; esac
      [[ ! -f "$d/runs-missing-${workflow}" ]] || { echo '[]'; exit 0; }
      git --git-dir "${origin}" for-each-ref --format='%(refname:short)' 'refs/tags/v*' |
        while read -r tag; do
          jq -cn --argjson id "${id}" --arg sha "$(tag_commit "${tag}")" --arg tag "${tag}" \
            '{databaseId:$id,headSha:$sha,headBranch:$tag}'
        done | jq -s .
      exit 0
    fi
    [[ ! -f "$d/run-fail-$3" ]] || { echo "run $3 failed" >&2; exit 1; }
    exit 0 ;;
  api) shift ;;
  *) echo "UNEXPECTED gh $*" >>"$d/unexpected"; exit 64 ;;
esac
method=GET
fields=()
endpoint=''
while [[ $# -gt 0 ]]; do
  case "$1" in
    --method) method="$2"; shift ;;
    --header) shift ;;
    --paginate | --slurp) ;;
    -f) fields+=("$2"); shift ;;
    *) endpoint="$1" ;;
  esac
  shift
done
case "${method} ${endpoint}" in
  "GET repos/RenseiAI/donmai/actions/organization-variables"*)
    [[ ! -f "$d/org-vars.fail" ]] || { echo 'HTTP 403' >&2; exit 1; }
    cat "$d/org-vars.json" ;;
  "GET repos/RenseiAI/donmai/actions/variables"*) cat "$d/repo-vars.json" ;;
  "GET repos/RenseiAI/donmai") cat "$d/repo.json" ;;
  "GET user") echo '{"login":"operator"}' ;;
  "GET repos/RenseiAI/donmai/releases/tags/"*)
    tag="${endpoint##*/}"
    [[ ! -f "$d/release-missing" ]] || not_found
    [[ -f "$d/release-exists" ]] || tag_commit "${tag}" >/dev/null || not_found
    draft=false
    [[ ! -f "$d/release-draft" ]] || draft=true
    jq -cn --arg tag "${tag}" --argjson draft "${draft}" '{draft:$draft,tag_name:$tag}' ;;
  "GET repos/RenseiAI/donmai/git/ref/tags/"*)
    tag="${endpoint##*/}"
    object="$(git --git-dir "${origin}" rev-parse --verify --quiet "refs/tags/${tag}")" || not_found
    jq -cn --arg sha "${object}" --arg type "$(git --git-dir "${origin}" cat-file -t "${object}")" '{object:{sha:$sha,type:$type}}' ;;
  "GET repos/RenseiAI/donmai/git/tags/"*)
    if [[ -f "$d/tag-unverified" ]]; then
      echo '{"verification":{"verified":false,"reason":"unknown_key"}}'
    else
      echo '{"verification":{"verified":true,"reason":"valid"}}'
    fi ;;
  "GET repos/RenseiAI/donmai/rulesets?per_page=100") echo '[[{"id":1},{"id":2}]]' ;;
  "GET repos/RenseiAI/donmai/rulesets/"*) cat "$d/ruleset-${endpoint##*/}.json" ;;
  "GET repos/example/tagger")
    if [[ -f "$d/tagger-read-only" ]]; then echo '{"permissions":{"push":false}}'; else echo '{"permissions":{"push":true}}'; fi ;;
  "GET repos/"*"/actions/workflows/"*)
    [[ -f "$d/tagger-exists" ]] || not_found
    echo '{"state":"active"}' ;;
  "POST repos/RenseiAI/donmai/statuses/"*)
    printf '%s\n' "${fields[@]}" >"$d/status-${endpoint##*/}"
    [[ ! -f "$d/switch-off-after-status" ]] || switch_off
    echo '{}' ;;
  "GET repos/RenseiAI/donmai/commits/"*"/status")
    sha="${endpoint%/status}"; sha="${sha##*/}"
    if [[ -f "$d/status-$sha" ]]; then
      state="$(sed -n 's/^state=//p' "$d/status-$sha")"
      context="$(sed -n 's/^context=//p' "$d/status-$sha")"
      jq -cn --arg sha "$sha" --arg s "$state" --arg c "$context" '{sha:$sha,statuses:[{context:$c,state:$s}]}'
    else
      jq -cn --arg sha "$sha" '{sha:$sha,statuses:[]}'
    fi ;;
  "GET repos/RenseiAI/homebrew-tap/contents/Casks/donmai.rb")
    # The cask follows the highest release tag, unless a case holds it back.
    newest="$(git --git-dir "${origin}" for-each-ref --format='%(refname:short)' 'refs/tags/v*' |
      sed 's/^v//' | sort -t. -k1,1n -k2,2n -k3,3n | tail -1)"
    [[ ! -f "$d/cask-version" ]] || newest="$(cat "$d/cask-version")"
    printf 'cask "donmai" do\n  version "%s"\nend\n' "${newest}" | base64 | jq -Rsc '{content:.}' ;;
  *) echo "UNEXPECTED gh api ${method} ${endpoint}" >>"$d/unexpected"; exit 64 ;;
esac
FAKE_GH
chmod +x "${temp_dir}/bin/gh"
export PATH="${temp_dir}/bin:${PATH}"

# new_fixture: an origin whose main is one landed fix past v0.1.0, its primary
# clone on main, and a linked worktree on feat/widget holding one work commit.
# The fake gh answers FAST_LANE=on (organization only), admin, and exact
# release-tag rulesets this login may bypass for creation.
new_fixture() {
  fx="${temp_dir}/case-$((passes + failures + 1))-$RANDOM"
  origin="${fx}/origin.git"
  primary="${fx}/primary"
  wt="${fx}/wt"
  export FAKE_GH_DIR="${fx}/gh"
  export FAKE_ORIGIN="${origin}"
  mkdir -p "${FAKE_GH_DIR}"
  git init --quiet --bare "${origin}"
  # Origin hooks let a case act "elsewhere" at a precise moment.
  cat >"${origin}/hooks/pre-receive" <<'HOOK'
#!/usr/bin/env bash
while read -r old new ref; do
  if [[ "${ref}" == refs/tags/* && -f "${FAKE_GH_DIR}/reject-tag-push" ]]; then
    echo "tag pushes are refused" >&2
    exit 1
  fi
done
HOOK
  cat >"${origin}/hooks/post-receive" <<'HOOK'
#!/usr/bin/env bash
while read -r old new ref; do
  [[ "${ref}" == refs/heads/main ]] || continue
  if [[ -f "${FAKE_GH_DIR}/switch-off-after-main" ]]; then
    printf '{"variables":[{"name":"FAST_LANE","value":"off"}]}\n' >"${FAKE_GH_DIR}/org-vars.json"
  fi
  if [[ -f "${FAKE_GH_DIR}/tag-elsewhere-after-main" ]]; then
    git tag v0.1.1 "${old}"
  fi
done
HOOK
  chmod +x "${origin}/hooks/pre-receive" "${origin}/hooks/post-receive"
  git clone --quiet "${origin}" "${primary}" 2>/dev/null
  mkdir -p "${primary}/scripts"
  cp "${root_dir}/scripts/guard-b-lint.sh" "${root_dir}/scripts/verify-release-authority.sh" "${primary}/scripts/"
  cp "${subject}" "${primary}/scripts/fast-lane.sh"
  # Stubs for the gates and helpers; each can be turned red from the case.
  cat >"${primary}/scripts/guard-b-lint-selftest.sh" <<'STUB'
#!/usr/bin/env bash
echo ok
exit "${FAKE_GUARD_SELFTEST_EXIT:-0}"
STUB
  cat >"${primary}/scripts/check-guard-b-vendor-drift.sh" <<'STUB'
#!/usr/bin/env bash
echo pinned
exit "${FAKE_VENDOR_DRIFT_EXIT:-0}"
STUB
  cat >"${primary}/scripts/test-release-workflows.sh" <<'STUB'
#!/usr/bin/env bash
echo ran >>"${FAKE_GH_DIR}/contracts-ran"
exit "${FAKE_CONTRACTS_EXIT:-0}"
STUB
  cat >"${primary}/scripts/verify-release-signing-key.sh" <<'STUB'
#!/usr/bin/env bash
echo "release-signing-key: fixture key registered"
exit "${FAKE_SIGNING_KEY_EXIT:-0}"
STUB
  for stub in guard-b-diff-gate-selftest.sh check-no-inbound-attach.sh; do
    printf '#!/usr/bin/env bash\necho ok\n' >"${primary}/scripts/${stub}"
  done
  cat >"${primary}/scripts/podman-go-test.sh" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
if [[ -n "${FAKE_MOVE_MAIN:-}" ]]; then
  d="$(mktemp -d)"
  git clone --quiet "${FAKE_MOVE_MAIN}" "$d/c"
  git -C "$d/c" commit --quiet --allow-empty -m 'concurrent landing'
  git -C "$d/c" push --quiet origin HEAD:main
fi
if [[ -n "${FAKE_SWITCH_OFF_DURING_GATES:-}" ]]; then
  printf '{"variables":[{"name":"FAST_LANE","value":"off"}]}\n' >"${FAKE_GH_DIR}/org-vars.json"
fi
echo ok
STUB
  chmod +x "${primary}"/scripts/*.sh
  sed 's/^>/\t/' >"${primary}/Makefile" <<'MAKEFILE'
lint:
>@test -z "$(FAKE_LINT_FAIL)" || { echo "lint: red"; exit 1; }
>@echo lint ok
test-tagged:
>@echo vet ok
build:
>@echo build ok
vuln:
>@echo vuln ok
release-dry-run:
>@echo snapshot ok
MAKEFILE
  cat >"${primary}/CHANGELOG.md" <<'CHANGELOG'
# Changelog

Format: `## vX.Y.Z — YYYY-MM-DD`.

---

## [Unreleased]

No unreleased changes.

## v0.1.0 — 2026-09-01

### Features

- First release.
CHANGELOG
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
  local scope='"source":"RenseiAI/donmai","source_type":"Repository","target":"tag","enforcement":"active","conditions":{"ref_name":{"include":["refs/tags/v*"],"exclude":[]}}'
  printf '{"id":1,%s,"rules":[{"type":"creation"}],"bypass_actors":[{"actor_id":null,"actor_type":"OrganizationAdmin","bypass_mode":"always"}],"current_user_can_bypass":"always"}\n' \
    "${scope}" >"${FAKE_GH_DIR}/ruleset-1.json"
  printf '{"id":2,%s,"rules":[{"type":"deletion"},{"type":"update"},{"type":"non_fast_forward"}],"bypass_actors":[],"current_user_can_bypass":"never"}\n' \
    "${scope}" >"${FAKE_GH_DIR}/ruleset-2.json"
  unset FAKE_SIGNING_KEY_EXIT FAKE_LINT_FAIL FAKE_MOVE_MAIN FAKE_GUARD_SELFTEST_EXIT FAKE_VENDOR_DRIFT_EXIT \
    FAKE_CONTRACTS_EXIT FAKE_SWITCH_OFF_DURING_GATES FAST_LANE_TAG_WAIT_SECONDS
}

set_vars() {
  printf '%s\n' "$1" >"${FAKE_GH_DIR}/org-vars.json"
  printf '%s\n' "$2" >"${FAKE_GH_DIR}/repo-vars.json"
}
origin_main() { git --git-dir "${origin}" rev-parse refs/heads/main; }
# tag_origin <tag> <rev>: a release tag made elsewhere, already fetched here.
tag_origin() {
  git --git-dir "${origin}" tag "$1" "$2"
  git -C "${primary}" fetch --quiet --tags origin
}
origin_tag() { git --git-dir "${origin}" rev-parse --verify --quiet "refs/tags/$1^{commit}" || true; }

snapshot() {
  printf '%s\n' \
    "$(origin_main)" \
    "$(git --git-dir "${origin}" for-each-ref --format='%(refname) %(objectname)' refs/tags refs/heads | tr '\n' ' ')" \
    "$(git -C "${primary}" tag --list | tr '\n' ' ')" \
    "$(git -C "${primary}" worktree list --porcelain | grep -c '^worktree ')" \
    "$(git -C "${primary}" status --porcelain)" \
    "$(git -C "${wt}" rev-parse HEAD 2>/dev/null || true)" \
    "$(git -C "${wt}" status --porcelain 2>/dev/null || true)"
  cat "${wt}/CHANGELOG.md" "${primary}/CHANGELOG.md"
}

# run_subject <dir> <mode> [args...]: runs the subject, leaving its exit code
# in $code and its output in ${fx}/out.
run_subject() {
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
  if grep -Eq -- '--method POST|^workflow run|^pr comment' "${FAKE_GH_DIR}/calls" 2>/dev/null; then
    fail_case 'a refused or dry run wrote to GitHub' "${FAKE_GH_DIR}/calls"
    return 1
  fi
  if [[ -f "${FAKE_GH_DIR}/unexpected" ]]; then
    fail_case 'unexpected gh calls' "${FAKE_GH_DIR}/unexpected"
    return 1
  fi
}

# expect_refusal <mode> <name> <exit> <message> <setup> [args...]: runs the
# case in real and dry-run mode; both must stop the same way and change
# nothing. ship runs in the work worktree, release in the primary checkout;
# a setup may point $dir elsewhere.
expect_refusal() {
  local mode=$1 name=$2 want_code=$3 want=$4 setup=$5 before run
  shift 5
  for run in real dry; do
    case_name="${mode}: ${name} (${run})"
    new_fixture
    dir="${wt}"
    [[ "${mode}" == ship ]] || dir="${primary}"
    "${setup}"
    before="$(snapshot)"
    if [[ "${run}" == dry ]]; then
      run_subject "${dir}" "${mode}" "$@" --dry-run
    else
      run_subject "${dir}" "${mode}" "$@"
    fi
    if [[ "${code}" != "${want_code}" ]]; then
      fail_case "exit ${code}, want ${want_code}" "${fx}/out"
      continue
    fi
    if ! grep -Fq -- "${want}" "${fx}/out"; then
      fail_case "output does not say: ${want}" "${fx}/out"
      continue
    fi
    assert_unchanged "${before}" && pass_case
  done
}

# expect_output <file> <line>...: every line appears in the output.
expect_output() {
  local want
  for want in "$@"; do
    grep -Fq -- "${want}" "${fx}/out" || { fail_case "output missing: ${want}" "${fx}/out"; return 1; }
  done
}

none() { :; }
switch_unset() { set_vars '{"variables":[]}' '{"variables":[]}'; }
switch_off() { set_vars '{"variables":[{"name":"FAST_LANE","value":"off"}]}' '{"variables":[]}'; }
switch_upper() { set_vars '{"variables":[{"name":"FAST_LANE","value":"ON"}]}' '{"variables":[]}'; }
switch_repo_on() { set_vars '{"variables":[]}' '{"variables":[{"name":"FAST_LANE","value":"on"}]}'; }
switch_repo_off() { set_vars '{"variables":[{"name":"FAST_LANE","value":"on"}]}' '{"variables":[{"name":"FAST_LANE","value":"off"}]}'; }
switch_both_on() { set_vars '{"variables":[{"name":"FAST_LANE","value":"on"}]}' '{"variables":[{"name":"FAST_LANE","value":"on"}]}'; }
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
two_commits() { git -C "${wt}" commit --quiet --allow-empty -m 'fix(cli): second change'; }
tracker_title() { git -C "${wt}" commit --quiet --amend -m "feat(cli): add the widget command (${tracker_id})"; }
tracker_branch() { git -C "${wt}" branch --quiet -m "feat/$(printf 'r%sn-%s' e 77)-widget"; }
release_exists() { : >"${FAKE_GH_DIR}/release-exists"; }
pins_set() { set_vars '{"variables":[{"name":"FAST_LANE","value":"on"},{"name":"RELEASE_TAG_SIGNERS","value":"bot ssh-ed25519 AAAA"}]}' '{"variables":[]}'; }
tagger_configured() {
  set_vars '{"variables":[{"name":"FAST_LANE","value":"on"},{"name":"RELEASE_TAGGER_REPOSITORY","value":"example/tagger"}]}' '{"variables":[]}'
}
tagger_read_only() {
  tagger_configured
  : >"${FAKE_GH_DIR}/tagger-exists"
  : >"${FAKE_GH_DIR}/tagger-read-only"
}
tagger_malformed() {
  set_vars '{"variables":[{"name":"FAST_LANE","value":"on"},{"name":"RELEASE_TAGGER_REPOSITORY","value":"not a repository"}]}' '{"variables":[]}'
}
key_not_ready() { export FAKE_SIGNING_KEY_EXIT=1; }
no_tag_right() { jq -c '.current_user_can_bypass = "never"' "${FAKE_GH_DIR}/ruleset-1.json" >"${FAKE_GH_DIR}/r" && mv "${FAKE_GH_DIR}/r" "${FAKE_GH_DIR}/ruleset-1.json"; }
stale_scripts() { printf '# edited\n' >>"${primary}/scripts/check-no-inbound-attach.sh"; }
# stale_self: main moved on to a newer fast-lane.sh than the one running.
stale_self() {
  printf '# newer\n' >>"${primary}/scripts/fast-lane.sh"
  git -C "${primary}" commit --quiet -am 'chore(release): newer train script'
  git -C "${primary}" push --quiet origin main
}
# foreign_local_tag: a local v0.1.1 this script did not make (another lane's).
foreign_local_tag() { git -C "${primary}" tag -a v0.1.1 -m 'manual' v0.1.0; }
# push_main <subject> [file content]: lands a commit on origin main from the
# primary checkout, as a reviewed pull request would.
push_main() {
  if [[ -n "${2:-}" ]]; then
    printf '%s\n' "$2" >>"${primary}/CHANGELOG.md"
    git -C "${primary}" commit --quiet -am "$1"
  else
    git -C "${primary}" commit --quiet --allow-empty -m "$1"
  fi
  git -C "${primary}" push --quiet origin main
}
brand_unreleased() {
  sed -i.bak "s/^No unreleased changes\\.\$/### Fixes\\
\\
- Works with ${brand_word} hosts./" "${primary}/CHANGELOG.md"
  rm -f "${primary}/CHANGELOG.md.bak"
  git -C "${primary}" commit --quiet -am 'docs: note host support'
  git -C "${primary}" push --quiet origin main
}
# A test: subject never reaches the composed section, only the release notes.
tracker_subject() { push_main "test: cover the ${tracker_id} edge case"; }

# --- ship: refusals ------------------------------------------------------------

expect_refusal ship 'switch unset' 3 "the organization FAST_LANE variable is 'unset', not 'on'" switch_unset
expect_refusal ship 'switch off' 3 "the organization FAST_LANE variable is 'off', not 'on'" switch_off
expect_refusal ship 'switch not exactly on' 3 "the organization FAST_LANE variable is 'ON', not 'on'" switch_upper
expect_refusal ship 'a repository variable cannot turn the lane on' 3 "a repository-level FAST_LANE variable exists (value 'on')" switch_repo_on
expect_refusal ship 'a repository variable set to on still refuses' 3 "a repository-level FAST_LANE variable exists (value 'on')" switch_both_on
expect_refusal ship 'a repository variable shadows the organization switch' 3 "a repository-level FAST_LANE variable exists (value 'off')" switch_repo_off
expect_refusal ship 'switch unreadable' 3 'FAILED: could not read the organization Actions variables: HTTP 403' vars_unreadable
expect_refusal ship 'not an admin' 3 'is not an admin' not_admin
expect_refusal ship 'primary checkout' 3 'this is the primary checkout' in_primary
expect_refusal ship 'detached HEAD' 3 'HEAD is detached' detached
expect_refusal ship 'uncommitted changes' 3 'uncommitted or untracked changes' dirty
expect_refusal ship 'behind origin/main' 3 'HEAD does not contain origin/main' behind
expect_refusal ship 'remote branch diverged' 3 'origin/feat/widget has commits this worktree does not' remote_diverged
expect_refusal ship 'several commits, no title' 3 "pass TITLE='...'" two_commits
expect_refusal ship 'tracker ID in the commit message' 3 'guard-b blocked the ship commit message' tracker_title
expect_refusal ship 'tracker slug in the branch name' 3 'the branch name fails guard-b' tracker_branch
expect_refusal ship 'a release option' 2 '--version is a make release option' none --version v0.1.1
expect_refusal ship 'unknown flag' 2 'unknown argument: --force' none --force

# --- ship: runs -------------------------------------------------------------------

case_name='ship: nothing ahead of main'
new_fixture
git -C "${primary}" worktree add --quiet -b feat/empty "${fx}/wt-empty" origin/main
before="$(snapshot)"
run_subject "${fx}/wt-empty" ship
if [[ "${code}" != 0 ]] || ! grep -Fq 'nothing to ship: feat/empty has no commits beyond origin/main' "${fx}/out"; then
  fail_case "exit ${code}, want 0 with nothing to ship" "${fx}/out"
else
  assert_unchanged "${before}" && pass_case
fi

case_name='ship: dry run plans a landing and no tag'
new_fixture
before="$(snapshot)"
run_subject "${wt}" ship --dry-run
if [[ "${code}" != 0 ]]; then
  fail_case "exit ${code}" "${fx}/out"
elif expect_output 'would squash 1 commit(s) onto origin/main' \
  'would run the gates: guard, lint, test-tagged, test-podman, build' \
  'would read FAST_LANE again, then fast-forward main' 'ship never tags'; then
  if grep -Eq 'would .*tag v|CHANGELOG' "${fx}/out"; then
    fail_case 'a ship dry run planned a tag or a CHANGELOG edit' "${fx}/out"
  else
    assert_unchanged "${before}" && pass_case
  fi
fi

# check_landed: origin main is one attested commit on the old main holding the
# worktree's tree, with no tag and no CHANGELOG edit.
check_landed() {
  local old_main=$1 want_gates=$2 sha message
  sha="$(origin_main)"
  message="$(git -C "${wt}" log -1 --format=%B "${sha}")"
  if [[ "$(git -C "${wt}" rev-parse HEAD)" != "${sha}" ]]; then
    fail_case 'origin main is not the worktree HEAD' "${fx}/out"
  elif [[ "$(git -C "${wt}" rev-parse "${sha}^")" != "${old_main}" ]]; then
    fail_case 'the landing is not one commit on the old main' "${fx}/out"
  elif [[ -n "$(git -C "${wt}" status --porcelain)" ]]; then
    fail_case 'the worktree is not clean after landing' "${fx}/out"
  elif ! grep -qx "Local-Verify-Gates: ${want_gates}" <<<"${message}" || ! grep -qx 'Fast-Lane: on' <<<"${message}" ||
    ! grep -qx "Local-Verify-Platform: ${platform}" <<<"${message}"; then
    fail_case "trailers missing or wrong: ${message}" "${fx}/out"
  elif grep -Fq "$(hostname -s 2>/dev/null || hostname)" <<<"${message}" ||
    grep -Fq "$(hostname -s 2>/dev/null || hostname)" "${FAKE_GH_DIR}/status-${sha}"; then
    fail_case 'the host name was published in the commit or the status' "${fx}/out"
  elif ! grep -qx 'state=success' "${FAKE_GH_DIR}/status-${sha}" || ! grep -qx 'context=local-verify' "${FAKE_GH_DIR}/status-${sha}"; then
    fail_case 'no local-verify=success status on the landed SHA' "${fx}/out"
  elif [[ "$(git --git-dir "${origin}" rev-parse refs/heads/feat/widget)" != "${sha}" ]]; then
    fail_case 'the work branch was not pushed at the landed SHA' "${fx}/out"
  elif [[ -n "$(git --git-dir "${origin}" tag --list 'v0.1.1')" ]] || grep -Eq '^workflow run|^run ' "${FAKE_GH_DIR}/calls"; then
    fail_case 'ship tagged or started a release' "${fx}/out"
  elif ! git -C "${wt}" diff --quiet "${old_main}" "${sha}" -- CHANGELOG.md; then
    fail_case 'ship edited the CHANGELOG' "${fx}/out"
  else
    return 0
  fi
  return 1
}

case_name='ship: lands the tested tree, attested, without a tag'
new_fixture
old_main="$(origin_main)"
run_subject "${wt}" ship
if [[ "${code}" != 0 ]]; then
  fail_case "exit ${code}" "${fx}/out"
elif check_landed "${old_main}" 'guard lint test-tagged test-podman build'; then
  if [[ -f "${FAKE_GH_DIR}/contracts-ran" ]]; then
    fail_case 'the contract tests ran without a release-path change' "${fx}/out"
  elif expect_output 'the daily release train (make release) publishes it'; then
    pass_case
  fi
fi

case_name='ship: the open pull request names the commit and gets the gate lines'
new_fixture
printf '{"number":7,"title":"feat(cli): the widget command","body":"Adds it.","state":"OPEN","url":"u"}\n' >"${FAKE_GH_DIR}/pr.json"
run_subject "${wt}" ship
if [[ "${code}" != 0 ]]; then
  fail_case "exit ${code}" "${fx}/out"
elif [[ "$(git --git-dir "${origin}" log -1 --format=%s main)" != 'feat(cli): the widget command' ]]; then
  fail_case 'the landed commit does not carry the PR title' "${fx}/out"
elif ! grep -q '^pr comment 7 ' "${FAKE_GH_DIR}/calls"; then
  fail_case 'no comment on the PR' "${FAKE_GH_DIR}/calls"
else
  pass_case
fi

# B1: the guard gate fails when any of its steps fails.
case_name='ship: a leak in a changed file turns the guard gate red'
new_fixture
printf 'tracked in %s\n' "${tracker_id}" >"${wt}/notes.txt"
git -C "${wt}" add notes.txt
git -C "${wt}" commit --quiet --amend --no-edit
old_main="$(origin_main)"
run_subject "${wt}" ship
if [[ "${code}" != 1 ]] || ! grep -Fq 'gate guard is red; nothing was committed or pushed' "${fx}/out"; then
  fail_case "exit ${code}, want 1 with a red guard gate" "${fx}/out"
elif [[ "$(origin_main)" != "${old_main}" ]] || grep -q -- '--method POST' "${FAKE_GH_DIR}/calls"; then
  fail_case 'a leaking change landed or was attested' "${fx}/out"
else
  pass_case
fi

case_name='ship: a red guard self-test turns the guard gate red'
new_fixture
export FAKE_GUARD_SELFTEST_EXIT=1
old_main="$(origin_main)"
run_subject "${wt}" ship
unset FAKE_GUARD_SELFTEST_EXIT
if [[ "${code}" != 1 ]] || ! grep -Fq 'gate guard is red' "${fx}/out" || [[ "$(origin_main)" != "${old_main}" ]]; then
  fail_case "exit ${code}, want 1 with a red guard gate and main unchanged" "${fx}/out"
else
  pass_case
fi

case_name='ship: a drifted guard copy turns the guard gate red'
new_fixture
export FAKE_VENDOR_DRIFT_EXIT=1
old_main="$(origin_main)"
run_subject "${wt}" ship
unset FAKE_VENDOR_DRIFT_EXIT
if [[ "${code}" != 1 ]] || ! grep -Fq 'gate guard is red' "${fx}/out" || [[ "$(origin_main)" != "${old_main}" ]]; then
  fail_case "exit ${code}, want 1 with a red guard gate and main unchanged" "${fx}/out"
else
  pass_case
fi

# B3: a release-path change runs the release contract tests as a gate.
add_release_path_change() {
  printf '#!/usr/bin/env bash\necho tool\n' >"${wt}/scripts/tool.sh"
  git -C "${wt}" add scripts/tool.sh
  git -C "${wt}" commit --quiet --amend --no-edit
}
case_name='ship: a release-path change runs the contract tests'
new_fixture
add_release_path_change
old_main="$(origin_main)"
run_subject "${wt}" ship
if [[ "${code}" != 0 ]]; then
  fail_case "exit ${code}" "${fx}/out"
elif [[ ! -f "${FAKE_GH_DIR}/contracts-ran" ]]; then
  fail_case 'the contract tests did not run' "${fx}/out"
elif check_landed "${old_main}" 'guard release-contracts lint test-tagged test-podman build'; then
  pass_case
fi

case_name='ship: red contract tests stop the landing'
new_fixture
add_release_path_change
export FAKE_CONTRACTS_EXIT=1
old_main="$(origin_main)"
run_subject "${wt}" ship
unset FAKE_CONTRACTS_EXIT
if [[ "${code}" != 1 ]] || ! grep -Fq 'gate release-contracts is red' "${fx}/out" || [[ "$(origin_main)" != "${old_main}" ]]; then
  fail_case "exit ${code}, want 1 with red contract tests and main unchanged" "${fx}/out"
else
  pass_case
fi

case_name='ship: a red gate lands nothing'
new_fixture
before="$(snapshot)"
export FAKE_LINT_FAIL=1
run_subject "${wt}" ship
unset FAKE_LINT_FAIL
if [[ "${code}" != 1 ]] || ! grep -Fq 'gate lint is red; nothing was committed or pushed' "${fx}/out"; then
  fail_case "exit ${code}, want 1 with a red lint gate" "${fx}/out"
else
  assert_unchanged "${before}" && pass_case
fi

case_name='ship: main moving during the gates stops the landing'
new_fixture
old_main="$(origin_main)"
export FAKE_MOVE_MAIN="${origin}"
run_subject "${wt}" ship
unset FAKE_MOVE_MAIN
now_main="$(origin_main)"
if [[ "${code}" != 1 ]] || ! grep -Fq 'main moved while the gates ran' "${fx}/out"; then
  fail_case "exit ${code}, want 1 because main moved" "${fx}/out"
elif [[ "${now_main}" == "${old_main}" || "${now_main}" == "$(git -C "${wt}" rev-parse HEAD)" ]]; then
  fail_case 'main should hold only the concurrent landing' "${fx}/out"
else
  pass_case
fi

# The switch is read again right before main moves.
case_name='ship: turning the switch off during the gates stops the landing'
new_fixture
old_main="$(origin_main)"
export FAKE_SWITCH_OFF_DURING_GATES=1
run_subject "${wt}" ship
unset FAKE_SWITCH_OFF_DURING_GATES
if [[ "${code}" != 1 ]] || ! grep -Fq "FAST_LANE was turned off before the fast-forward of main (the organization FAST_LANE variable is 'off'" "${fx}/out"; then
  fail_case "exit ${code}, want 1 because the switch went off" "${fx}/out"
elif [[ "$(origin_main)" != "${old_main}" ]]; then
  fail_case 'main moved after the switch went off' "${fx}/out"
else
  pass_case
fi

# --- release: nothing to release -------------------------------------------------

case_name='release: nothing since the latest tag'
new_fixture
tag_origin v0.1.1 main
before="$(snapshot)"
run_subject "${primary}" release
if [[ "${code}" != 0 ]] || ! grep -Fq "nothing to release: origin/main $(origin_main | cut -c1-12) is v0.1.1, and its release, publishers and cask are complete" "${fx}/out"; then
  fail_case "exit ${code}, want 0 with nothing to release" "${fx}/out"
elif [[ "$(grep -c '^run watch 10[123] ' "${FAKE_GH_DIR}/calls")" != 3 ]]; then
  fail_case 'the latest release was not checked before nothing to release' "${FAKE_GH_DIR}/calls"
else
  assert_unchanged "${before}" && pass_case
fi

case_name='release: only docs since the latest tag'
new_fixture
tag_origin v0.1.1 main
push_main 'docs: fix a typo'
before="$(snapshot)"
run_subject "${primary}" release
if [[ "${code}" != 0 ]] || ! grep -Fq 'nothing to release: the 1 commit(s) since v0.1.1 change only docs, tests or CI' "${fx}/out"; then
  fail_case "exit ${code}, want 0 with nothing to release" "${fx}/out"
else
  assert_unchanged "${before}" && pass_case
fi

# --- release: refusals -----------------------------------------------------------

expect_refusal release 'switch off' 3 "the organization FAST_LANE variable is 'off', not 'on'" switch_off
expect_refusal release 'a repository variable exists' 3 "a repository-level FAST_LANE variable exists (value 'on')" switch_both_on
expect_refusal release 'not an admin' 3 'is not an admin' not_admin
expect_refusal release "scripts that are not main's" 3 "the release train runs main's own scripts" stale_scripts
expect_refusal release "a running script that is not main's" 3 "differs from origin/main's scripts/fast-lane.sh" stale_self
expect_refusal release "another lane's local tag" 3 'a local-only v0.1.1 tag exists that the release train did not make' foreign_local_tag
expect_refusal release 'version leap' 3 'is not the smallest increment from v0.1.0' none --version v0.3.0
expect_refusal release 'release exists' 3 'RenseiAI/donmai already has a v0.1.1 release' release_exists
expect_refusal release 'pins set, local signing' 3 'use --tagger central' pins_set --tagger local
expect_refusal release 'pins set, no central tagger configured' 3 'the tagging-identity pins are set but RELEASE_TAGGER_REPOSITORY is not' pins_set
expect_refusal release 'central tagger not configured' 3 'set the RELEASE_TAGGER_REPOSITORY Actions variable' none --tagger central
expect_refusal release 'central tagger malformed' 3 "RELEASE_TAGGER_REPOSITORY must be owner/repository, got 'not a repository'" tagger_malformed --tagger central
expect_refusal release 'central tagger missing' 3 'does not exist yet; use --tagger local' tagger_configured --tagger central
expect_refusal release 'central tagger not dispatchable' 3 'the gh login cannot dispatch the central tagging workflow' tagger_read_only --tagger central
expect_refusal release 'signing key not ready' 3 'the local signing key is not ready' key_not_ready
expect_refusal release 'no tag-creation right' 3 'the gh login cannot create release tags' no_tag_right
expect_refusal release 'brand word in the Unreleased entries' 3 'guard-b blocked the changelog section' brand_unreleased
expect_refusal release 'tracker ID in a release-note subject' 3 'guard-b blocked the release note subjects' tracker_subject
expect_refusal release 'a ship option' 2 '--title is a make ship option' none --title x
expect_refusal release 'malformed version' 2 '--version must be vX.Y.Z' none --version 0.1.1
expect_refusal release 'unknown tagger' 2 '--tagger must be auto, local or central' none --tagger host

# --- release: runs ---------------------------------------------------------------

case_name='release: dry run composes and changes nothing'
new_fixture
before="$(snapshot)"
run_subject "${primary}" release --dry-run
if [[ "${code}" != 0 ]]; then
  fail_case "exit ${code}" "${fx}/out"
elif expect_output 'CHANGELOG v0.1.1 section: composed' '| - Keep the stable copy on restart' \
  'would check out origin/main' 'would run the gates: guard, lint, test-tagged, test-podman, build' \
  'then sign and push tag v0.1.1 at that SHA' 'would watch release.yml worker-image.yml e2b-template.yml' \
  'dry run complete: nothing was changed'; then
  if grep -Eq '^fast-lane release: \|.*(\(#[0-9]+\)|No unreleased changes)' "${fx}/out"; then
    fail_case 'the composed section kept a PR number or the placeholder' "${fx}/out"
  elif grep -qv '^fast-lane release: ' "${fx}/out"; then
    # Every line carries the label, so a caller can anchor on it: a section
    # entry that quotes "nothing to release" can never read as the verdict.
    fail_case 'an output line lacks the program label' "${fx}/out"
  else
    assert_unchanged "${before}" && pass_case
  fi
fi

# check_released <old-main> <tagger>: origin main is the release preparation on
# the old main, attested, tagged and watched.
check_released() {
  local old_main=$1 sha changelog
  sha="$(origin_main)"
  changelog="$(git --git-dir "${origin}" show "${sha}:CHANGELOG.md")"
  if [[ "$(git --git-dir "${origin}" rev-parse "${sha}^")" != "${old_main}" ]]; then
    fail_case 'the release preparation is not one commit on the old main' "${fx}/out"
  elif [[ "$(git --git-dir "${origin}" log -1 --format=%s "${sha}")" != 'chore(release): prepare v0.1.1' ]]; then
    fail_case 'the release preparation has the wrong title' "${fx}/out"
  elif [[ "$(git --git-dir "${origin}" diff --name-only "${old_main}" "${sha}")" != CHANGELOG.md ]]; then
    fail_case 'the release preparation changed more than CHANGELOG.md' "${fx}/out"
  elif [[ "$(awk '/^## \[Unreleased\]/ { f = 1 } f && /^## v0\.1\.0/ { exit } f { print }' <<<"${changelog}")" != \
    "$(printf '## [Unreleased]\n\nNo unreleased changes.\n\n## v0.1.1 — %s\n\n### Fixes\n\n- Keep the stable copy on restart\n' "$(date -u +%Y-%m-%d)")" ]]; then
    fail_case "the CHANGELOG section is wrong: ${changelog}" "${fx}/out"
  elif ! grep -qx 'state=success' "${FAKE_GH_DIR}/status-${sha}"; then
    fail_case 'no local-verify=success status on the release SHA' "${fx}/out"
  elif [[ "$(origin_tag v0.1.1)" != "${sha}" ]]; then
    fail_case 'tag v0.1.1 does not point at the release SHA' "${fx}/out"
  elif [[ -n "$(git --git-dir "${origin}" for-each-ref 'refs/heads/release-train/*')" ]]; then
    fail_case 'the release-train branch was left behind' "${fx}/out"
  elif [[ "$(git -C "${primary}" worktree list --porcelain | grep -c '^worktree ')" != 2 ]]; then
    fail_case 'the scratch worktree was left behind' "${fx}/out"
  else
    return 0
  fi
  return 1
}

case_name='release: lands, tags, and watches every publisher'
new_fixture
old_main="$(origin_main)"
run_subject "${primary}" release
if [[ "${code}" != 0 ]]; then
  fail_case "exit ${code}" "${fx}/out"
elif check_released "${old_main}"; then
  if ! grep -q -- '-----BEGIN SSH SIGNATURE-----' <<<"$(git --git-dir "${origin}" cat-file tag v0.1.1)"; then
    fail_case 'tag v0.1.1 is not SSH-signed' "${fx}/out"
  elif [[ "$(grep -c '^run watch 10[123] ' "${FAKE_GH_DIR}/calls")" != 3 ]]; then
    fail_case 'not every publisher run was watched' "${FAKE_GH_DIR}/calls"
  elif expect_output 'Casks/donmai.rb is at v0.1.1' 'released v0.1.1 at '; then
    pass_case
  fi
fi

case_name="release: releases origin/main, not the caller's branch"
new_fixture
old_main="$(origin_main)"
run_subject "${wt}" release --no-watch
if [[ "${code}" != 0 ]]; then
  fail_case "exit ${code}" "${fx}/out"
elif git --git-dir "${origin}" cat-file -e "$(origin_main):widget.txt" 2>/dev/null; then
  fail_case "the release carried the caller's unlanded work" "${fx}/out"
elif grep -q '^run ' "${FAKE_GH_DIR}/calls"; then
  fail_case '--no-watch still watched' "${FAKE_GH_DIR}/calls"
elif check_released "${old_main}"; then
  pass_case
fi

case_name='release: the Unreleased entries become the section, the placeholder stays behind'
new_fixture
sed -i.bak 's/^No unreleased changes\.$/### Fixes\
\
- The widget no longer flickers./' "${primary}/CHANGELOG.md"
rm -f "${primary}/CHANGELOG.md.bak"
git -C "${primary}" commit --quiet -am 'fix(ui): stop the flicker'
git -C "${primary}" push --quiet origin main
run_subject "${primary}" release --no-watch
changelog="$(git --git-dir "${origin}" show "$(origin_main):CHANGELOG.md")"
if [[ "${code}" != 0 ]]; then
  fail_case "exit ${code}" "${fx}/out"
elif [[ "$(awk '/^## \[Unreleased\]/ { f = 1 } f && /^## v0\.1\.0/ { exit } f { print }' <<<"${changelog}")" != \
  "$(printf '## [Unreleased]\n\nNo unreleased changes.\n\n## v0.1.1 — %s\n\n### Fixes\n\n- The widget no longer flickers.\n' "$(date -u +%Y-%m-%d)")" ]]; then
  fail_case "the Unreleased entries were not moved into v0.1.1: ${changelog}" "${fx}/out"
elif expect_output 'CHANGELOG v0.1.1 section: unreleased'; then
  pass_case
fi

# A failed tag push leaves no local tag, and the re-run releases main as it is.
case_name='release: a failed tag push cleans up, and the re-run finishes the release'
new_fixture
old_main="$(origin_main)"
: >"${FAKE_GH_DIR}/reject-tag-push"
run_subject "${primary}" release --no-watch
landed="$(origin_main)"
if [[ "${code}" != 1 ]] || ! grep -Fq 'FAILED: pushing tag v0.1.1 failed' "${fx}/out" ||
  ! grep -Fq 'removed the local tag so the next run can retry' "${fx}/out"; then
  fail_case "exit ${code}, want 1 with a failed tag push" "${fx}/out"
elif [[ -n "$(git -C "${primary}" tag --list v0.1.1)" || -e "${primary}/.git/fast-lane/local-tag-v0.1.1" ]]; then
  fail_case 'the failed tag push left a local tag or its marker' "${fx}/out"
elif [[ "${landed}" == "${old_main}" ]]; then
  fail_case 'the release preparation did not land before the tag step' "${fx}/out"
else
  rm -f "${FAKE_GH_DIR}/reject-tag-push"
  run_subject "${primary}" release --no-watch
  if [[ "${code}" != 0 ]]; then
    fail_case "re-run exit ${code}" "${fx}/out"
  elif [[ "$(origin_main)" != "${landed}" ]]; then
    fail_case 'the re-run landed a second release preparation' "${fx}/out"
  elif [[ "$(origin_tag v0.1.1)" != "${landed}" ]]; then
    fail_case 'the re-run did not tag the landed release preparation' "${fx}/out"
  elif expect_output 'already carries the v0.1.1 section; releasing it as it is'; then
    pass_case
  fi
fi

case_name='release: a local-only tag left by a killed run is replaced'
new_fixture
git -C "${primary}" tag -a v0.1.1 -m v0.1.1 v0.1.0
mkdir -p "${primary}/.git/fast-lane"
git -C "${primary}" rev-parse refs/tags/v0.1.1 >"${primary}/.git/fast-lane/local-tag-v0.1.1"
run_subject "${primary}" release --no-watch
if [[ "${code}" != 0 ]]; then
  fail_case "exit ${code}" "${fx}/out"
elif [[ "$(origin_tag v0.1.1)" != "$(origin_main)" ]]; then
  fail_case 'origin v0.1.1 does not point at the release' "${fx}/out"
elif [[ -e "${primary}/.git/fast-lane/local-tag-v0.1.1" ]]; then
  fail_case 'the local-tag marker outlived the pushed tag' "${fx}/out"
elif expect_output 'removed a local-only v0.1.1 tag an earlier run made'; then
  pass_case
fi

case_name='release: turning the switch off after main moved stops before the tag'
new_fixture
: >"${FAKE_GH_DIR}/switch-off-after-main"
run_subject "${primary}" release
if [[ "${code}" != 1 ]] || ! grep -Fq 'FAST_LANE was turned off before tagging v0.1.1' "${fx}/out"; then
  fail_case "exit ${code}, want 1 because the switch went off" "${fx}/out"
elif [[ -n "$(origin_tag v0.1.1)" ]]; then
  fail_case 'a tag was created after the switch went off' "${fx}/out"
else
  pass_case
fi

case_name='release: turning the switch off before main moves stops the landing'
new_fixture
old_main="$(origin_main)"
: >"${FAKE_GH_DIR}/switch-off-after-status"
run_subject "${primary}" release
if [[ "${code}" != 1 ]] || ! grep -Fq 'FAST_LANE was turned off before the fast-forward of main' "${fx}/out"; then
  fail_case "exit ${code}, want 1 because the switch went off" "${fx}/out"
elif [[ "$(origin_main)" != "${old_main}" || -n "$(origin_tag v0.1.1)" ]]; then
  fail_case 'main moved or a tag appeared after the switch went off' "${fx}/out"
else
  pass_case
fi

case_name='release: a version taken during the run stops before the tag'
new_fixture
: >"${FAKE_GH_DIR}/tag-elsewhere-after-main"
run_subject "${primary}" release
if [[ "${code}" != 1 ]] || ! grep -Fq 'tag v0.1.1 appeared on origin' "${fx}/out" || ! grep -Fq 'before tagging v0.1.1' "${fx}/out"; then
  fail_case "exit ${code}, want 1 because v0.1.1 was taken" "${fx}/out"
elif [[ -n "$(git -C "${primary}" tag --list v0.1.1)" ]]; then
  fail_case 'a local tag was created for a taken version' "${fx}/out"
else
  pass_case
fi

case_name='release: the central tagger tags the landed SHA'
new_fixture
tagger_configured
: >"${FAKE_GH_DIR}/tagger-exists"
old_main="$(origin_main)"
run_subject "${primary}" release --tagger central
if [[ "${code}" != 0 ]]; then
  fail_case "exit ${code}" "${fx}/out"
elif ! grep -Fxq "workflow run tag-release.yml -R example/tagger -f repository=donmai -f sha=$(origin_main) -f version=v0.1.1" "${FAKE_GH_DIR}/calls"; then
  fail_case 'the central tagger was not dispatched for the landed SHA' "${FAKE_GH_DIR}/calls"
elif [[ -n "$(git -C "${primary}" tag --list v0.1.1)" ]]; then
  fail_case 'the central path created a local tag' "${fx}/out"
elif check_released "${old_main}"; then
  pass_case
fi

case_name='release: a central tag on another SHA fails at once'
new_fixture
tagger_configured
: >"${FAKE_GH_DIR}/tagger-exists"
origin_main >"${FAKE_GH_DIR}/tagger-sha"
export FAST_LANE_TAG_WAIT_SECONDS=20
run_subject "${primary}" release --tagger central
unset FAST_LANE_TAG_WAIT_SECONDS
if [[ "${code}" != 1 ]] || ! grep -Fq "tag v0.1.1 appeared at $(cut -c1-12 "${FAKE_GH_DIR}/tagger-sha"), not " "${fx}/out"; then
  fail_case "exit ${code}, want 1 at once for a tag on another SHA" "${fx}/out"
else
  pass_case
fi

case_name='release: a tag GitHub cannot verify fails the release'
new_fixture
: >"${FAKE_GH_DIR}/tag-unverified"
run_subject "${primary}" release
if [[ "${code}" != 1 ]] || ! grep -Fq 'GitHub does not verify the v0.1.1 signature (false unknown_key)' "${fx}/out"; then
  fail_case "exit ${code}, want 1 with an unverified tag" "${fx}/out"
else
  pass_case
fi

case_name='release: a failed worker-image run fails the release'
new_fixture
: >"${FAKE_GH_DIR}/run-fail-102"
run_subject "${primary}" release
if [[ "${code}" != 1 ]] || ! grep -Fq 'FAILED: worker-image.yml run 102 for v0.1.1 did not succeed' "${fx}/out"; then
  fail_case "exit ${code}, want 1 with a failed worker-image run" "${fx}/out"
else
  pass_case
fi

case_name='release: a gh failure while watching prints a FAILED line'
new_fixture
: >"${FAKE_GH_DIR}/run-list.fail"
run_subject "${primary}" release
if [[ "${code}" != 1 ]] || ! grep -Fq 'FAILED: could not list the release.yml runs: HTTP 502: Bad Gateway' "${fx}/out"; then
  fail_case "exit ${code}, want 1 with a FAILED line naming gh's error" "${fx}/out"
else
  pass_case
fi

case_name='release: main moving during the gates stops the landing'
new_fixture
old_main="$(origin_main)"
export FAKE_MOVE_MAIN="${origin}"
run_subject "${primary}" release
unset FAKE_MOVE_MAIN
if [[ "${code}" != 1 ]] || ! grep -Fq 'main moved while the gates ran; run make release again' "${fx}/out"; then
  fail_case "exit ${code}, want 1 because main moved" "${fx}/out"
elif [[ -n "$(origin_tag v0.1.1)" || "$(git --git-dir "${origin}" rev-parse main^)" != "${old_main}" ]]; then
  fail_case 'main should hold only the concurrent landing, with no tag' "${fx}/out"
else
  pass_case
fi

# --- release: no new work, but the latest release did not finish ---------------

# expect_incomplete <name> <want> <setup>: v0.1.1 is tagged at main, so there
# is nothing new; the setup leaves part of its release unfinished. The run
# must fail with <want>, never say "nothing to release", and change nothing.
expect_incomplete() {
  local name=$1 want=$2 setup=$3 before
  case_name="release: nothing new, ${name}"
  new_fixture
  tag_origin v0.1.1 main
  "${setup}"
  before="$(snapshot)"
  run_subject "${primary}" release
  if [[ "${code}" != 1 ]] || ! grep -Fq -- "FAILED: ${want}" "${fx}/out"; then
    fail_case "exit ${code}, want 1 with: FAILED: ${want}" "${fx}/out"
  elif grep -Fq 'nothing to release' "${fx}/out"; then
    fail_case 'an unfinished release read as nothing to release' "${fx}/out"
  elif ! grep -Fq 'checking that the v0.1.1 release completed' "${fx}/out"; then
    fail_case 'the latest release was not checked' "${fx}/out"
  else
    assert_unchanged "${before}" && pass_case
  fi
}
release_missing() { : >"${FAKE_GH_DIR}/release-missing"; }
release_draft() { : >"${FAKE_GH_DIR}/release-draft"; }
release_run_failed() { : >"${FAKE_GH_DIR}/run-fail-101"; }
image_run_failed() { : >"${FAKE_GH_DIR}/run-fail-102"; }
template_run_missing() { : >"${FAKE_GH_DIR}/runs-missing-e2b-template.yml"; }
cask_behind() { printf '0.1.0\n' >"${FAKE_GH_DIR}/cask-version"; }
expect_incomplete 'the GitHub release is missing' 'the RenseiAI/donmai v0.1.1 release is missing' release_missing
expect_incomplete 'the GitHub release is a draft' 'the RenseiAI/donmai v0.1.1 release is still a draft' release_draft
expect_incomplete 'the release run failed' 'release.yml run 101 for v0.1.1 did not succeed' release_run_failed
expect_incomplete 'the worker-image run failed' 'worker-image.yml run 102 for v0.1.1 did not succeed' image_run_failed
expect_incomplete 'no e2b-template run' 'no e2b-template.yml run appeared for v0.1.1' template_run_missing
expect_incomplete 'the cask is behind' 'RenseiAI/homebrew-tap/Casks/donmai.rb did not move to v0.1.1' cask_behind

case_name='release: only docs since an unfinished release still fails'
new_fixture
tag_origin v0.1.1 main
push_main 'docs: fix a typo'
: >"${FAKE_GH_DIR}/release-draft"
before="$(snapshot)"
run_subject "${primary}" release
if [[ "${code}" != 1 ]] || ! grep -Fq 'FAILED: the RenseiAI/donmai v0.1.1 release is still a draft' "${fx}/out" ||
  grep -Fq 'nothing to release' "${fx}/out"; then
  fail_case "exit ${code}, want 1 with the draft release" "${fx}/out"
else
  assert_unchanged "${before}" && pass_case
fi

# The whole story: the watch fails after the tag, the operator re-runs the
# failed jobs, and the next train finishes that release without a new one.
case_name='release: a re-run resumes a release whose publisher failed after the tag'
new_fixture
: >"${FAKE_GH_DIR}/run-fail-103"
run_subject "${primary}" release
released="$(origin_main)"
if [[ "${code}" != 1 ]] || ! grep -Fq 'FAILED: e2b-template.yml run 103 for v0.1.1 did not succeed' "${fx}/out" ||
  ! grep -Fq 'gh run rerun 103 --failed' "${fx}/out"; then
  fail_case "exit ${code}, want 1 with the failed e2b-template run" "${fx}/out"
elif [[ "$(origin_tag v0.1.1)" != "${released}" ]]; then
  fail_case 'the first run did not tag the release' "${fx}/out"
else
  run_subject "${primary}" release
  if [[ "${code}" != 1 ]] || grep -Fq 'nothing to release' "${fx}/out"; then
    fail_case "still failing: exit ${code}, want 1 without nothing to release" "${fx}/out"
  else
    rm -f "${FAKE_GH_DIR}/run-fail-103"
    run_subject "${primary}" release
    if [[ "${code}" != 0 ]] || ! grep -Fq 'v0.1.1, and its release, publishers and cask are complete' "${fx}/out"; then
      fail_case "after the re-run jobs: exit ${code}, want 0 with a complete v0.1.1" "${fx}/out"
    elif [[ "$(origin_main)" != "${released}" || -n "$(origin_tag v0.1.2)" ]]; then
      fail_case 'the resumed run landed or tagged something new' "${fx}/out"
    else
      pass_case
    fi
  fi
fi

# A release preparation already on main keeps its version on the re-run.
case_name='release: a re-run keeps the version of the release preparation on main'
new_fixture
: >"${FAKE_GH_DIR}/reject-tag-push"
run_subject "${primary}" release --version v0.2.0 --no-watch
landed="$(origin_main)"
rm -f "${FAKE_GH_DIR}/reject-tag-push"
if [[ "${code}" != 1 || "$(git --git-dir "${origin}" log -1 --format=%s main)" != 'chore(release): prepare v0.2.0' ]]; then
  fail_case "first run exit ${code}, want 1 after landing the v0.2.0 preparation" "${fx}/out"
else
  run_subject "${primary}" release --no-watch
  if [[ "${code}" != 0 ]]; then
    fail_case "re-run exit ${code}" "${fx}/out"
  elif [[ "$(origin_tag v0.2.0)" != "${landed}" || -n "$(origin_tag v0.1.1)" ]]; then
    fail_case 'the re-run did not tag the prepared v0.2.0' "${fx}/out"
  elif [[ "$(origin_main)" != "${landed}" ]]; then
    fail_case 'the re-run landed a second preparation' "${fx}/out"
  else
    pass_case
  fi
fi

# --- source ----------------------------------------------------------------------

# This repository is public: the script may name only the public repositories
# it releases to. The central tagger is read from an Actions variable.
case_name='the script names only public repositories'
named="$(grep -oE 'RenseiAI/[A-Za-z0-9_.-]+' "${subject}" | sort -u | tr '\n' ' ')"
if [[ "${named}" != 'RenseiAI/donmai RenseiAI/homebrew-tap ' ]]; then
  printf '%s\n' "${named}" >"${temp_dir}/named.out"
  fail_case 'fast-lane.sh names a repository other than the public release targets' "${temp_dir}/named.out"
else
  pass_case
fi

# --- make -------------------------------------------------------------------------

case_name='make ship and make release pass only command-line inputs, shell-quoted'
make_ship="$(make -s -n -C "${root_dir}" ship TITLE='feat: widget' FULL=1 DRY_RUN=1)"
make_release="$(make -s -n -C "${root_dir}" release VERSION=v1.2.3 TAGGER=central FULL=1 DRY_RUN=1 NO_WATCH=1)"
title_with_quotes="feat: a \"quoted\" \`title\`"
quoted="$(make -s -C "${root_dir}" ship FAST_LANE_SH="printf '[%s]\\n'" TITLE="${title_with_quotes}")"
if [[ "${make_ship}" != "./scripts/fast-lane.sh ship --title \"\$TITLE\" --full --dry-run" ]]; then
  printf '%s\n' "${make_ship}" >"${temp_dir}/make.out"
  fail_case 'make ship did not pass the inputs through' "${temp_dir}/make.out"
elif [[ "${make_release}" != "./scripts/fast-lane.sh release --version \"\$VERSION\" --tagger \"\$TAGGER\" --full --dry-run --no-watch" ]]; then
  printf '%s\n' "${make_release}" >"${temp_dir}/make.out"
  fail_case 'make release did not pass the inputs through' "${temp_dir}/make.out"
elif [[ "$(make -s -n -C "${root_dir}" ship | sed 's/[[:space:]]*$//')" != './scripts/fast-lane.sh ship' ||
  "$(make -s -n -C "${root_dir}" release | sed 's/[[:space:]]*$//')" != './scripts/fast-lane.sh release' ]]; then
  fail_case 'a plain make ship or make release passed flags nobody asked for' ''
elif [[ "$(env VERSION=v9.9.9 TAGGER=central NO_WATCH=1 FULL=1 DRY_RUN=1 make -s -n -C "${root_dir}" release | sed 's/[[:space:]]*$//')" != './scripts/fast-lane.sh release' ||
  "$(env TITLE=x FULL=1 DRY_RUN=1 make -s -n -C "${root_dir}" ship | sed 's/[[:space:]]*$//')" != './scripts/fast-lane.sh ship' ]]; then
  fail_case 'make passed values from the environment, not the command line' ''
elif [[ "${quoted}" != "$(printf '[ship]\n[--title]\n[%s]' "${title_with_quotes}")" ]]; then
  printf '%s\n' "${quoted}" >"${temp_dir}/make.out"
  fail_case 'make ship broke a quoted title' "${temp_dir}/make.out"
else
  pass_case
fi

printf 'fast-lane tests: %d passed, %d failed\n' "${passes}" "${failures}"
[[ "${failures}" -eq 0 ]]
