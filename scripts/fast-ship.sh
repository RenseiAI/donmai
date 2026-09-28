#!/usr/bin/env bash
# fast-ship.sh — the fast-lane ship command (`make ship`).
#
# Lands this worktree's work on main as ONE commit that passed the local gates,
# then releases it through the existing release workflows with a signed tag:
#
#   1. Preflight, read-only; any failure refuses before anything changes:
#      FAST_LANE is exactly "on", the gh login is a repository admin, this is a
#      linked worktree on a branch (never the primary checkout, never main), the
#      tree is clean, HEAD contains fresh origin/main, the version is the
#      smallest increment from the latest tag and unused (tag and GitHub
#      release), and the tag signer is ready.
#   2. Compose: write the CHANGELOG section (the existing one, the Unreleased
#      entries, or one composed from the subjects landed since the last tag),
#      then run guard-b over everything this ship publishes: the changed files,
#      the squashed commit message, the CHANGELOG section, and the commit
#      subjects GoReleaser turns into the release notes.
#   3. Gates, fast first, stop at the first red: the guard self-tests and the
#      attach-path listener check, lint (gofumpt included), the build-tagged
#      test type-check, `go test -race ./...` in a Linux podman container (the
#      daemon install tests never run on this host), and the build. --full adds
#      `make vuln` and `make release-dry-run`.
#   4. Record and land: commit the exact tested tree onto origin/main with
#      Local-Verify-* trailers, push it to this branch, post the `local-verify`
#      commit status on that SHA, comment on the open PR, and fast-forward main
#      to the SHA (never a force push).
#   5. Tag at that SHA: sign locally with the operator's registered signing key
#      (interim), or dispatch the central tagging workflow once the
#      tagging-identity pins are set. GitHub must verify the signature.
#   6. Watch the release run, the GitHub release and the Homebrew cask.
#
# --dry-run runs every read-only check, composes the CHANGELOG in a scratch copy
# and runs the guard on everything above, prints the remaining steps, and
# changes nothing: no commit, status, push or tag.
#
# Usage: scripts/fast-ship.sh [--dry-run] [--version vX.Y.Z] [--title TEXT]
#          [--tagger auto|local|central] [--full] [--no-watch]
# Make:  make ship [VERSION=vX.Y.Z] [TITLE=...] [TAGGER=...] [FULL=1]
#          [DRY_RUN=1] [NO_WATCH=1]
#
# Exit codes: 0 shipped (or dry run clean), 1 a gate or step failed,
#             2 usage, 3 refused by preflight.

set -euo pipefail

readonly repo='RenseiAI/donmai'
readonly tap_repo='RenseiAI/homebrew-tap'
readonly cask_path='Casks/donmai.rb'
readonly tagger_repo='RenseiAI/release-tagger'
readonly tagger_workflow='tag-release.yml'
readonly status_context='local-verify'
readonly semver_re='^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$'

dry_run=false
version=''
title=''
tagger='auto'
full=false
watch=true

log() { printf 'fast-ship: %s\n' "$*"; }
plan() { printf 'fast-ship: [dry-run] would %s\n' "$*"; }
refuse() {
  printf 'fast-ship: REFUSED: %s\n' "$*" >&2
  exit 3
}
fail() {
  printf 'fast-ship: FAILED: %s\n' "$*" >&2
  exit 1
}
usage() {
  sed -n '/^# Usage:/,/^# Exit codes/p' "$0" | sed 's/^# \{0,1\}//' >&2
  exit 2
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --dry-run) dry_run=true ;;
    --version) [[ $# -ge 2 ]] || usage; version="$2"; shift ;;
    --title) [[ $# -ge 2 ]] || usage; title="$2"; shift ;;
    --tagger) [[ $# -ge 2 ]] || usage; tagger="$2"; shift ;;
    --full) full=true ;;
    --no-watch) watch=false ;;
    -h | --help) usage ;;
    *) printf 'fast-ship: unknown argument: %s\n' "$1" >&2; usage ;;
  esac
  shift
done

[[ -z "${version}" || "${version}" =~ ${semver_re} ]] || { printf 'fast-ship: --version must be vX.Y.Z, got %s\n' "${version}" >&2; exit 2; }
case "${tagger}" in auto | local | central) ;; *) printf 'fast-ship: --tagger must be auto, local or central\n' >&2; exit 2 ;; esac

# Gate `make` calls behave exactly like a developer's plain invocation, not like
# a sub-make of `make ship VERSION=...`.
unset MAKEFLAGS MFLAGS MAKELEVEL
export GOWORK=off

started=${SECONDS}
scratch="$(mktemp -d)"
composed_files=()
committed=false
root=''

cleanup() {
  local status=$?
  # A failure after compose and before the commit puts back the files the
  # script edited, so a re-run starts from the same clean tree.
  if [[ "${committed}" == false && ${#composed_files[@]} -gt 0 ]]; then
    local file
    for file in "${composed_files[@]}"; do
      cp -p "${scratch}/orig/${file}" "${root}/${file}"
    done
    log "restored ${composed_files[*]} after the failure"
  fi
  rm -rf -- "${scratch}"
  exit "${status}"
}
trap cleanup EXIT

for tool in git gh jq; do
  command -v "${tool}" >/dev/null 2>&1 || refuse "${tool} is required"
done

# --- 1. Preflight (read-only) ------------------------------------------------

root="$(git rev-parse --show-toplevel 2>/dev/null)" || refuse "run inside a donmai worktree"
cd "${root}"

# gh_lookup <endpoint>: 0 when the resource exists, 1 when GitHub answers 404,
# 2 when it cannot be read at all (callers fail closed on that).
gh_lookup() {
  local out
  if out="$(gh api "$1" 2>&1)"; then
    return 0
  fi
  if grep -q 'HTTP 404' <<<"${out}"; then
    return 1
  fi
  return 2
}

# Actions variables as the workflows see them: a repository variable wins over
# the organization variable of the same name.
org_vars="$(gh api --paginate "repos/${repo}/actions/organization-variables?per_page=30" 2>/dev/null)" ||
  refuse "cannot read the organization Actions variables for ${repo} (is gh logged in?)"
repo_vars="$(gh api --paginate "repos/${repo}/actions/variables?per_page=30" 2>/dev/null)" ||
  refuse "cannot read the repository Actions variables for ${repo}"
variables="$(jq -n \
  --argjson org "$(jq -s '[.[].variables[]?]' <<<"${org_vars}")" \
  --argjson repo "$(jq -s '[.[].variables[]?]' <<<"${repo_vars}")" \
  '($org + $repo) | map({(.name): .value}) | add // {}')" ||
  refuse "cannot parse the Actions variables for ${repo}"
var() { jq -r --arg name "$1" '.[$name] // ""' <<<"${variables}"; }

fast_lane="$(var FAST_LANE)"
[[ "${fast_lane}" == "on" ]] ||
  refuse "FAST_LANE is not on (it is '${fast_lane:-unset}'). Ship through a reviewed PR and the release steps in RELEASING.md."
log "FAST_LANE is on"

admin="$(gh api "repos/${repo}" 2>/dev/null | jq -r '.permissions.admin // false')" ||
  refuse "cannot read your permissions on ${repo}"
[[ "${admin}" == "true" ]] || refuse "the gh login is not an admin of ${repo}; only an admin can fast-forward main"
login="$(gh api user 2>/dev/null | jq -r '.login // ""')"
log "gh login ${login:-<unknown>} is an admin of ${repo}"

git_dir="$(cd "$(git rev-parse --absolute-git-dir)" && pwd -P)"
common_dir="$(cd "$(git rev-parse --git-common-dir)" && pwd -P)"
[[ "${git_dir}" != "${common_dir}" ]] ||
  refuse "this is the primary checkout; run make ship from a linked worktree (scripts/create-worktree.sh <name>)"
branch="$(git symbolic-ref --quiet --short HEAD)" || refuse "HEAD is detached; run make ship from a branch"
[[ "${branch}" != "main" ]] || refuse "the worktree is on main; run make ship from a work branch"
[[ -z "$(git status --porcelain --untracked-files=normal)" ]] ||
  refuse "the worktree has uncommitted or untracked changes; commit or remove them first"

git fetch --quiet --tags origin '+refs/heads/main:refs/remotes/origin/main' || fail "git fetch origin main failed"
base="$(git rev-parse --verify 'refs/remotes/origin/main^{commit}')"
git merge-base --is-ancestor "${base}" HEAD ||
  refuse "HEAD does not contain origin/main (${base:0:12}); rebase onto origin/main and re-run"
orig_head="$(git rev-parse HEAD)"
ahead="$(git rev-list --count "${base}..HEAD")"
remote_branch="$(git ls-remote origin "refs/heads/${branch}" | awk '{ print $1 }')"
if [[ -n "${remote_branch}" ]] && ! git merge-base --is-ancestor "${remote_branch}" HEAD 2>/dev/null; then
  refuse "origin/${branch} has commits this worktree does not (${remote_branch:0:12}); pull them first"
fi
# The branch name is published by the push below; guard it like CI does.
printf '%s\n' "${branch}" | bash scripts/guard-b-lint.sh --stdin ship-branch-name >"${scratch}/guard-branch.log" 2>&1 || {
  cat "${scratch}/guard-branch.log" >&2
  refuse "the branch name fails guard-b; rename the branch"
}

# The latest release is read from origin, so a stale local-only tag can
# neither move the baseline nor hide an existing remote one.
remote_tags="$(git ls-remote --tags --refs origin 'refs/tags/v*' | awk '{ sub("refs/tags/", "", $2); print $2 }')" ||
  fail "cannot list origin tags"
latest="$(grep -E "${semver_re}" <<<"${remote_tags}" | sed 's/^v//' | sort -t. -k1,1n -k2,2n -k3,3n | tail -1 || true)"
latest="${latest:+v${latest}}"
[[ -n "${latest}" ]] || refuse "no vX.Y.Z tag found; the first release is not a fast-lane release"
IFS=. read -r major minor patch <<<"${latest#v}"
next_patch="v${major}.${minor}.$((patch + 1))"
next_minor="v${major}.$((minor + 1)).0"
next_major="v$((major + 1)).0.0"
version="${version:-${next_patch}}"
case "${version}" in
  "${next_patch}" | "${next_minor}" | "${next_major}") ;;
  *) refuse "${version} is not the smallest increment from ${latest}; use ${next_patch}, ${next_minor} or ${next_major}" ;;
esac
if git rev-parse --quiet --verify "refs/tags/${version}" >/dev/null || grep -qx -- "${version}" <<<"${remote_tags}"; then
  refuse "tag ${version} already exists"
fi
rc=0
gh_lookup "repos/${repo}/releases/tags/${version}" || rc=$?
case "${rc}" in
  0) refuse "${repo} already has a ${version} release" ;;
  1) ;;
  *) refuse "cannot read ${repo} releases to prove ${version} is unused" ;;
esac
log "version ${version} (latest ${latest})"

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
  scripts/verify-release-signing-key.sh >"${scratch}/signing-key.log" 2>&1 ||
    refuse "the local signing key is not ready: $(tail -1 "${scratch}/signing-key.log")"
  log "tag signer: local registered key ($(tail -1 "${scratch}/signing-key.log"))"
else
  rc=0
  gh_lookup "repos/${tagger_repo}/actions/workflows/${tagger_workflow}" || rc=$?
  case "${rc}" in
    0) log "tag signer: central tagging workflow ${tagger_repo}/${tagger_workflow}" ;;
    1)
      [[ "${pins_active}" == false ]] ||
        refuse "${tagger_repo}/${tagger_workflow} does not exist, and the tagging-identity pins are set, so no signer can produce an accepted tag"
      refuse "the central tagging workflow ${tagger_repo}/${tagger_workflow} does not exist yet; use --tagger local until it does"
      ;;
    *) refuse "cannot read ${tagger_repo}; the central tagger is unavailable" ;;
  esac
fi

# --- 2. Compose ----------------------------------------------------------------

# Commit message title and body. An open PR for this branch names the change;
# otherwise the single work commit does; otherwise --title is required.
pr_json=''
if pr_out="$(gh pr view "${branch}" -R "${repo}" --json number,title,body,state,url 2>/dev/null)" &&
  [[ "$(jq -r '.state' <<<"${pr_out}")" == "OPEN" ]]; then
  pr_json="${pr_out}"
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
elif [[ "${ahead}" -eq 0 ]]; then
  title="chore(release): prepare ${version}"
else
  refuse "${ahead} commits ahead of origin/main and no open PR names them; pass TITLE='...' for the squashed commit"
fi
[[ -n "${title}" ]] || refuse "the squashed commit needs a title"

# sanitize_subject: turn one commit subject into a CHANGELOG line.
sanitize_subject() {
  sed -E \
    -e 's/^\[[^]]*\][[:space:]]*//' \
    -e 's/[[:space:]]*\(#[0-9]+\)//g' \
    -e 's/^[a-z]+(\([^)]*\))?!?:[[:space:]]*//' \
    -e 's/[[:space:]]+$//'
}

# compose_changelog <in> <out>: prints existing|unreleased|composed.
compose_changelog() {
  local in="$1" out="$2" escaped unreleased heading
  escaped="${version//./\\.}"
  heading="## ${version} — $(date -u +%Y-%m-%d)"
  if grep -Eq "^## ${escaped}([[:space:]]|\$)" "${in}"; then
    cp "${in}" "${out}"
    echo existing
    return
  fi
  grep -q '^## \[Unreleased\]' "${in}" || fail "CHANGELOG.md has no '## [Unreleased]' heading to release under"
  unreleased="$(awk '/^## \[Unreleased\]/ { f = 1; next } f && /^## / { exit } f { print }' "${in}")"
  if [[ -n "$(tr -d '[:space:]-' <<<"${unreleased}")" ]]; then
    awk -v heading="${heading}" '
      /^## \[Unreleased\]/ && !done { print; print ""; print heading; done = 1; next }
      { print }
    ' "${in}" >"${out}"
    echo unreleased
    return
  fi
  local features='' fixes='' chores='' line kind entry
  while IFS= read -r line; do
    [[ -n "${line}" ]] || continue
    case "${line}" in chore\(release\)* | docs:* | docs\(* | test:* | test\(* | ci:* | ci\(*) continue ;; esac
    kind=chores
    case "${line}" in feat*) kind=features ;; fix*) kind=fixes ;; esac
    entry="$(printf '%s\n' "${line}" | sanitize_subject)"
    [[ -n "${entry}" ]] || continue
    entry="- $(tr '[:lower:]' '[:upper:]' <<<"${entry:0:1}")${entry:1}"
    case "${kind}" in
      features) features+="${entry}"$'\n' ;;
      fixes) fixes+="${entry}"$'\n' ;;
      *) chores+="${entry}"$'\n' ;;
    esac
  done <"${scratch}/subjects"
  [[ -n "${features}${fixes}${chores}" ]] ||
    refuse "no release-note entries since ${latest}; add them under '## [Unreleased]' in CHANGELOG.md"
  {
    printf '%s\n' "${heading}"
    [[ -z "${features}" ]] || printf '\n### Features\n\n%s' "${features}"
    [[ -z "${fixes}" ]] || printf '\n### Fixes\n\n%s' "${fixes}"
    [[ -z "${chores}" ]] || printf '\n### Chores\n\n%s' "${chores}"
  } >"${scratch}/section.md"
  awk -v section="${scratch}/section.md" '
    /^## \[Unreleased\]/ && !done {
      print; print ""
      while ((getline l < section) > 0) print l
      done = 1; next
    }
    { print }
  ' "${in}" >"${out}"
  echo composed
}

# The subjects the release notes are generated from: everything landed since
# the last tag, plus this ship's squashed title.
{
  git log --no-merges --reverse --format=%s "${latest}..${base}"
  printf '%s\n' "${title}"
} >"${scratch}/subjects"

mkdir -p "${scratch}/orig"
cp -p CHANGELOG.md "${scratch}/orig/"
changelog_source="$(compose_changelog CHANGELOG.md "${scratch}/CHANGELOG.md")"
log "CHANGELOG ${version} section: ${changelog_source}"
escaped_version="${version//./\\.}"
awk -v v="^## ${escaped_version}([[:space:]]|$)" '
  $0 ~ v { f = 1; print; next } f && /^## / { exit } f { print }
' "${scratch}/CHANGELOG.md" >"${scratch}/changelog-section.md"
log "CHANGELOG section for ${version}:"
sed 's/^/    | /' "${scratch}/changelog-section.md"

{
  printf '%s\n' "${title}"
  [[ -z "${body}" ]] || printf '\n%s\n' "${body}"
} >"${scratch}/message"

# guard_text <label> <file>: guard-b over text that has no file in the tree.
guard_text() {
  bash scripts/guard-b-lint.sh --stdin "$1" <"$2" >"${scratch}/guard-$1.log" 2>&1 || {
    cat "${scratch}/guard-$1.log" >&2
    refuse "guard-b blocked the ${1//-/ }; rewrite it brand-neutrally"
  }
}
guard_text ship-commit-message "${scratch}/message"
guard_text changelog-section "${scratch}/changelog-section.md"
guard_text release-note-subjects "${scratch}/subjects"
log "guard-b: the commit message, CHANGELOG section and release-note subjects are clean"

if [[ "${dry_run}" == true ]]; then
  if [[ "${ahead}" -gt 0 ]]; then
    plan "squash ${ahead} commit(s) onto origin/main ${base:0:12} as: ${title}"
  else
    plan "commit the release preparation onto origin/main ${base:0:12} as: ${title}"
  fi
  [[ "${changelog_source}" == existing ]] || plan "write the ${version} CHANGELOG section above into CHANGELOG.md"
  plan "run the gates: guard (self-tests, changed files, listener check), make lint, make test-tagged, scripts/podman-go-test.sh -race ./..., make build$([[ "${full}" == false ]] || printf ', make vuln, make release-dry-run')"
  plan "commit the tested tree with Local-Verify-Host/Gates/Duration and Fast-Lane: on trailers"
  plan "push the commit to ${branch}, post ${status_context}=success on it$([[ -z "${pr_json}" ]] || printf ', comment on PR #%s' "$(jq -r .number <<<"${pr_json}")")"
  plan "fast-forward main (git push origin <sha>:refs/heads/main, never forced)"
  if [[ "${tagger}" == local ]]; then
    plan "sign and push tag ${version} at that SHA with the local registered key, then require GitHub to verify it"
  else
    plan "dispatch ${tagger_repo}/${tagger_workflow} (repository=donmai, sha, version=${version}) and wait for the tag"
  fi
  [[ "${watch}" == false ]] || plan "watch release.yml for ${version}, the ${repo} release and ${tap_repo}/${cask_path}"
  log "dry run complete: nothing was changed"
  exit 0
fi

if [[ "${changelog_source}" != existing ]]; then
  composed_files+=(CHANGELOG.md)
  cp "${scratch}/CHANGELOG.md" CHANGELOG.md
fi

# tree_of_worktree: the tree object of the working tree, written through a
# scratch index so the real index is untouched until the commit exists.
tree_of_worktree() {
  cp "$(git rev-parse --git-path index)" "${scratch}/index"
  GIT_INDEX_FILE="${scratch}/index" git add -A
  GIT_INDEX_FILE="${scratch}/index" git write-tree
}
tested_tree="$(tree_of_worktree)"

# --- 3. Gates ------------------------------------------------------------------

# guard_changed: `make guard` for a squash. `--staged` would see only what is
# staged; the landing commit is everything that differs from origin/main.
guard_changed() {
  local files=()
  bash scripts/guard-b-lint-selftest.sh
  bash scripts/guard-b-diff-gate-selftest.sh
  while IFS= read -r file; do
    [[ -n "${file}" ]] && files+=("${file}")
  done < <(git diff --name-only --diff-filter=ACMR "${base}" "${tested_tree}")
  if [[ ${#files[@]} -gt 0 ]]; then
    bash scripts/guard-b-lint.sh "${files[@]}"
  fi
  bash scripts/check-no-inbound-attach.sh
}

gate_names=()
gate_lines=()
run_gate() {
  local name="$1" t0=${SECONDS}
  shift
  log "gate ${name}: $*"
  if ! "$@" >"${scratch}/gate-${name}.log" 2>&1; then
    tail -40 "${scratch}/gate-${name}.log" >&2
    fail "gate ${name} is red; nothing was committed or pushed"
  fi
  gate_names+=("${name}")
  gate_lines+=("${name}: ok in $((SECONDS - t0))s")
  log "gate ${name}: ok ($((SECONDS - t0))s)"
}
run_gate guard guard_changed
run_gate lint make lint
run_gate test-tagged make test-tagged
run_gate test-podman scripts/podman-go-test.sh -race ./...
run_gate build make build
if [[ "${full}" == true ]]; then
  run_gate vuln make vuln
  run_gate release-dry-run make release-dry-run
fi

[[ "$(tree_of_worktree)" == "${tested_tree}" ]] || fail "the gates changed tracked files; the tested tree is no longer the tree to land"

# --- 4. Record and land ----------------------------------------------------------

duration="$(((SECONDS - started) / 60))m$(((SECONDS - started) % 60))s"
host="$(hostname -s 2>/dev/null || hostname)"
gates_line="${gate_names[*]}"
if [[ "${tested_tree}" == "$(git rev-parse "${base}^{tree}")" ]]; then
  sha="${base}"
  log "nothing to commit: ${version} releases origin/main ${sha:0:12} as it is"
else
  git interpret-trailers --in-place \
    --trailer "Local-Verify-Host: ${host}" \
    --trailer "Local-Verify-Gates: ${gates_line}" \
    --trailer "Local-Verify-Duration: ${duration}" \
    --trailer "Fast-Lane: on" \
    "${scratch}/message"
  # The host name is published in the trailers; guard the final message.
  guard_text ship-commit-message "${scratch}/message"
  sha="$(git commit-tree "${tested_tree}" -p "${base}" -F "${scratch}/message")"
  git update-ref -m "fast-ship: ${version}" "refs/heads/${branch}" "${sha}" "${orig_head}"
  git read-tree "${sha}"
  committed=true
  [[ -z "$(git status --porcelain --untracked-files=normal)" ]] || fail "the worktree does not match the landed commit ${sha}"
  log "committed ${sha:0:12} (${ahead} work commit(s) squashed)"
  git push --quiet --force-with-lease="refs/heads/${branch}:${remote_branch}" origin "${sha}:refs/heads/${branch}" ||
    fail "could not push ${sha:0:12} to ${branch}"
fi
committed=true

description="${host}; ${gates_line}; ${duration}"
gh api --method POST "repos/${repo}/statuses/${sha}" \
  -f state=success -f context="${status_context}" -f description="${description:0:140}" >/dev/null ||
  fail "could not post the ${status_context} status on ${sha}"
readback="$(gh api "repos/${repo}/commits/${sha}/status" | jq -r --arg c "${status_context}" \
  '[.statuses[]? | select(.context == $c)] | first | .state // ""')"
[[ "${readback}" == success ]] || fail "the ${status_context} status on ${sha} reads back '${readback}'"
log "posted ${status_context}=success on ${sha:0:12}: ${description:0:140}"

if [[ -n "${pr_json}" ]]; then
  {
    printf 'Fast-lane ship of %s as %s from %s.\n\n' "\`${sha}\`" "${version}" "${host}"
    printf -- '- %s\n' "${gate_lines[@]}"
    printf '\nLanding on main as a fast-forward; the release runs from a signed %s tag.\n' "${version}"
  } >"${scratch}/pr-comment.md"
  guard_text ship-pr-comment "${scratch}/pr-comment.md"
  gh pr comment "$(jq -r .number <<<"${pr_json}")" -R "${repo}" --body-file "${scratch}/pr-comment.md" >/dev/null ||
    log "warning: could not comment on the PR (the status and trailers are the record)"
fi

git fetch --quiet origin '+refs/heads/main:refs/remotes/origin/main' || fail "git fetch origin main failed"
if ! git merge-base --is-ancestor "$(git rev-parse refs/remotes/origin/main)" "${sha}"; then
  fail "main moved while the gates ran; rebase onto origin/main and run make ship again (nothing landed)"
fi
if [[ "$(git rev-parse refs/remotes/origin/main)" != "${sha}" ]]; then
  git push --quiet origin "${sha}:refs/heads/main" || fail "the fast-forward of main to ${sha:0:12} was rejected"
fi
landed="$(git ls-remote origin refs/heads/main | awk '{ print $1 }')"
[[ "${landed}" == "${sha}" ]] || fail "main reads back ${landed}, not ${sha}"
log "main is ${sha:0:12}"

# --- 5. Tag ------------------------------------------------------------------------

tag_target() { git ls-remote origin "refs/tags/${version}^{}" | awk '{ print $1 }'; }
if [[ "${tagger}" == local ]]; then
  # Interim until the central tagger is live: the operator's registered
  # signing key signs, exactly as in RELEASING.md "Create the release tag".
  git tag -s "${version}" "${sha}" -m "${version}" || fail "git tag -s ${version} failed"
  scripts/verify-release-signing-key.sh --verify-tag "${version}" >/dev/null || fail "the local tag ${version} does not verify"
  git push --quiet origin "refs/tags/${version}" || fail "pushing tag ${version} failed"
else
  gh workflow run "${tagger_workflow}" -R "${tagger_repo}" \
    -f repository=donmai -f sha="${sha}" -f version="${version}" ||
    fail "could not dispatch ${tagger_repo}/${tagger_workflow}"
  log "dispatched the central tagger for ${version} at ${sha:0:12}; waiting for the tag"
  deadline=$((SECONDS + 900))
  until [[ "$(tag_target)" == "${sha}" ]]; do
    [[ ${SECONDS} -lt ${deadline} ]] || fail "the central tagger did not create ${version} within 15 minutes"
    sleep 15
  done
fi
[[ "$(tag_target)" == "${sha}" ]] || fail "tag ${version} does not point at ${sha}"
# release-authority requires GitHub's own verification of the tag signature.
tag_object="$(gh api "repos/${repo}/git/ref/tags/${version}" | jq -r '.object.sha // ""')"
verified="$(gh api "repos/${repo}/git/tags/${tag_object}" | jq -r '"\(.verification.verified) \(.verification.reason)"')"
[[ "${verified}" == "true valid" ]] ||
  fail "GitHub does not verify the ${version} signature (${verified}); release-authority will refuse it. The tag is immutable: fix the signer and ship the next patch"
log "tag ${version} -> ${sha:0:12} (GitHub-verified signature)"

# --- 6. Watch -------------------------------------------------------------------------

if [[ "${watch}" == false ]]; then
  log "shipped ${version} at ${sha:0:12} in ${duration}; not watching (--no-watch)"
  exit 0
fi
run_id=''
deadline=$((SECONDS + 300))
while [[ -z "${run_id}" ]]; do
  run_id="$(gh run list -R "${repo}" --workflow release.yml --limit 20 \
    --json databaseId,headSha,headBranch 2>/dev/null |
    jq -r --arg sha "${sha}" --arg tag "${version}" \
      '[.[] | select(.headSha == $sha and .headBranch == $tag)] | first | .databaseId // ""')"
  [[ -n "${run_id}" ]] && break
  [[ ${SECONDS} -lt ${deadline} ]] || fail "no release.yml run appeared for ${version} within 5 minutes"
  sleep 10
done
log "watching release.yml run ${run_id} (worker-image.yml and e2b-template.yml run alongside)"
gh run watch "${run_id}" -R "${repo}" --exit-status --interval 30 >/dev/null ||
  fail "release.yml run ${run_id} for ${version} did not succeed"
published="$(gh api "repos/${repo}/releases/tags/${version}" | jq -r 'select(.draft == false) | .tag_name // ""')"
[[ "${published}" == "${version}" ]] || fail "${repo} ${version} is missing or still a draft"
log "${repo} ${version} is published"
deadline=$((SECONDS + 600))
until gh api "repos/${tap_repo}/contents/${cask_path}" 2>/dev/null | jq -r '.content' | base64 --decode 2>/dev/null |
  grep -q "version \"${version#v}\""; do
  [[ ${SECONDS} -lt ${deadline} ]] || fail "${tap_repo}/${cask_path} did not move to ${version} within 10 minutes"
  sleep 20
done
log "${tap_repo}/${cask_path} is at ${version}"
log "shipped ${version} at ${sha:0:12}; total $(((SECONDS - started) / 60))m$(((SECONDS - started) % 60))s"
