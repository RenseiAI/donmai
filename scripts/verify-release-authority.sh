#!/usr/bin/env bash
# Fail closed unless release tags have separate creation authority and
# no-bypass immutability, and GitHub has cryptographically verified the tag.

set -euo pipefail

readonly numeric_identifier='(0|[1-9][0-9]*)'
readonly nonnumeric_identifier='[0-9A-Za-z-]*[A-Za-z-][0-9A-Za-z-]*'
readonly prerelease_identifier="(${numeric_identifier}|${nonnumeric_identifier})"
readonly release_tag_pattern="^v${numeric_identifier}\.${numeric_identifier}\.${numeric_identifier}(-${prerelease_identifier}(\.${prerelease_identifier})*)?$"

usage() {
  printf 'usage: %s <owner/repository> <tag>\n' "$0" >&2
  printf '       %s <owner/repository> <tag> --json-file <rulesets> <tag-ref> <tag-object>\n' "$0" >&2
  printf '       %s --self-test\n' "$0" >&2
}

die() {
  printf '::error::release-authority: %s\n' "$*" >&2
  exit 1
}

verify_rulesets() {
  local repository=$1
  local rulesets=$2
  local counts
  local creation_count
  local immutability_count

  if ! jq -e 'type == "array"' >/dev/null 2>&1 <<<"${rulesets}"; then
    printf 'release-authority: ruleset readback is not a JSON array\n' >&2
    return 1
  fi

  counts=$(jq -c --arg repository "${repository}" '
    def repository_tag_scope:
      .source == $repository
      and .source_type == "Repository"
      and .target == "tag"
      and .enforcement == "active"
      and (((.conditions.ref_name.include // []) | index("refs/tags/v*")) != null);
    def rule_types: [(.rules // [])[]?.type] | sort;
    def creation_authority:
      repository_tag_scope
      and rule_types == ["creation"]
      and ((.bypass_actors // []) | length) == 1
      and .bypass_actors[0].actor_type == "OrganizationAdmin"
      and .bypass_actors[0].bypass_mode == "always";
    def no_bypass_immutability:
      repository_tag_scope
      and rule_types == ["deletion", "non_fast_forward", "update"]
      and ((.bypass_actors // []) | length) == 0;
    {
      creation: ([.[] | select(creation_authority)] | length),
      immutability: ([.[] | select(no_bypass_immutability)] | length)
    }
  ' <<<"${rulesets}")

  creation_count=$(jq -r '.creation' <<<"${counts}")
  immutability_count=$(jq -r '.immutability' <<<"${counts}")
  if [[ "${creation_count}" -lt 1 || "${immutability_count}" -lt 1 ]]; then
    printf 'release-authority: RED: refs/tags/v* requires two active repository rulesets\n' >&2
    printf 'release-authority: creation-only ruleset with OrganizationAdmin(always): %s\n' "${creation_count}" >&2
    printf 'release-authority: no-bypass deletion/update/non_fast_forward ruleset: %s\n' "${immutability_count}" >&2
    jq -r '.[] | "  observed: source=\(.source // "<missing>") target=\(.target // "<missing>") enforcement=\(.enforcement // "<missing>") rules=\([(.rules // [])[]?.type] | sort | join(",")) bypass=\([(.bypass_actors // [])[]? | [.actor_type, .bypass_mode] | join(":")] | join(","))"' <<<"${rulesets}" >&2
    return 1
  fi

  printf 'release-authority: rulesets GREEN: creation=%s immutability=%s\n' "${creation_count}" "${immutability_count}"
}

verify_signed_tag() {
  local tag=$1
  local tag_ref=$2
  local tag_object=$3
  local expected_ref="refs/tags/${tag}"
  local ref_object_sha

  if ! jq -e 'type == "object"' >/dev/null 2>&1 <<<"${tag_ref}" ||
     ! jq -e 'type == "object"' >/dev/null 2>&1 <<<"${tag_object}"; then
    printf 'release-authority: tag readback is not a JSON object\n' >&2
    return 1
  fi

  if ! jq -e --arg expected_ref "${expected_ref}" '
    .ref == $expected_ref
    and .object.type == "tag"
    and (.object.sha | type == "string" and length > 0)
  ' >/dev/null <<<"${tag_ref}"; then
    printf 'release-authority: RED: %s is absent or lightweight; an annotated signed tag is required\n' "${expected_ref}" >&2
    return 1
  fi

  ref_object_sha=$(jq -r '.object.sha' <<<"${tag_ref}")
  if ! jq -e --arg tag "${tag}" --arg ref_object_sha "${ref_object_sha}" '
    .sha == $ref_object_sha
    and .tag == $tag
    and .object.type == "commit"
    and .verification.verified == true
    and .verification.reason == "valid"
  ' >/dev/null <<<"${tag_object}"; then
    printf 'release-authority: RED: GitHub did not verify the annotated signature for %s\n' "${expected_ref}" >&2
    return 1
  fi

  printf 'release-authority: signed tag GREEN: %s object=%s commit=%s\n' \
    "${expected_ref}" \
    "${ref_object_sha}" \
    "$(jq -r '.object.sha' <<<"${tag_object}")"
}

verify_authority() {
  local repository=$1
  local tag=$2
  local rulesets=$3
  local tag_ref=$4
  local tag_object=$5

  verify_rulesets "${repository}" "${rulesets}" || return 1
  verify_signed_tag "${tag}" "${tag_ref}" "${tag_object}" || return 1
  printf 'release-authority: GREEN: %s %s is authorized, signed, and immutable\n' "${repository}" "${tag}"
}

read_live_rulesets() {
  local repository=$1
  local summary
  local id
  local detail
  local details='[]'

  summary=$(gh api --paginate --slurp \
    --header 'Accept: application/vnd.github+json' \
    "repos/${repository}/rulesets?per_page=100" | jq 'add') ||
    die 'GitHub ruleset list readback failed; refusing to build or publish'
  while IFS= read -r id; do
    [[ -n "${id}" ]] || continue
    detail=$(gh api \
      --header 'Accept: application/vnd.github+json' \
      "repos/${repository}/rulesets/${id}") ||
      die "GitHub ruleset detail readback failed for id ${id}; refusing to build or publish"
    details=$(jq --argjson detail "${detail}" '. + [$detail]' <<<"${details}")
  done < <(jq -r '.[].id' <<<"${summary}")
  printf '%s\n' "${details}"
}

self_test() {
  local repository='example/release-repo'
  local tag='v1.2.3'
  local creation
  local immutability
  local valid_rulesets
  local valid_ref
  local valid_object
  local pass=0
  local fail=0

  creation='{"source":"example/release-repo","source_type":"Repository","target":"tag","enforcement":"active","conditions":{"ref_name":{"include":["refs/tags/v*"],"exclude":[]}},"bypass_actors":[{"actor_id":null,"actor_type":"OrganizationAdmin","bypass_mode":"always"}],"rules":[{"type":"creation"}]}'
  immutability='{"source":"example/release-repo","source_type":"Repository","target":"tag","enforcement":"active","conditions":{"ref_name":{"include":["refs/tags/v*"],"exclude":[]}},"bypass_actors":[],"rules":[{"type":"deletion"},{"type":"update"},{"type":"non_fast_forward"}]}'
  valid_rulesets=$(jq -cn --argjson creation "${creation}" --argjson immutability "${immutability}" '[ $creation, $immutability ]')
  valid_ref='{"ref":"refs/tags/v1.2.3","object":{"type":"tag","sha":"tag-object-sha"}}'
  valid_object='{"sha":"tag-object-sha","tag":"v1.2.3","object":{"type":"commit","sha":"commit-sha"},"verification":{"verified":true,"reason":"valid"}}'

  expect_pass() {
    local name=$1 rulesets=$2 tag_ref=$3 tag_object=$4
    if verify_authority "${repository}" "${tag}" "${rulesets}" "${tag_ref}" "${tag_object}" >/dev/null 2>&1; then
      pass=$((pass + 1))
    else
      printf 'self-test FAIL: valid fixture rejected: %s\n' "${name}" >&2
      fail=$((fail + 1))
    fi
  }

  expect_red() {
    local name=$1 rulesets=$2 tag_ref=$3 tag_object=$4
    if verify_authority "${repository}" "${tag}" "${rulesets}" "${tag_ref}" "${tag_object}" >/dev/null 2>&1; then
      printf 'self-test FAIL: unsafe fixture accepted: %s\n' "${name}" >&2
      fail=$((fail + 1))
    else
      pass=$((pass + 1))
    fi
  }

  expect_pass 'authorized exact signed tag' "${valid_rulesets}" "${valid_ref}" "${valid_object}"
  expect_red 'current absence' '[]' "${valid_ref}" "${valid_object}"
  expect_red 'missing creation authority' "[$immutability]" "${valid_ref}" "${valid_object}"
  expect_red 'missing immutability' "[$creation]" "${valid_ref}" "${valid_object}"
  expect_red 'combined ruleset weakens immutability bypass' \
    "$(jq -cn --argjson creation "${creation}" '$creation | .rules += [{"type":"deletion"},{"type":"update"},{"type":"non_fast_forward"}] | [.]')" \
    "${valid_ref}" "${valid_object}"
  expect_red 'immutability bypass' \
    "$(jq -cn --argjson creation "${creation}" --argjson immutability "${immutability}" '[ $creation, ($immutability | .bypass_actors = [{"actor_id":null,"actor_type":"OrganizationAdmin","bypass_mode":"always"}]) ]')" \
    "${valid_ref}" "${valid_object}"
  expect_red 'lightweight tag' "${valid_rulesets}" \
    '{"ref":"refs/tags/v1.2.3","object":{"type":"commit","sha":"commit-sha"}}' "${valid_object}"
  expect_red 'unsigned annotated tag' "${valid_rulesets}" "${valid_ref}" \
    '{"sha":"tag-object-sha","tag":"v1.2.3","object":{"type":"commit","sha":"commit-sha"},"verification":{"verified":false,"reason":"unsigned"}}'
  expect_red 'tag object mismatch' "${valid_rulesets}" "${valid_ref}" \
    '{"sha":"different-object-sha","tag":"v1.2.3","object":{"type":"commit","sha":"commit-sha"},"verification":{"verified":true,"reason":"valid"}}'

  printf 'verify-release-authority self-test: %s passed, %s failed\n' "${pass}" "${fail}"
  [[ "${fail}" -eq 0 ]]
}

if [[ "${1:-}" == '--self-test' ]]; then
  command -v jq >/dev/null 2>&1 || die 'jq is required for fixture self-tests'
  self_test
  exit 0
fi

repository=${1:-}
tag=${2:-}
[[ "${repository}" =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ && "${tag}" =~ ${release_tag_pattern} ]] || { usage; exit 2; }
command -v jq >/dev/null 2>&1 || die 'jq is required'

if [[ "${3:-}" == '--json-file' ]]; then
  [[ $# -eq 6 ]] || { usage; exit 2; }
  rulesets=$(<"$4") || die "could not read ruleset fixture $4"
  tag_ref=$(<"$5") || die "could not read tag-ref fixture $5"
  tag_object=$(<"$6") || die "could not read tag-object fixture $6"
else
  [[ $# -eq 2 ]] || { usage; exit 2; }
  command -v gh >/dev/null 2>&1 || die 'gh is required for live authority readback'
  [[ -n "${GH_TOKEN:-}" ]] || die 'GH_TOKEN is required; refusing an unauthenticated authority readback'
  rulesets=$(read_live_rulesets "${repository}")
  tag_ref=$(gh api \
    --header 'Accept: application/vnd.github+json' \
    "repos/${repository}/git/ref/tags/${tag}") ||
    die "GitHub tag ref readback failed for ${tag}; refusing to build or publish"
  if [[ "$(jq -r '.object.type // empty' <<<"${tag_ref}")" == tag ]]; then
    tag_object_sha=$(jq -r '.object.sha' <<<"${tag_ref}")
    tag_object=$(gh api \
      --header 'Accept: application/vnd.github+json' \
      "repos/${repository}/git/tags/${tag_object_sha}") ||
      die "GitHub tag object readback failed for ${tag}; refusing to build or publish"
  else
    tag_object='{}'
  fi
fi

verify_authority "${repository}" "${tag}" "${rulesets}" "${tag_ref}" "${tag_object}"
