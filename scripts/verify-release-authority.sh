#!/usr/bin/env bash
# Verify release-tag policy before any release build starts. Normal workflow
# mode is structural and works with a contents:read GITHUB_TOKEN. The separate
# --audit-policy mode requires administrator-visible bypass fields. This script
# is read-only and never creates or updates a GitHub ruleset.

set -euo pipefail

readonly numeric_identifier='(0|[1-9][0-9]*)'
readonly nonnumeric_identifier='[0-9A-Za-z-]*[A-Za-z-][0-9A-Za-z-]*'
readonly prerelease_identifier="(${numeric_identifier}|${nonnumeric_identifier})"
readonly release_tag_pattern="^v${numeric_identifier}\.${numeric_identifier}\.${numeric_identifier}(-${prerelease_identifier}(\.${prerelease_identifier})*)?$"

usage() {
  printf 'usage: %s <owner/repository> <tag>\n' "$0" >&2
  printf '       %s <owner/repository> <tag> --json-file <rulesets> <tag-ref> <tag-object>\n' "$0" >&2
  printf '       %s --check-actor-visibility <owner/repository> [--json-file <rulesets>]\n' "$0" >&2
  printf '       %s --audit-policy <owner/repository> [--json-file <rulesets>]\n' "$0" >&2
  printf '       %s --self-test\n' "$0" >&2
}

die() {
  printf '::error::release-authority: %s\n' "$*" >&2
  exit 1
}

structural_counts() {
  local repository=$1
  local rulesets=$2
  jq -c --arg repository "${repository}" '
    def scoped:
      .source == $repository
      and .source_type == "Repository"
      and .target == "tag"
      and .enforcement == "active"
      and .conditions.ref_name.include == ["refs/tags/v*"]
      and .conditions.ref_name.exclude == [];
    def rule_types: [(.rules // [])[]?.type] | sort;
    def immutable: scoped and rule_types == ["deletion", "non_fast_forward", "update"];
    def creation: scoped and rule_types == ["creation"];
    {
      scoped: ([.[] | select(scoped)] | length),
      immutability: ([.[] | select(immutable)] | length),
      creation: ([.[] | select(creation)] | length)
    }
  ' <<<"${rulesets}"
}

audit_counts() {
  local repository=$1
  local rulesets=$2
  jq -c --arg repository "${repository}" '
    def scoped:
      .source == $repository
      and .source_type == "Repository"
      and .target == "tag"
      and .enforcement == "active"
      and .conditions.ref_name.include == ["refs/tags/v*"]
      and .conditions.ref_name.exclude == [];
    def rule_types: [(.rules // [])[]?.type] | sort;
    def immutable:
      scoped
      and rule_types == ["deletion", "non_fast_forward", "update"]
      and .bypass_actors == []
      and .current_user_can_bypass == "never";
    def creation:
      scoped
      and rule_types == ["creation"]
      and ((.bypass_actors // []) | length) == 1
      and .bypass_actors[0].actor_type == "OrganizationAdmin"
      and .bypass_actors[0].bypass_mode == "always"
      and (.bypass_actors[0].actor_id // null) == null
      and .current_user_can_bypass == "always";
    {
      scoped: ([.[] | select(scoped)] | length),
      immutability: ([.[] | select(immutable)] | length),
      creation: ([.[] | select(creation)] | length)
    }
  ' <<<"${rulesets}"
}

verify_counts() {
  local label=$1
  local counts=$2
  local scoped
  local creation
  local immutability

  scoped=$(jq -r '.scoped' <<<"${counts}")
  creation=$(jq -r '.creation' <<<"${counts}")
  immutability=$(jq -r '.immutability' <<<"${counts}")
  if [[ "${scoped}" -ne 2 || "${creation}" -ne 1 || "${immutability}" -ne 1 ]]; then
    printf 'release-authority: RED (%s): expected exactly two active refs/tags/v* rulesets, one creation-only and one immutable\n' "${label}" >&2
    printf 'release-authority: observed scoped=%s creation=%s immutability=%s\n' "${scoped}" "${creation}" "${immutability}" >&2
    return 1
  fi
}

verify_rulesets() {
  local repository=$1
  local rulesets=$2

  if ! jq -e 'type == "array"' >/dev/null 2>&1 <<<"${rulesets}"; then
    printf 'release-authority: ruleset readback is not a JSON array\n' >&2
    return 1
  fi
  verify_counts structural "$(structural_counts "${repository}" "${rulesets}")" || {
    jq -r '.[] | "  observed: source=\(.source // "<missing>") target=\(.target // "<missing>") enforcement=\(.enforcement // "<missing>") include=\(.conditions.ref_name.include // []) exclude=\(.conditions.ref_name.exclude // []) rules=\([(.rules // [])[]?.type] | join(",")) bypass_fields=\(if has("bypass_actors") then "visible" else "omitted" end)"' <<<"${rulesets}" >&2
    return 1
  }
  printf 'release-authority: structural rulesets GREEN: exact creation and immutability scopes verified\n'
}

audit_policy() {
  local repository=$1
  local rulesets=$2

  if ! jq -e 'type == "array"' >/dev/null 2>&1 <<<"${rulesets}"; then
    printf 'release-authority: ruleset readback is not a JSON array\n' >&2
    return 1
  fi
  verify_counts audit "$(audit_counts "${repository}" "${rulesets}")" || {
    printf 'release-authority: audit requires exact bypass_actors and current_user_can_bypass fields\n' >&2
    return 1
  }

  printf 'release-authority: audit GREEN: no-bypass immutability and sole OrganizationAdmin(always) creation verified\n'
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

verify_actor_visibility() {
  local repository=$1
  local rulesets=$2
  local repository_rulesets
  local missing_structural
  local missing_actor_fields

  if ! jq -e 'type == "array"' >/dev/null 2>&1 <<<"${rulesets}"; then
    printf 'release-authority: ruleset readback is not a JSON array\n' >&2
    return 1
  fi
  repository_rulesets=$(jq --arg repository "${repository}" \
    '[.[] | select(.source == $repository and .source_type == "Repository")] | length' <<<"${rulesets}")
  if [[ "${repository_rulesets}" -lt 1 ]]; then
    printf 'release-authority visibility: RED: no repository ruleset was visible to github.token\n' >&2
    return 1
  fi
  missing_structural=$(jq --arg repository "${repository}" '
    [.[]
      | select(.source == $repository and .source_type == "Repository")
      | select((has("target") | not) or (has("enforcement") | not) or (has("conditions") | not) or (has("rules") | not))
    ] | length
  ' <<<"${rulesets}")
  if [[ "${missing_structural}" -ne 0 ]]; then
    printf 'release-authority visibility: RED: structural fields missing on %s repository ruleset(s)\n' "${missing_structural}" >&2
    return 1
  fi
  missing_actor_fields=$(jq --arg repository "${repository}" '
    [.[]
      | select(.source == $repository and .source_type == "Repository")
      | select((has("bypass_actors") | not) or (has("current_user_can_bypass") | not))
    ] | length
  ' <<<"${rulesets}")
  if [[ "${missing_actor_fields}" -gt 0 ]]; then
    printf 'release-authority visibility: GREEN: structural fields visible; actor fields omitted on %s/%s ruleset(s), so administrator audit remains required\n' \
      "${missing_actor_fields}" "${repository_rulesets}"
  else
    printf 'release-authority visibility: GREEN: structural and actor fields visible on %s repository ruleset(s)\n' "${repository_rulesets}"
  fi
}

self_test() {
  local repository='example/release-repo'
  local tag='v1.2.3'
  local structural_creation
  local structural_immutability
  local structural_rulesets
  local audit_creation
  local audit_immutability
  local audit_rulesets
  local valid_ref
  local valid_object
  local pass=0
  local fail=0

  structural_creation='{"source":"example/release-repo","source_type":"Repository","target":"tag","enforcement":"active","conditions":{"ref_name":{"include":["refs/tags/v*"],"exclude":[]}},"rules":[{"type":"creation"}]}'
  structural_immutability='{"source":"example/release-repo","source_type":"Repository","target":"tag","enforcement":"active","conditions":{"ref_name":{"include":["refs/tags/v*"],"exclude":[]}},"rules":[{"type":"deletion"},{"type":"update"},{"type":"non_fast_forward"}]}'
  structural_rulesets=$(jq -cn --argjson creation "${structural_creation}" --argjson immutability "${structural_immutability}" '[ $creation, $immutability ]')
  audit_creation=$(jq -c '. + {bypass_actors:[{actor_id:null,actor_type:"OrganizationAdmin",bypass_mode:"always"}],current_user_can_bypass:"always"}' <<<"${structural_creation}")
  audit_immutability=$(jq -c '. + {bypass_actors:[],current_user_can_bypass:"never"}' <<<"${structural_immutability}")
  audit_rulesets=$(jq -cn --argjson creation "${audit_creation}" --argjson immutability "${audit_immutability}" '[ $creation, $immutability ]')
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

  expect_audit_pass() {
    local name=$1 rulesets=$2
    if audit_policy "${repository}" "${rulesets}" >/dev/null 2>&1; then
      pass=$((pass + 1))
    else
      printf 'self-test FAIL: valid audit fixture rejected: %s\n' "${name}" >&2
      fail=$((fail + 1))
    fi
  }

  expect_audit_red() {
    local name=$1 rulesets=$2
    if audit_policy "${repository}" "${rulesets}" >/dev/null 2>&1; then
      printf 'self-test FAIL: unsafe audit fixture accepted: %s\n' "${name}" >&2
      fail=$((fail + 1))
    else
      pass=$((pass + 1))
    fi
  }

  expect_pass 'authorized exact signed tag with actor fields omitted' "${structural_rulesets}" "${valid_ref}" "${valid_object}"
  expect_red 'current absence' '[]' "${valid_ref}" "${valid_object}"
  expect_red 'missing creation structure' "[$structural_immutability]" "${valid_ref}" "${valid_object}"
  expect_red 'missing immutability structure' "[$structural_creation]" "${valid_ref}" "${valid_object}"
  expect_red 'combined rule types' \
    "$(jq -cn --argjson creation "${structural_creation}" '$creation | .rules += [{"type":"deletion"},{"type":"update"},{"type":"non_fast_forward"}] | [.]')" \
    "${valid_ref}" "${valid_object}"
  expect_red 'excluded release scope' \
    "$(jq '.[1].conditions.ref_name.exclude=["refs/tags/v*"]' <<<"${structural_rulesets}")" \
    "${valid_ref}" "${valid_object}"
  expect_red 'extra include scope' \
    "$(jq '.[1].conditions.ref_name.include += ["refs/tags/legacy*"]' <<<"${structural_rulesets}")" \
    "${valid_ref}" "${valid_object}"
  expect_red 'extra scoped ruleset' "$(jq '. + [.[1]]' <<<"${structural_rulesets}")" "${valid_ref}" "${valid_object}"
  expect_red 'evaluate-only creation rule' "$(jq '.[0].enforcement="evaluate"' <<<"${structural_rulesets}")" "${valid_ref}" "${valid_object}"
  expect_pass 'structural mode ignores unavailable actor policy' \
    "$(jq '.[0].bypass_actors=[{"actor_id":5,"actor_type":"RepositoryRole","bypass_mode":"always"}]' <<<"${structural_rulesets}")" \
    "${valid_ref}" "${valid_object}"
  expect_audit_pass 'exact two-ruleset policy' "${audit_rulesets}"
  expect_audit_pass 'exact rule sets in alternate order' \
    "$(jq '.[1].rules |= reverse' <<<"${audit_rulesets}")"
  expect_audit_red 'wrong creation actor' \
    "$(jq '.[0].bypass_actors=[{"actor_id":5,"actor_type":"RepositoryRole","bypass_mode":"always"}]' <<<"${audit_rulesets}")"
  expect_audit_red 'extra creation actor' \
    "$(jq '.[0].bypass_actors += [{"actor_id":5,"actor_type":"RepositoryRole","bypass_mode":"always"}]' <<<"${audit_rulesets}")"
  expect_audit_red 'creation operator cannot bypass' \
    "$(jq '.[0].current_user_can_bypass="never"' <<<"${audit_rulesets}")"
  expect_audit_red 'immutability bypass' \
    "$(jq '.[1].bypass_actors=[{"actor_id":null,"actor_type":"OrganizationAdmin","bypass_mode":"always"}]' <<<"${audit_rulesets}")"
  expect_audit_red 'immutability current user can bypass' \
    "$(jq '.[1].current_user_can_bypass="always"' <<<"${audit_rulesets}")"
  expect_audit_red 'audit excluded release scope' \
    "$(jq '.[0].conditions.ref_name.exclude=["refs/tags/v*"]' <<<"${audit_rulesets}")"
  expect_audit_red 'audit extra include scope' \
    "$(jq '.[0].conditions.ref_name.include += ["refs/tags/legacy*"]' <<<"${audit_rulesets}")"
  expect_red 'lightweight tag' "${structural_rulesets}" \
    '{"ref":"refs/tags/v1.2.3","object":{"type":"commit","sha":"commit-sha"}}' "${valid_object}"
  expect_red 'unsigned annotated tag' "${structural_rulesets}" "${valid_ref}" \
    '{"sha":"tag-object-sha","tag":"v1.2.3","object":{"type":"commit","sha":"commit-sha"},"verification":{"verified":false,"reason":"unsigned"}}'
  expect_red 'tag object mismatch' "${structural_rulesets}" "${valid_ref}" \
    '{"sha":"different-object-sha","tag":"v1.2.3","object":{"type":"commit","sha":"commit-sha"},"verification":{"verified":true,"reason":"valid"}}'
  if verify_actor_visibility "${repository}" \
    '[{"source":"example/release-repo","source_type":"Repository","target":"branch","enforcement":"active","conditions":{"ref_name":{"include":["refs/heads/main"],"exclude":[]}},"rules":[{"type":"non_fast_forward"}]}]' >/dev/null 2>&1; then
    pass=$((pass + 1))
  else
    printf 'self-test FAIL: actor omission visibility fixture rejected\n' >&2
    fail=$((fail + 1))
  fi

  printf 'verify-release-authority self-test: %s passed, %s failed\n' "${pass}" "${fail}"
  [[ "${fail}" -eq 0 ]]
}

if [[ "${1:-}" == '--self-test' ]]; then
  command -v jq >/dev/null 2>&1 || die 'jq is required for fixture self-tests'
  self_test
  exit 0
fi

if [[ "${1:-}" == '--check-actor-visibility' || "${1:-}" == '--audit-policy' ]]; then
  mode=${1#--}
  repository=${2:-}
  [[ "${repository}" =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ ]] || { usage; exit 2; }
  command -v jq >/dev/null 2>&1 || die 'jq is required'
  if [[ "${3:-}" == '--json-file' ]]; then
    [[ $# -eq 4 ]] || { usage; exit 2; }
    rulesets=$(<"$4") || die "could not read ruleset fixture $4"
  else
    [[ $# -eq 2 ]] || { usage; exit 2; }
    command -v gh >/dev/null 2>&1 || die 'gh is required for live authority readback'
    [[ -n "${GH_TOKEN:-}" ]] || die 'GH_TOKEN is required; refusing an unauthenticated authority readback'
    rulesets=$(read_live_rulesets "${repository}")
  fi
  case "${mode}" in
    check-actor-visibility) verify_actor_visibility "${repository}" "${rulesets}" ;;
    audit-policy) audit_policy "${repository}" "${rulesets}" ;;
  esac
  exit
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
