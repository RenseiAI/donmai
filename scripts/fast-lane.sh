#!/usr/bin/env bash
# fast-lane.sh — the fast lane: `make ship` lands work on main, and
# `make release` is the once-a-day release train.
#
# Both refuse unless the fast-lane switch is on: the organization Actions
# variable FAST_LANE is exactly "on" AND no repository variable of that name
# exists. The switch is read again from GitHub right before main moves and
# right before a tag is created.
#
# ship lands this worktree's branch on main as ONE commit that passed the local
# gates. It never tags or publishes anything; the next release train does.
#   1. Preflight, read-only; any failure refuses before anything changes: the
#      switch, the gh login is a repository admin, a linked worktree on a
#      branch (never the primary checkout, never main) whose name passes
#      guard-b, a clean tree, HEAD contains fresh origin/main, and the remote
#      branch holds nothing HEAD lacks.
#   2. Compose the squashed commit message (the open pull request, the single
#      commit, or --title) and run guard-b over it.
#   3. The gates (run_gates below), fast first, stopping at the first red.
#   4. Land: commit the tested tree onto origin/main with Local-Verify-*
#      trailers, push it to the branch, post the local-verify status on that
#      SHA and read it back, comment on the open pull request, read the switch
#      again, and fast-forward main to the SHA (never forced).
#
# release is the release train, run once a day (RELEASING.md "Daily release
# train").
#   1. Preflight, read-only: the switch, admin, and the scripts this run uses
#      (this script included) are origin/main's own. When main has no commits
#      since the latest v* tag, it finishes that tag's release instead: it
#      watches the tag's publisher runs, the GitHub release and the cask, and
#      prints "nothing to release" (exit 0) only once all of them are
#      complete, or FAILED (exit 1). The version is the next patch (or
#      --version, a smallest increment; or the version of a release
#      preparation already on main) and is unused on origin and as a GitHub
#      release. The tag signer is ready and allowed to create release tags.
#   2. Compose the release preparation in a scratch worktree at origin/main:
#      the CHANGELOG section (an existing one, the Unreleased entries, or one
#      composed from the subjects landed since the last tag). Run guard-b over
#      the commit message, the section and those subjects.
#   3. The same gates, in that scratch worktree.
#   4. Land the release-preparation commit through the ship's attested path:
#      push it to release-train/<version>, post and read back local-verify,
#      read the switch and check the version again, fast-forward main. When
#      main already carries the section (a re-run after a later failure), main
#      is released as it is.
#   5. Read the switch and check the version again, then tag that SHA: the
#      transitional operator key signs it until the tagging-identity pins are
#      set, after which the central tagging workflow creates it. GitHub must
#      verify the signature.
#   6. Watch release.yml, worker-image.yml and e2b-template.yml to success,
#      then the published release and the Homebrew cask.
#
# Re-running after any failure is safe and never prompts: nothing lands unless
# every earlier step passed; a failure after the tag is resumed by the next
# run's watch; a failed tag push removes its local tag; and a local-only tag
# this script made in a killed run is replaced (any other one is refused).
#
# Usage: scripts/fast-lane.sh ship [--dry-run] [--title TEXT] [--full]
#        scripts/fast-lane.sh release [--dry-run] [--version vX.Y.Z]
#          [--tagger auto|local|central] [--full] [--no-watch]
# Make:  make ship [TITLE=...] [FULL=1] [DRY_RUN=1]
#        make release [VERSION=vX.Y.Z] [TAGGER=...] [FULL=1] [DRY_RUN=1]
#          [NO_WATCH=1]
#
# Exit codes: 0 landed, released, nothing to do, or a clean dry run;
#             1 a gate or step failed (a FAILED: line says which);
#             2 usage; 3 refused by preflight (a REFUSED: line says why).

set -euo pipefail

readonly repo='RenseiAI/donmai'
readonly tap_repo='RenseiAI/homebrew-tap'
readonly cask_path='Casks/donmai.rb'
readonly status_context='local-verify'
readonly semver_re='^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$'
readonly repository_re='^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$'
readonly default_tagger_workflow='tag-release.yml'
watched_workflows=(release.yml worker-image.yml e2b-template.yml)
# A change under these paths re-runs the release contract tests (CI's
# release-contracts job) as a gate: the release workflows run these scripts.
contract_paths=(scripts .github/workflows .goreleaser.yaml)
# Waits; tests shorten them.
poll_seconds="${FAST_LANE_POLL_SECONDS:-15}"
tag_wait_seconds="${FAST_LANE_TAG_WAIT_SECONDS:-900}"
run_wait_seconds="${FAST_LANE_RUN_WAIT_SECONDS:-300}"
cask_wait_seconds="${FAST_LANE_CASK_WAIT_SECONDS:-600}"

prog='fast-lane'
mode=''
dry_run=false
title=''
version=''
tagger='auto'
full=false
watch=true
reported=false
error_line=''

say() { printf '%s: %s\n' "${prog}" "$*"; }
log() { say "$@"; }
plan() { say "[dry-run] would $*"; }
refuse() {
  reported=true
  printf '%s: REFUSED: %s\n' "${prog}" "$*" >&2
  exit 3
}
fail() {
  reported=true
  printf '%s: FAILED: %s\n' "${prog}" "$*" >&2
  exit 1
}
usage_text() { sed -n '/^# Usage:/,/^# Exit codes/p' "$0" | sed -e '$d' -e 's/^# \{0,1\}//'; }
usage_error() {
  reported=true
  printf '%s: %s\n' "${prog}" "$*" >&2
  usage_text >&2
  exit 2
}

case "${1:-}" in
  ship | release)
    mode=$1
    prog="fast-lane ${mode}"
    shift
    ;;
  -h | --help) usage_text; exit 0 ;;
  *) usage_error "the first argument must be ship or release" ;;
esac
only_for() { [[ "${mode}" == "$1" ]] || usage_error "$2 is a make $1 option"; }
while [[ $# -gt 0 ]]; do
  case "$1" in
    --dry-run) dry_run=true ;;
    --full) full=true ;;
    --title) only_for ship "$1"; [[ $# -ge 2 ]] || usage_error "$1 needs a value"; title=$2; shift ;;
    --version) only_for release "$1"; [[ $# -ge 2 ]] || usage_error "$1 needs a value"; version=$2; shift ;;
    --tagger) only_for release "$1"; [[ $# -ge 2 ]] || usage_error "$1 needs a value"; tagger=$2; shift ;;
    --no-watch) only_for release "$1"; watch=false ;;
    -h | --help) usage_text; exit 0 ;;
    *) usage_error "unknown argument: $1" ;;
  esac
  shift
done
[[ -z "${version}" || "${version}" =~ ${semver_re} ]] || usage_error "--version must be vX.Y.Z, got ${version}"
case "${tagger}" in auto | local | central) ;; *) usage_error "--tagger must be auto, local or central" ;; esac

# Gate `make` calls behave exactly like a developer's plain invocation, not like
# a sub-make of `make release VERSION=...`. Nothing may prompt: this runs
# unattended from a scheduler.
unset MAKEFLAGS MFLAGS MAKELEVEL
export GOWORK=off GIT_TERMINAL_PROMPT=0 GH_PROMPT_DISABLED=1 GH_NO_UPDATE_NOTIFIER=1

started=${SECONDS}
scratch="$(mktemp -d)"
root=''
release_tree=''

cleanup() {
  local status=$?
  set +e
  if [[ -n "${release_tree}" && -e "${release_tree}" ]]; then
    git -C "${root}" worktree remove --force "${release_tree}" >/dev/null 2>&1 ||
      printf '%s: warning: could not remove the scratch worktree %s\n' "${prog}" "${release_tree}" >&2
  fi
  # Every non-zero exit carries a FAILED or REFUSED line, even an unexpected one.
  if [[ ${status} -ne 0 && "${reported}" == false ]]; then
    printf '%s: FAILED: stopped unexpectedly (exit %s%s)\n' "${prog}" "${status}" "${error_line:+ at line ${error_line}}" >&2
    status=1
  fi
  rm -rf -- "${scratch}"
  exit "${status}"
}
trap cleanup EXIT
trap 'error_line=${LINENO}' ERR
trap 'exit 130' INT TERM

for tool in git gh jq; do
  command -v "${tool}" >/dev/null 2>&1 || refuse "${tool} is required"
done

root="$(git rev-parse --show-toplevel 2>/dev/null)" || refuse "run inside a donmai checkout"
cd "${root}"

# gh_read <what> <gh arguments...>: prints gh's output. On failure it prints a
# FAILED line with gh's own error and returns 1; the caller decides the exit.
gh_read() {
  local what=$1 out
  shift
  if out="$(gh "$@" 2>"${scratch}/gh.err")"; then
    printf '%s\n' "${out}"
    return 0
  fi
  printf '%s: FAILED: could not %s: %s\n' "${prog}" "${what}" "$(tr '\n' ' ' <"${scratch}/gh.err" | cut -c1-300)" >&2
  return 1
}

# gh_lookup <endpoint>: 0 when the resource exists, 1 when GitHub answers 404,
# 2 (after a FAILED line) when it cannot be read at all. Callers fail closed.
gh_lookup() {
  if gh api "$1" >/dev/null 2>"${scratch}/gh.err"; then
    return 0
  fi
  if grep -q 'HTTP 404' "${scratch}/gh.err"; then
    return 1
  fi
  printf '%s: FAILED: could not read %s: %s\n' "${prog}" "$1" "$(tr '\n' ' ' <"${scratch}/gh.err" | cut -c1-300)" >&2
  return 2
}

# watch_release <tag> <commit>: wait for every publisher of <tag> and stop
# with a FAILED line unless all of them succeeded: the release.yml,
# worker-image.yml and e2b-template.yml runs the tag push started, the
# published GitHub release, and the Homebrew cask at that version. A run that
# already finished answers at once, so this also resumes or re-checks an
# earlier release.
watch_release() {
  local tag=$1 commit=$2 workflow runs run_id deadline index=0 release_json cask_json cask
  local run_ids=()
  for workflow in "${watched_workflows[@]}"; do
    deadline=$((SECONDS + run_wait_seconds))
    while :; do
      runs="$(gh_read "list the ${workflow} runs" run list -R "${repo}" --workflow "${workflow}" --limit 50 \
        --json databaseId,headSha,headBranch)" || fail "cannot find the ${workflow} run for ${tag}"
      run_id="$(jq -r --arg sha "${commit}" --arg tag "${tag}" \
        '[.[] | select(.headSha == $sha and .headBranch == $tag)] | first | .databaseId // ""' <<<"${runs}")"
      [[ -z "${run_id}" ]] || break
      [[ ${SECONDS} -lt ${deadline} ]] || fail "no ${workflow} run appeared for ${tag} within ${run_wait_seconds}s"
      sleep "${poll_seconds}"
    done
    run_ids+=("${run_id}")
  done
  for workflow in "${watched_workflows[@]}"; do
    run_id="${run_ids[${index}]}"
    index=$((index + 1))
    log "watching ${workflow} run ${run_id} for ${tag}"
    gh run watch "${run_id}" -R "${repo}" --exit-status --interval 30 >"${scratch}/watch.log" 2>&1 </dev/null ||
      fail "${workflow} run ${run_id} for ${tag} did not succeed ($(tail -1 "${scratch}/watch.log")): https://github.com/${repo}/actions/runs/${run_id}. Re-run its failed jobs (gh run rerun ${run_id} --failed) so the tag-push policy still applies; the next make release checks again"
    log "${workflow} run ${run_id} succeeded"
  done
  release_json="$(gh_read "read the ${tag} release" api "repos/${repo}/releases/tags/${tag}")" ||
    fail "the ${repo} ${tag} release is missing"
  [[ "$(jq -r 'if .draft == false then .tag_name else "" end' <<<"${release_json}")" == "${tag}" ]] ||
    fail "the ${repo} ${tag} release is still a draft"
  log "${repo} ${tag} is published"
  deadline=$((SECONDS + cask_wait_seconds))
  while :; do
    cask_json="$(gh_read "read ${tap_repo}/${cask_path}" api "repos/${tap_repo}/contents/${cask_path}")" ||
      fail "cannot read the Homebrew cask"
    cask="$(jq -r '.content // "" | gsub("\n"; "")' <<<"${cask_json}" | base64 --decode 2>/dev/null)" || cask=''
    if grep -q "version \"${tag#v}\"" <<<"${cask}"; then
      break
    fi
    [[ ${SECONDS} -lt ${deadline} ]] || fail "${tap_repo}/${cask_path} did not move to ${tag} within ${cask_wait_seconds}s"
    sleep "${poll_seconds}"
  done
  log "${tap_repo}/${cask_path} is at ${tag}"
}

# --- 1. Preflight (read-only) ------------------------------------------------

org_variables='{}'
repo_variables='{}'
# load_variables: the Actions variables this repository sees, read fresh.
load_variables() {
  local org_pages repo_pages
  org_pages="$(gh_read 'read the organization Actions variables' api --paginate "repos/${repo}/actions/organization-variables?per_page=30")" || return 1
  repo_pages="$(gh_read 'read the repository Actions variables' api --paginate "repos/${repo}/actions/variables?per_page=30")" || return 1
  org_variables="$(jq -cs '[.[].variables[]?] | map({(.name): .value}) | add // {}' <<<"${org_pages}")" || return 1
  repo_variables="$(jq -cs '[.[].variables[]?] | map({(.name): .value}) | add // {}' <<<"${repo_pages}")" || return 1
}
# var <name>: a variable as the workflows see it (a repository value wins).
var() { jq -rn --argjson org "${org_variables}" --argjson repo "${repo_variables}" --arg name "$1" '($org + $repo)[$name] // ""'; }
# switch_off_reason: prints why the fast lane is off; prints nothing when on.
# On means the organization variable is exactly "on" and no repository
# variable of the same name exists, so no single repository can hold the lane
# open or shadow the organization's decision.
switch_off_reason() {
  if jq -e 'has("FAST_LANE")' >/dev/null <<<"${repo_variables}"; then
    printf "a repository-level FAST_LANE variable exists (value '%s'); only the organization variable may turn the lane on, so delete the repository one" \
      "$(jq -r '.FAST_LANE' <<<"${repo_variables}")"
    return 0
  fi
  local value
  value="$(jq -r '.FAST_LANE // ""' <<<"${org_variables}")"
  [[ "${value}" == on ]] || printf "the organization FAST_LANE variable is '%s', not 'on'" "${value:-unset}"
}
# recheck_switch <step>: read the switch from GitHub again right before <step>.
recheck_switch() {
  local reason
  load_variables || fail "cannot read FAST_LANE again before $1; stopped"
  reason="$(switch_off_reason)"
  [[ -z "${reason}" ]] || fail "FAST_LANE was turned off before $1 (${reason}); stopped"
  log "FAST_LANE is still on before $1"
}

load_variables || refuse "cannot read the Actions variables of ${repo} (is gh logged in?)"
reason="$(switch_off_reason)"
[[ -z "${reason}" ]] ||
  refuse "FAST_LANE is not on: ${reason}. Land through a reviewed pull request and release with the steps in RELEASING.md."
log "FAST_LANE is on"

repo_json="$(gh_read "read ${repo}" api "repos/${repo}")" || refuse "cannot read your permissions on ${repo}"
[[ "$(jq -r '.permissions.admin // false' <<<"${repo_json}")" == true ]] ||
  refuse "the gh login is not an admin of ${repo}; only an admin can fast-forward main"
user_json="$(gh_read 'read the gh login' api user)" || refuse "cannot read the gh login"
log "gh login $(jq -r '.login // "<unknown>"' <<<"${user_json}") is an admin of ${repo}"

git fetch --quiet --tags origin '+refs/heads/main:refs/remotes/origin/main' 2>"${scratch}/fetch.err" ||
  refuse "git fetch origin failed: $(tail -1 "${scratch}/fetch.err")"
base="$(git rev-parse --verify 'refs/remotes/origin/main^{commit}')"

# remote_tag_target: the commit origin's ${version} tag points at, or nothing.
remote_tag_target() {
  local lines
  lines="$(git ls-remote origin "refs/tags/${version}" "refs/tags/${version}^{}")" || return 1
  awk -v ref="refs/tags/${version}" '
    $2 == ref "^{}" { peeled = $1 } $2 == ref { direct = $1 }
    END { print (peeled != "" ? peeled : direct) }
  ' <<<"${lines}"
}
# version_free <step>: stop unless ${version} is still unused on origin and as
# a GitHub release.
version_free() {
  local target rc=0
  target="$(remote_tag_target)" || fail "cannot read the origin tags before $1"
  [[ -z "${target}" ]] || fail "tag ${version} appeared on origin (at ${target:0:12}) before $1; stopped"
  gh_lookup "repos/${repo}/releases/tags/${version}" || rc=$?
  case "${rc}" in
    0) fail "a ${repo} ${version} release appeared before $1; stopped" ;;
    1) ;;
    *) fail "cannot read the ${repo} releases before $1" ;;
  esac
}

common_dir="$(cd "$(git rev-parse --git-common-dir)" && pwd -P)"
if [[ "${mode}" == ship ]]; then
  git_dir="$(cd "$(git rev-parse --absolute-git-dir)" && pwd -P)"
  [[ "${git_dir}" != "${common_dir}" ]] ||
    refuse "this is the primary checkout; run make ship from a linked worktree (scripts/create-worktree.sh <name>)"
  branch="$(git symbolic-ref --quiet --short HEAD)" || refuse "HEAD is detached; run make ship from a branch"
  [[ "${branch}" != "main" ]] || refuse "the worktree is on main; run make ship from a work branch"
  [[ -z "$(git status --porcelain --untracked-files=normal)" ]] ||
    refuse "the worktree has uncommitted or untracked changes; commit or remove them first"
  git merge-base --is-ancestor "${base}" HEAD ||
    refuse "HEAD does not contain origin/main (${base:0:12}); merge origin/main and re-run"
  orig_head="$(git rev-parse HEAD)"
  ahead="$(git rev-list --count "${base}..HEAD")"
  remote_branch="$(git ls-remote origin "refs/heads/${branch}")" || refuse "cannot read origin/${branch}"
  remote_branch="$(awk '{ print $1 }' <<<"${remote_branch}")"
  if [[ -n "${remote_branch}" ]] && ! git merge-base --is-ancestor "${remote_branch}" HEAD 2>/dev/null; then
    refuse "origin/${branch} has commits this worktree does not (${remote_branch:0:12}); pull them first"
  fi
  # The branch name is published by the push below; guard it like CI does.
  printf '%s\n' "${branch}" | bash scripts/guard-b-lint.sh --stdin ship-branch-name >"${scratch}/guard-branch.log" 2>&1 || {
    cat "${scratch}/guard-branch.log" >&2
    refuse "the branch name fails guard-b; rename the branch"
  }
  if [[ "${ahead}" -eq 0 ]]; then
    log "nothing to ship: ${branch} has no commits beyond origin/main ${base:0:12}"
    exit 0
  fi
  contracts_from="${base}"
  contracts_to=HEAD
else
  # The release train runs main's own scripts, so a stale or edited checkout
  # cannot release with an unreviewed copy of this script or its helpers.
  git diff --quiet "${base}" -- scripts ||
    refuse "this checkout's scripts/ differ from origin/main ${base:0:12}; the release train runs main's own scripts. Update the checkout to origin/main and re-run."
  git show "${base}:scripts/fast-lane.sh" 2>/dev/null | cmp -s - "$0" ||
    refuse "the running $0 differs from origin/main's scripts/fast-lane.sh; run the checkout's own copy (make release) at origin/main"

  # The latest release is read from origin, so a stale local-only tag can
  # neither move the baseline nor hide an existing remote one.
  tag_lines="$(git ls-remote --tags origin 'refs/tags/v*')" || refuse "cannot list the origin tags"
  remote_tags="$(awk '$2 !~ /\^\{\}$/ { sub("refs/tags/", "", $2); print $2 }' <<<"${tag_lines}")"
  latest="$(grep -E "${semver_re}" <<<"${remote_tags}" | sed 's/^v//' | sort -t. -k1,1n -k2,2n -k3,3n | tail -1 || true)"
  latest="${latest:+v${latest}}"
  [[ -n "${latest}" ]] || refuse "no vX.Y.Z tag found on origin; the first release is not a release-train release"
  latest_commit="$(awk -v ref="refs/tags/${latest}" '
    $2 == ref "^{}" { peeled = $1 } $2 == ref { direct = $1 }
    END { print (peeled != "" ? peeled : direct) }
  ' <<<"${tag_lines}")"
  git cat-file -e "${latest_commit}^{commit}" 2>/dev/null ||
    refuse "the ${latest} commit ${latest_commit:0:12} is not in this repository"
  pending="$(git rev-list --count "${latest_commit}..${base}")"
  if [[ "${pending}" -eq 0 ]]; then
    # No new work. A run that failed after its tag is finished here, so a
    # failed release never reads as "nothing to release".
    log "no commits on main since ${latest}; checking that the ${latest} release completed"
    watch_release "${latest}" "${latest_commit}"
    log "nothing to release: origin/main ${base:0:12} is ${latest}, and its release, publishers and cask are complete"
    exit 0
  fi
  log "${pending} commit(s) on main since ${latest}"

  IFS=. read -r major minor patch <<<"${latest#v}"
  next_patch="v${major}.${minor}.$((patch + 1))"
  next_minor="v${major}.$((minor + 1)).0"
  next_major="v$((major + 1)).0.0"
  # A release preparation already on main (an earlier run that failed after
  # landing it) names the version; releasing another would duplicate it.
  prepared="$(git log --format=%s "${latest_commit}..${base}" |
    awk '/^chore\(release\): prepare v[0-9]+\.[0-9]+\.[0-9]+( \(#[0-9]+\))?$/ && !found { print $3; found = 1 }')"
  version="${version:-${prepared:-${next_patch}}}"
  case "${version}" in
    "${next_patch}" | "${next_minor}" | "${next_major}") ;;
    *) refuse "${version} is not the smallest increment from ${latest}; use ${next_patch}, ${next_minor} or ${next_major}" ;;
  esac
  ! grep -qx -- "${version}" <<<"${remote_tags}" || refuse "tag ${version} already exists on origin"
  rc=0
  gh_lookup "repos/${repo}/releases/tags/${version}" || rc=$?
  case "${rc}" in
    0) refuse "${repo} already has a ${version} release" ;;
    1) ;;
    *) refuse "cannot read ${repo} releases to prove ${version} is unused" ;;
  esac
  log "version ${version} (latest ${latest})"
  # A local tag this script made records its object here until it is pushed,
  # so a killed run's tag is replaced and nobody else's ever is.
  tag_marker="${common_dir}/fast-lane/local-tag-${version}"
  if local_tag="$(git rev-parse --quiet --verify "refs/tags/${version}")"; then
    [[ -f "${tag_marker}" && "$(cat "${tag_marker}")" == "${local_tag}" ]] ||
      refuse "a local-only ${version} tag exists that the release train did not make; if it is not another lane's, remove it (git tag -d ${version}) and re-run"
    log "a local-only ${version} tag an earlier run made (never pushed) will be replaced"
  fi

  pins_active=false
  if [[ -n "$(var RELEASE_TAG_SIGNERS)" || -n "$(var RELEASE_TAGGER_EMAIL)" || -n "$(var RELEASE_TAG_CREATOR)" ]]; then
    pins_active=true
  fi
  if [[ "${tagger}" == auto ]]; then
    tagger=local
    [[ "${pins_active}" == false ]] || tagger=central
  fi
  if [[ "${tagger}" == local ]]; then
    [[ "${pins_active}" == false ]] ||
      refuse "the tagging-identity pins are set, so an operator-signed tag would be refused by release-authority; use --tagger central"
    [[ -x scripts/verify-release-signing-key.sh ]] || refuse "scripts/verify-release-signing-key.sh is missing"
    scripts/verify-release-signing-key.sh >"${scratch}/signing-key.log" 2>&1 </dev/null ||
      refuse "the local signing key is not ready: $(tail -1 "${scratch}/signing-key.log")"
    log "tag signer: local registered key ($(tail -1 "${scratch}/signing-key.log"))"
    # Creating a v* tag needs the creation ruleset's bypass; the administrator
    # audit proves this login holds it (current_user_can_bypass=always).
    token="${GH_TOKEN:-}"
    [[ -n "${token}" ]] || token="$(gh_read 'read the gh token' auth token)" || refuse "cannot read the gh token to audit tag-creation rights"
    GH_TOKEN="${token}" RELEASE_TAG_CREATOR='' RELEASE_TAGGER_EMAIL='' RELEASE_TAG_SIGNERS='' \
      bash scripts/verify-release-authority.sh --audit-policy "${repo}" >"${scratch}/audit.log" 2>&1 </dev/null || {
      sed 's/^/    /' "${scratch}/audit.log" >&2
      refuse "the gh login cannot create release tags: the tag rulesets must be exact and this login must read current_user_can_bypass=always on the creation ruleset (RELEASING.md \"Tag authority and immutability\")"
    }
    log "tag creation: $(tail -1 "${scratch}/audit.log")"
  else
    # The central tagging workflow is named by Actions variables, never in
    # source: RELEASE_TAGGER_REPOSITORY (owner/repository) and optionally
    # RELEASE_TAGGER_WORKFLOW.
    tagger_repo="$(var RELEASE_TAGGER_REPOSITORY)"
    tagger_workflow="$(var RELEASE_TAGGER_WORKFLOW)"
    tagger_workflow="${tagger_workflow:-${default_tagger_workflow}}"
    if [[ -z "${tagger_repo}" ]]; then
      [[ "${pins_active}" == false ]] ||
        refuse "the tagging-identity pins are set but RELEASE_TAGGER_REPOSITORY is not, so no signer can produce an accepted tag"
      refuse "the central tagging workflow is not configured (set the RELEASE_TAGGER_REPOSITORY Actions variable); use --tagger local until it is"
    fi
    [[ "${tagger_repo}" =~ ${repository_re} ]] ||
      refuse "RELEASE_TAGGER_REPOSITORY must be owner/repository, got '${tagger_repo}'"
    # Dispatching a workflow needs write access to the repository holding it.
    tagger_json="$(gh_read 'read the central tagging repository' api "repos/${tagger_repo}")" ||
      refuse "cannot read the central tagging repository named by RELEASE_TAGGER_REPOSITORY"
    [[ "$(jq -r '.permissions.push // false' <<<"${tagger_json}")" == true ]] ||
      refuse "the gh login cannot dispatch the central tagging workflow; it needs write access to the RELEASE_TAGGER_REPOSITORY repository"
    rc=0
    gh_lookup "repos/${tagger_repo}/actions/workflows/${tagger_workflow}" || rc=$?
    case "${rc}" in
      0) log "tag signer: the central tagging workflow ${tagger_workflow} in RELEASE_TAGGER_REPOSITORY" ;;
      1)
        [[ "${pins_active}" == false ]] ||
          refuse "the central tagging workflow ${tagger_workflow} does not exist, and the tagging-identity pins are set, so no signer can produce an accepted tag"
        refuse "the central tagging workflow ${tagger_workflow} does not exist yet; use --tagger local until it does"
        ;;
      *) refuse "cannot read the central tagging workflow; the central tagger is unavailable" ;;
    esac
  fi
  contracts_from="${latest_commit}"
  contracts_to="${base}"
fi

# Release-path code changed since the scope base: the contract tests gate it.
contract_changes="$(git diff --name-only "${contracts_from}" "${contracts_to}" -- "${contract_paths[@]}")" ||
  refuse "cannot list the release-path changes"
contracts_gate=false
[[ -z "${contract_changes}" ]] || contracts_gate=true

# --- 2. Compose ----------------------------------------------------------------

# guard_text <label> <file>: guard-b over text that has no file in the tree.
guard_text() {
  bash scripts/guard-b-lint.sh --stdin "$1" <"$2" >"${scratch}/guard-$1.log" 2>&1 || {
    cat "${scratch}/guard-$1.log" >&2
    refuse "guard-b blocked the ${1//-/ }; rewrite it brand-neutrally"
  }
}

# sanitize_subject: turn one commit subject into a CHANGELOG line.
sanitize_subject() {
  sed -E \
    -e 's/^\[[^]]*\][[:space:]]*//' \
    -e 's/[[:space:]]*\(#[0-9]+\)//g' \
    -e 's/^[a-z]+(\([^)]*\))?!?:[[:space:]]*//' \
    -e 's/[[:space:]]+$//'
}

# compose_changelog <in> <out>: writes <out> with a "## <version>" section and
# prints where it came from: existing, unreleased, composed, or none when
# nothing since the last tag has a release note.
compose_changelog() {
  local in=$1 out=$2 heading features='' fixes='' chores='' line kind entry
  if grep -Eq "^## ${version//./\\.}([[:space:]]|\$)" "${in}"; then
    cp "${in}" "${out}" || return 1
    echo existing
    return 0
  fi
  grep -q '^## \[Unreleased\]' "${in}" || {
    printf "CHANGELOG.md has no '## [Unreleased]' heading to release under\n" >&2
    return 1
  }
  # The Unreleased entries, without the placeholder line or edge blank lines.
  awk '/^## \[Unreleased\]/ { f = 1; next } f && /^## / { exit } f { print }' "${in}" |
    sed -E '/^No unreleased changes\.?[[:space:]]*$/d' |
    awk '{ line[NR] = $0 } /[^[:space:]]/ { if (!first) first = NR; last = NR }
         END { for (i = first; first && i <= last; i++) print line[i] }' >"${scratch}/section-body.md" || return 1
  if [[ -s "${scratch}/section-body.md" ]]; then
    echo unreleased
  else
    while IFS= read -r line; do
      [[ -n "${line}" ]] || continue
      case "${line}" in chore\(release\)* | docs:* | docs\(* | test:* | test\(* | ci:* | ci\(*) continue ;; esac
      kind=chores
      case "${line}" in feat*) kind=features ;; fix*) kind=fixes ;; esac
      entry="$(printf '%s\n' "${line}" | sanitize_subject)" || return 1
      [[ -n "${entry}" ]] || continue
      entry="- $(tr '[:lower:]' '[:upper:]' <<<"${entry:0:1}")${entry:1}"
      case "${kind}" in
        features) features+="${entry}"$'\n' ;;
        fixes) fixes+="${entry}"$'\n' ;;
        *) chores+="${entry}"$'\n' ;;
      esac
    done <"${scratch}/subjects"
    if [[ -z "${features}${fixes}${chores}" ]]; then
      echo none
      return 0
    fi
    {
      [[ -z "${features}" ]] || printf '### Features\n\n%s' "${features}"
      [[ -z "${features}" || -z "${fixes}${chores}" ]] || printf '\n'
      [[ -z "${fixes}" ]] || printf '### Fixes\n\n%s' "${fixes}"
      [[ -z "${fixes}" || -z "${chores}" ]] || printf '\n'
      [[ -z "${chores}" ]] || printf '### Chores\n\n%s' "${chores}"
    } >"${scratch}/section-body.md" || return 1
    echo composed
  fi
  heading="## ${version} — $(date -u +%Y-%m-%d)"
  awk -v heading="${heading}" -v body="${scratch}/section-body.md" '
    /^## \[Unreleased\]/ && !done {
      print; print ""; print "No unreleased changes."; print ""; print heading; print ""
      while ((getline l < body) > 0) print l
      print ""
      done = 1; skip = 1; next
    }
    skip && /^## / { skip = 0 }
    !skip { print }
  ' "${in}" >"${out}" || return 1
}

if [[ "${mode}" == ship ]]; then
  # The squashed commit's title and body: an open pull request for this branch
  # names the change; otherwise the single work commit does; otherwise --title.
  pr_json=''
  if pr_out="$(gh pr view "${branch}" -R "${repo}" --json number,title,body,state,url 2>"${scratch}/pr.err")"; then
    [[ "$(jq -r '.state' <<<"${pr_out}")" != OPEN ]] || pr_json="${pr_out}"
  elif ! grep -q 'no pull requests found' "${scratch}/pr.err"; then
    printf '%s: FAILED: could not read the pull request for %s: %s\n' "${prog}" "${branch}" "$(tr '\n' ' ' <"${scratch}/pr.err" | cut -c1-300)" >&2
    refuse "cannot tell whether ${branch} has an open pull request"
  fi
  body=''
  if [[ -n "${title}" ]]; then
    body="$(git log --reverse --format='- %s' "${base}..HEAD")"
  elif [[ -n "${pr_json}" ]]; then
    title="$(jq -r '.title' <<<"${pr_json}")"
    body="$(jq -r '.body // ""' <<<"${pr_json}")"
  elif [[ "${ahead}" -eq 1 ]]; then
    title="$(git log -1 --format=%s HEAD)"
    body="$(git log -1 --format=%b HEAD)"
  else
    refuse "${ahead} commits ahead of origin/main and no open PR names them; pass TITLE='...' for the squashed commit"
  fi
  [[ -n "${title}" ]] || refuse "the squashed commit needs a title"
else
  title="chore(release): prepare ${version}"
  body="Release train: ${pending} commit(s) on main since ${latest}."
  # The subjects GoReleaser turns into the release notes: everything landed
  # since the last tag, plus this release-preparation commit.
  {
    git log --no-merges --reverse --format=%s "${latest_commit}..${base}"
    printf '%s\n' "${title}"
  } >"${scratch}/subjects"
  git show "${base}:CHANGELOG.md" >"${scratch}/CHANGELOG.base.md" 2>/dev/null ||
    refuse "origin/main has no CHANGELOG.md"
  changelog_source="$(compose_changelog "${scratch}/CHANGELOG.base.md" "${scratch}/CHANGELOG.md")" ||
    refuse "cannot compose the ${version} CHANGELOG section"
  if [[ "${changelog_source}" == none ]]; then
    log "only docs, tests or CI since ${latest}; checking that the ${latest} release completed"
    watch_release "${latest}" "${latest_commit}"
    log "nothing to release: the ${pending} commit(s) since ${latest} change only docs, tests or CI, and CHANGELOG.md has no Unreleased entries"
    exit 0
  fi
  log "CHANGELOG ${version} section: ${changelog_source}"
  awk -v v="^## ${version//./[.]}([[:space:]]|$)" '
    $0 ~ v { f = 1; print; next } f && /^## / { exit } f { print }
  ' "${scratch}/CHANGELOG.md" >"${scratch}/changelog-section.md"
  log "CHANGELOG section for ${version}:"
  sed "s/^/${prog}: | /" "${scratch}/changelog-section.md"
  guard_text changelog-section "${scratch}/changelog-section.md"
  guard_text release-note-subjects "${scratch}/subjects"
fi

{
  printf '%s\n' "${title}"
  [[ -z "${body}" ]] || printf '\n%s\n' "${body}"
} >"${scratch}/message"
guard_text "${mode}-commit-message" "${scratch}/message"
log "guard-b: the commit message$([[ "${mode}" == ship ]] || printf ', CHANGELOG section and release-note subjects') are clean"

gate_list="guard$([[ "${contracts_gate}" == false ]] || printf ', release-contracts'), lint, test-tagged, test-podman, build$([[ "${full}" == false ]] || printf ', vuln, release-dry-run')"
if [[ "${dry_run}" == true ]]; then
  if [[ "${mode}" == ship ]]; then
    plan "squash ${ahead} commit(s) onto origin/main ${base:0:12} as: ${title}"
  else
    plan "check out origin/main ${base:0:12} in a scratch worktree$([[ "${changelog_source}" == existing ]] || printf ' and write the %s CHANGELOG section above' "${version}")"
  fi
  plan "run the gates: ${gate_list}"
  plan "commit the tested tree with Local-Verify-Platform/Gates/Duration and Fast-Lane: on trailers"
  if [[ "${mode}" == ship ]]; then
    plan "push the commit to ${branch}, post ${status_context}=success on it$([[ -z "${pr_json}" ]] || printf ', comment on PR #%s' "$(jq -r .number <<<"${pr_json}")")"
  else
    plan "push the commit to release-train/${version} and post ${status_context}=success on it"
  fi
  plan "read FAST_LANE again, then fast-forward main (git push origin <sha>:refs/heads/main, never forced)"
  if [[ "${mode}" == ship ]]; then
    log "dry run complete: nothing was changed; ship never tags (make release does, once a day)"
    exit 0
  fi
  if [[ "${tagger}" == local ]]; then
    plan "read FAST_LANE and check ${version} again, then sign and push tag ${version} at that SHA with the local registered key, and require GitHub to verify it"
  else
    plan "read FAST_LANE and check ${version} again, then dispatch the central tagging workflow ${tagger_workflow} (repository=donmai, sha, version=${version}) and wait for the tag"
  fi
  [[ "${watch}" == false ]] || plan "watch ${watched_workflows[*]} for ${version}, the ${repo} release and ${tap_repo}/${cask_path}"
  log "dry run complete: nothing was changed"
  exit 0
fi

if [[ "${mode}" == release ]]; then
  release_tree="${scratch}/tree"
  git worktree add --quiet --detach "${release_tree}" "${base}" 2>"${scratch}/worktree.err" ||
    fail "cannot check out origin/main in a scratch worktree: $(tail -1 "${scratch}/worktree.err")"
  [[ "${changelog_source}" == existing ]] || cp "${scratch}/CHANGELOG.md" "${release_tree}/CHANGELOG.md"
  cd "${release_tree}"
  # A fresh lint cache: a shared one replays another worktree's paths.
  export GOLANGCI_LINT_CACHE="${scratch}/golangci-lint"
fi

# tree_of_worktree: the tree object of the working tree, written through a
# scratch index so the real index is untouched until the commit exists.
tree_of_worktree() {
  local index
  index="$(git rev-parse --git-path index)" || return 1
  cp "${index}" "${scratch}/index" || return 1
  GIT_INDEX_FILE="${scratch}/index" git add -A || return 1
  GIT_INDEX_FILE="${scratch}/index" git write-tree
}
tested_tree="$(tree_of_worktree)" || fail "cannot compute the tree to land"
base_tree="$(git rev-parse "${base}^{tree}")"
if [[ "${mode}" == ship && "${tested_tree}" == "${base_tree}" ]]; then
  log "nothing to ship: the tree of ${branch} equals origin/main ${base:0:12}"
  exit 0
fi

# --- 3. Gates ------------------------------------------------------------------

# guard_changed: `make guard` for a landing. `--staged` would see only what is
# staged; the landing commit is everything that differs from origin/main. Each
# step checks its own status: set -e does not apply inside a function that
# run_gate calls from an `if !` condition.
guard_changed() {
  local list file files=()
  bash scripts/guard-b-lint-selftest.sh || return 1
  bash scripts/guard-b-diff-gate-selftest.sh || return 1
  # The vendored guard must still match its pin, so a branch cannot weaken
  # the guard that checks it.
  bash scripts/check-guard-b-vendor-drift.sh || return 1
  list="$(git diff --name-only --diff-filter=ACMR "${base}" "${tested_tree}")" || return 1
  while IFS= read -r file; do
    if [[ -n "${file}" ]]; then files+=("${file}"); fi
  done <<<"${list}"
  if [[ ${#files[@]} -gt 0 ]]; then
    bash scripts/guard-b-lint.sh "${files[@]}" || return 1
  fi
  bash scripts/check-no-inbound-attach.sh || return 1
}

gate_names=()
gate_lines=()
run_gate() {
  local name="$1" t0=${SECONDS}
  shift
  log "gate ${name}: $*"
  if ! "$@" >"${scratch}/gate-${name}.log" 2>&1 </dev/null; then
    tail -40 "${scratch}/gate-${name}.log" >&2
    fail "gate ${name} is red; nothing was committed or pushed"
  fi
  gate_names+=("${name}")
  gate_lines+=("${name}: ok in $((SECONDS - t0))s")
  log "gate ${name}: ok ($((SECONDS - t0))s)"
}
run_gate guard guard_changed
if [[ "${contracts_gate}" == true ]]; then
  run_gate release-contracts bash scripts/test-release-workflows.sh
else
  log "gate release-contracts: skipped (nothing under ${contract_paths[*]} changed since ${contracts_from:0:12})"
fi
run_gate lint make lint
run_gate test-tagged make test-tagged
run_gate test-podman scripts/podman-go-test.sh -race ./...
run_gate build make build
if [[ "${full}" == true ]]; then
  run_gate vuln make vuln
  run_gate release-dry-run make release-dry-run
fi

after_gates="$(tree_of_worktree)" || fail "cannot compute the tree after the gates"
[[ "${after_gates}" == "${tested_tree}" ]] || fail "the gates changed tracked files; the tested tree is no longer the tree to land"

# --- 4. Record and land ----------------------------------------------------------

duration="$(((SECONDS - started) / 60))m$(((SECONDS - started) % 60))s"
# A neutral platform label, never the host name: the trailers, the status and
# the PR comment are public.
platform="$(uname -s | tr '[:upper:]' '[:lower:]')/$(uname -m)"
gates_line="${gate_names[*]}"
pushed_branch=''
if [[ "${tested_tree}" == "${base_tree}" ]]; then
  sha="${base}"
  log "origin/main ${sha:0:12} already carries the ${version} section; releasing it as it is"
else
  git interpret-trailers --in-place \
    --trailer "Local-Verify-Platform: ${platform}" \
    --trailer "Local-Verify-Gates: ${gates_line}" \
    --trailer "Local-Verify-Duration: ${duration}" \
    --trailer "Fast-Lane: on" \
    "${scratch}/message"
  guard_text "${mode}-commit-message" "${scratch}/message"
  sha="$(git commit-tree "${tested_tree}" -p "${base}" -F "${scratch}/message")" || fail "git commit-tree failed"
  if [[ "${mode}" == ship ]]; then
    git update-ref -m "fast-lane ship" "refs/heads/${branch}" "${sha}" "${orig_head}" || fail "cannot move ${branch} to ${sha:0:12}"
    git read-tree "${sha}" || fail "cannot update the index to ${sha:0:12}"
    [[ -z "$(git status --porcelain --untracked-files=normal)" ]] || fail "the worktree does not match the landed commit ${sha}"
    log "committed ${sha:0:12} (${ahead} work commit(s) squashed)"
    git push --quiet --force-with-lease="refs/heads/${branch}:${remote_branch}" origin "${sha}:refs/heads/${branch}" ||
      fail "could not push ${sha:0:12} to ${branch}"
  else
    pushed_branch="release-train/${version}"
    observed="$(git ls-remote origin "refs/heads/${pushed_branch}")" || fail "cannot read origin/${pushed_branch}"
    observed="$(awk '{ print $1 }' <<<"${observed}")"
    log "committed the release preparation ${sha:0:12}"
    # A branch left by an earlier failed run is replaced, but only if it is
    # still exactly what this run just read.
    git push --quiet --force-with-lease="refs/heads/${pushed_branch}:${observed}" origin "${sha}:refs/heads/${pushed_branch}" ||
      fail "could not push ${sha:0:12} to ${pushed_branch}"
  fi
fi

description="${platform}; ${gates_line}; ${duration}"
gh_read "post the ${status_context} status" api --method POST "repos/${repo}/statuses/${sha}" \
  -f state=success -f context="${status_context}" -f description="${description:0:140}" >/dev/null ||
  fail "could not post the ${status_context} status on ${sha}"
status_json="$(gh_read "read back the ${status_context} status" api "repos/${repo}/commits/${sha}/status")" ||
  fail "could not read back the ${status_context} status on ${sha}"
readback="$(jq -r --arg c "${status_context}" '[.statuses[]? | select(.context == $c)] | first | .state // ""' <<<"${status_json}")"
[[ "${readback}" == success ]] || fail "the ${status_context} status on ${sha} reads back '${readback}'"
log "posted ${status_context}=success on ${sha:0:12}: ${description:0:140}"

if [[ "${mode}" == ship && -n "${pr_json}" ]]; then
  {
    printf 'Fast-lane ship of %s (%s).\n\n' "\`${sha}\`" "${platform}"
    printf -- '- %s\n' "${gate_lines[@]}"
    printf '\nLanding on main as a fast-forward; the daily release train publishes it.\n'
  } >"${scratch}/pr-comment.md"
  guard_text ship-pr-comment "${scratch}/pr-comment.md"
  gh_read "comment on PR #$(jq -r .number <<<"${pr_json}")" pr comment "$(jq -r .number <<<"${pr_json}")" -R "${repo}" \
    --body-file "${scratch}/pr-comment.md" >/dev/null ||
    log "the PR comment is not fatal: the status and trailers are the record"
fi

recheck_switch "the fast-forward of main"
[[ "${mode}" == ship ]] || version_free "the fast-forward of main"
git fetch --quiet origin '+refs/heads/main:refs/remotes/origin/main' 2>"${scratch}/fetch.err" ||
  fail "git fetch origin main failed: $(tail -1 "${scratch}/fetch.err")"
now_main="$(git rev-parse refs/remotes/origin/main)"
if ! git merge-base --is-ancestor "${now_main}" "${sha}"; then
  fail "main moved while the gates ran; run make ${mode} again (nothing landed)"
fi
if [[ "${now_main}" != "${sha}" ]]; then
  git push --quiet origin "${sha}:refs/heads/main" 2>"${scratch}/push-main.err" ||
    fail "the fast-forward of main to ${sha:0:12} was rejected: $(tail -1 "${scratch}/push-main.err")"
fi
landed="$(git ls-remote origin refs/heads/main)" || fail "cannot read main back"
landed="$(awk '{ print $1 }' <<<"${landed}")"
[[ "${landed}" == "${sha}" ]] || fail "main reads back ${landed}, not ${sha}"
log "main is ${sha:0:12}"
if [[ -n "${pushed_branch}" ]]; then
  git push --quiet origin --delete "refs/heads/${pushed_branch}" 2>/dev/null ||
    log "warning: could not delete ${pushed_branch}; it is merged, delete it by hand"
fi
if [[ "${mode}" == ship ]]; then
  log "landed ${sha:0:12} on main in ${duration}; the daily release train (make release) publishes it"
  exit 0
fi

# --- 5. Tag ------------------------------------------------------------------------

recheck_switch "tagging ${version}"
version_free "tagging ${version}"
if [[ "${tagger}" == local ]]; then
  # Interim until the central tagger is live: the operator's registered
  # signing key signs, exactly as in RELEASING.md "Create the release tag".
  # drop_local_tag: remove the local tag this run made, and its marker.
  drop_local_tag() {
    git tag -d "${version}" >/dev/null 2>&1 || true
    rm -f -- "${tag_marker}"
  }
  if git rev-parse --quiet --verify "refs/tags/${version}" >/dev/null; then
    git tag -d "${version}" >/dev/null || fail "cannot remove the local-only ${version} tag an earlier run made"
    log "removed a local-only ${version} tag an earlier run made (it was never pushed)"
  fi
  git tag -s "${version}" "${sha}" -m "${version}" </dev/null 2>"${scratch}/tag.err" || {
    drop_local_tag
    fail "git tag -s ${version} failed: $(tail -1 "${scratch}/tag.err")"
  }
  if ! { mkdir -p "${tag_marker%/*}" && git rev-parse "refs/tags/${version}" >"${tag_marker}"; }; then
    drop_local_tag
    fail "cannot record the local ${version} tag at ${tag_marker}; removed it"
  fi
  scripts/verify-release-signing-key.sh --verify-tag "${version}" >"${scratch}/verify-tag.log" 2>&1 </dev/null || {
    drop_local_tag
    fail "the local tag ${version} does not verify ($(tail -1 "${scratch}/verify-tag.log")); removed it"
  }
  if ! git push --quiet origin "refs/tags/${version}" 2>"${scratch}/push-tag.err"; then
    target="$(remote_tag_target)" || target=''
    if [[ "${target}" != "${sha}" ]]; then
      drop_local_tag
      fail "pushing tag ${version} failed ($(tail -1 "${scratch}/push-tag.err")); removed the local tag so the next run can retry"
    fi
    log "the tag push reported an error, but origin has ${version} at ${sha:0:12}"
  fi
  rm -f -- "${tag_marker}"
else
  gh_read "dispatch the central tagging workflow" workflow run "${tagger_workflow}" -R "${tagger_repo}" \
    -f repository=donmai -f sha="${sha}" -f version="${version}" >/dev/null ||
    fail "could not dispatch the central tagging workflow for ${version}"
  log "dispatched the central tagger for ${version} at ${sha:0:12}; waiting for the tag"
  deadline=$((SECONDS + tag_wait_seconds))
  while :; do
    target="$(remote_tag_target)" || fail "cannot read the origin tags while waiting for ${version}"
    [[ "${target}" != "${sha}" ]] || break
    [[ -z "${target}" ]] ||
      fail "tag ${version} appeared at ${target:0:12}, not ${sha:0:12}; the tag is immutable, so fix the tagger and release the next version"
    [[ ${SECONDS} -lt ${deadline} ]] || fail "the central tagger did not create ${version} within ${tag_wait_seconds}s"
    sleep "${poll_seconds}"
  done
fi
target="$(remote_tag_target)" || fail "cannot read tag ${version} back"
[[ "${target}" == "${sha}" ]] || fail "tag ${version} points at '${target}', not ${sha}"
# release-authority requires GitHub's own verification of the tag signature.
ref_json="$(gh_read "read the ${version} tag ref" api "repos/${repo}/git/ref/tags/${version}")" ||
  fail "cannot check the ${version} signature"
[[ "$(jq -r '.object.type // ""' <<<"${ref_json}")" == tag ]] || fail "${version} is not an annotated tag"
tag_json="$(gh_read "read the ${version} tag object" api "repos/${repo}/git/tags/$(jq -r '.object.sha' <<<"${ref_json}")")" ||
  fail "cannot check the ${version} signature"
verified="$(jq -r '"\(.verification.verified) \(.verification.reason)"' <<<"${tag_json}")"
[[ "${verified}" == "true valid" ]] ||
  fail "GitHub does not verify the ${version} signature (${verified}); release-authority will refuse it. The tag is immutable: fix the signer and release the next patch"
log "tag ${version} -> ${sha:0:12} (GitHub-verified signature)"

# --- 6. Watch -------------------------------------------------------------------------

if [[ "${watch}" == false ]]; then
  log "tagged ${version} at ${sha:0:12}; not watching (--no-watch)"
  exit 0
fi
watch_release "${version}" "${sha}"
log "released ${version} at ${sha:0:12}; total $(((SECONDS - started) / 60))m$(((SECONDS - started) % 60))s"
