#!/usr/bin/env bash
# Verify release-tag policy before any release build starts. Normal workflow
# mode is structural and works with a contents:read GITHUB_TOKEN. The separate
# --audit-policy mode requires administrator-visible bypass fields. This script
# is read-only and never creates or updates a GitHub ruleset.
#
# Release identity pins (read from the environment; the workflows pass the
# repository or organization Actions variables of the same names):
#
#   RELEASE_TAG_CREATOR   the sole bypass actor of the creation-only ruleset:
#                         `OrganizationAdmin` (the default) or `Team:<id>` for a
#                         dedicated tagging identity's team.
#   RELEASE_TAGGER_EMAIL  the tagger email every release tag must carry.
#   RELEASE_TAG_SIGNERS   OpenSSH allowed-signers lines (public keys only) for
#                         the keys allowed to sign release tags. Keep the
#                         previous key's line through one rotation so a retry of
#                         an older tag still verifies.
#
# With no pins set the policy is the transitional one: an OrganizationAdmin
# creation bypass and any signature GitHub verifies. Once a dedicated creator
# is pinned, the tagger email and signer pins are mandatory; an email pin and a
# signer pin must always be set together. A half-configured identity fails
# closed instead of silently accepting any GitHub-verified signer. GitHub's own
# verification stays required in every mode; the pins narrow it to one identity.

set -euo pipefail

readonly numeric_identifier='(0|[1-9][0-9]*)'
readonly nonnumeric_identifier='[0-9A-Za-z-]*[A-Za-z-][0-9A-Za-z-]*'
readonly prerelease_identifier="(${numeric_identifier}|${nonnumeric_identifier})"
readonly release_tag_pattern="^v${numeric_identifier}\.${numeric_identifier}\.${numeric_identifier}(-${prerelease_identifier}(\.${prerelease_identifier})*)?$"

release_signature_namespace='git'

# Whitespace-only values count as unset, so an empty multi-line Actions
# variable cannot masquerade as a configured pin.
pin_value() {
  if [[ "$1" =~ [^[:space:]] ]]; then
    printf '%s' "$1"
  fi
}

creator_pin=$(pin_value "${RELEASE_TAG_CREATOR:-}")
creator_pin=${creator_pin:-OrganizationAdmin}
tagger_email_pin=$(pin_value "${RELEASE_TAGGER_EMAIL:-}")
tag_signers_pin=$(pin_value "${RELEASE_TAG_SIGNERS:-}")
creator_actor_type=''
creator_actor_id='null'

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

# Parse and cross-check the identity pins. Sets creator_actor_type and
# creator_actor_id for the ruleset filters below.
validate_identity_pins() {
  case "${creator_pin}" in
    OrganizationAdmin)
      creator_actor_type='OrganizationAdmin'
      creator_actor_id='null'
      ;;
    Team:*)
      if [[ ! "${creator_pin#Team:}" =~ ^[1-9][0-9]*$ ]]; then
        printf 'release-authority: RED: RELEASE_TAG_CREATOR must be OrganizationAdmin or Team:<numeric id>, got %s\n' "${creator_pin}" >&2
        return 1
      fi
      creator_actor_type='Team'
      creator_actor_id=${creator_pin#Team:}
      ;;
    *)
      printf 'release-authority: RED: RELEASE_TAG_CREATOR must be OrganizationAdmin or Team:<numeric id>, got %s\n' "${creator_pin}" >&2
      return 1
      ;;
  esac

  if [[ -n "${tagger_email_pin}" && -z "${tag_signers_pin}" ]] ||
     [[ -z "${tagger_email_pin}" && -n "${tag_signers_pin}" ]]; then
    printf 'release-authority: RED: tagger identity is half-configured; set RELEASE_TAGGER_EMAIL and RELEASE_TAG_SIGNERS together\n' >&2
    return 1
  fi
  if [[ "${creator_actor_type}" != OrganizationAdmin && -z "${tagger_email_pin}" ]]; then
    printf 'release-authority: RED: RELEASE_TAG_CREATOR pins %s but the tagger email and signer pins are empty; refusing to accept any GitHub-verified signer\n' "${creator_pin}" >&2
    return 1
  fi
}

structural_counts() {
  local repository=$1
  local rulesets=$2
  jq -c --arg repository "${repository}" \
    --arg actor_type "${creator_actor_type}" \
    --argjson actor_id "${creator_actor_id}" '
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
      and ((has("bypass_actors") | not) or .bypass_actors == [])
      and ((has("current_user_can_bypass") | not) or .current_user_can_bypass == "never");
    def pinned_creator:
      ((.bypass_actors // []) | length) == 1
      and .bypass_actors[0].actor_type == $actor_type
      and .bypass_actors[0].bypass_mode == "always"
      and (.bypass_actors[0].actor_id // null) == $actor_id;
    def creation:
      scoped
      and rule_types == ["creation"]
      and ((has("bypass_actors") | not) or pinned_creator)
      and ((has("current_user_can_bypass") | not) or .current_user_can_bypass == "never");
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
  jq -c --arg repository "${repository}" \
    --arg actor_type "${creator_actor_type}" \
    --argjson actor_id "${creator_actor_id}" '
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
    # An administrator auditing the transitional OrganizationAdmin policy is
    # itself the creator, so it must read "always". A dedicated creator team
    # normally excludes the auditing administrator; the exact visible actor is
    # then the proof, and the field only has to be present.
    def auditor_bypass:
      if $actor_type == "OrganizationAdmin" then .current_user_can_bypass == "always"
      else .current_user_can_bypass == "always" or .current_user_can_bypass == "never"
      end;
    def creation:
      scoped
      and rule_types == ["creation"]
      and ((.bypass_actors // []) | length) == 1
      and .bypass_actors[0].actor_type == $actor_type
      and .bypass_actors[0].bypass_mode == "always"
      and (.bypass_actors[0].actor_id // null) == $actor_id
      and auditor_bypass;
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

  validate_identity_pins || return 1
  if ! jq -e 'type == "array"' >/dev/null 2>&1 <<<"${rulesets}"; then
    printf 'release-authority: ruleset readback is not a JSON array\n' >&2
    return 1
  fi
  verify_counts audit "$(audit_counts "${repository}" "${rulesets}")" || {
    printf 'release-authority: audit requires exact bypass_actors and current_user_can_bypass fields\n' >&2
    return 1
  }

  printf 'release-authority: audit GREEN: no-bypass immutability and sole %s(always) creation verified\n' "${creator_pin}"
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

# Check the signed tag object against the pinned tagging identity: the tagger
# email, and an OpenSSH verification of the exact signed payload GitHub returns
# against the pinned allowed signers. The payload's own header lines must name
# this tag and commit, so a signature lifted from another tag cannot pass.
verify_tagger_identity() {
  local tag=$1
  local tag_object=$2
  local commit
  local work
  local header
  local tagger_line
  local tagger_pattern='^tagger .* <([^<>]*)> [0-9]+ [-+][0-9]{4}$'
  local principals
  local principal
  local verified=false

  if [[ -z "${tagger_email_pin}" ]]; then
    printf 'release-authority: tagger identity pins unset: transitional policy accepts any GitHub-verified signer\n'
    return 0
  fi
  command -v ssh-keygen >/dev/null 2>&1 || {
    printf 'release-authority: RED: ssh-keygen is required to verify the pinned release signer\n' >&2
    return 1
  }

  if ! jq -e --arg email "${tagger_email_pin}" '.tagger.email == $email' >/dev/null <<<"${tag_object}"; then
    printf 'release-authority: RED: refs/tags/%s tagger %s is not the pinned release tagger\n' \
      "${tag}" "$(jq -r '.tagger.email // "<missing>"' <<<"${tag_object}")" >&2
    return 1
  fi
  if ! jq -e '
    (.verification.signature | type == "string" and length > 0)
    and (.verification.payload | type == "string" and length > 0)
  ' >/dev/null <<<"${tag_object}"; then
    printf 'release-authority: RED: refs/tags/%s readback carries no signature payload to check against the pinned signers\n' "${tag}" >&2
    return 1
  fi

  commit=$(jq -r '.object.sha' <<<"${tag_object}")
  work=$(mktemp -d)
  # -j keeps the bytes exact: no trailing newline is added to either value.
  jq -j '.verification.payload' <<<"${tag_object}" >"${work}/payload"
  jq -j '.verification.signature' <<<"${tag_object}" >"${work}/payload.sig"
  printf '%s\n' "${tag_signers_pin}" >"${work}/allowed_signers"

  header=$(awk 'NF == 0 { exit } { print }' "${work}/payload")
  tagger_line=$(sed -n '4p' <<<"${header}")
  if [[ "$(sed -n '1,3p' <<<"${header}")" != "$(printf 'object %s\ntype commit\ntag %s' "${commit}" "${tag}")" ]] ||
     [[ "$(wc -l <<<"${header}" | tr -d '[:space:]')" != 4 ]] ||
     [[ ! "${tagger_line}" =~ ${tagger_pattern} ]] ||
     [[ "${BASH_REMATCH[1]}" != "${tagger_email_pin}" ]]; then
    rm -rf "${work}"
    printf 'release-authority: RED: the signed payload does not name refs/tags/%s, commit %s, and the pinned tagger\n' "${tag}" "${commit}" >&2
    return 1
  fi

  if principals=$(ssh-keygen -Y find-principals -s "${work}/payload.sig" -f "${work}/allowed_signers" 2>/dev/null); then
    while IFS= read -r principal; do
      [[ -n "${principal}" ]] || continue
      if ssh-keygen -Y verify -f "${work}/allowed_signers" -I "${principal}" \
        -n "${release_signature_namespace}" -s "${work}/payload.sig" <"${work}/payload" >/dev/null 2>&1; then
        verified=true
        break
      fi
    done < <(tr ',' '\n' <<<"${principals}")
  fi
  rm -rf "${work}"

  if [[ "${verified}" != true ]]; then
    printf 'release-authority: RED: refs/tags/%s is not signed by a pinned release signing key\n' "${tag}" >&2
    return 1
  fi
  printf 'release-authority: tagger identity GREEN: %s signed by pinned signer %s\n' "${tag}" "${principal}"
}

verify_authority() {
  local repository=$1
  local tag=$2
  local rulesets=$3
  local tag_ref=$4
  local tag_object=$5

  validate_identity_pins || return 1
  verify_rulesets "${repository}" "${rulesets}" || return 1
  verify_signed_tag "${tag}" "${tag_ref}" "${tag_object}" || return 1
  verify_tagger_identity "${tag}" "${tag_object}" || return 1
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

# Tagging-identity fixtures: real OpenSSH signatures from throwaway keys over
# tag payloads shaped like GitHub's readback. Runs inside self_test and shares
# its fixtures and counters.
identity_self_test() {
  local fixture_dir
  local bot_email='release-bot@example.com'
  local other_email='someone-else@example.com'
  local pinned_signers
  local rotated_signers
  local future_signers
  local pinned_object
  local previous_object
  local unpinned_object
  local team_rulesets
  local team_audit_rulesets
  local key

  command -v ssh-keygen >/dev/null 2>&1 || {
    printf 'self-test FAIL: ssh-keygen is required for the tagging-identity fixtures\n' >&2
    fail=$((fail + 1))
    return
  }
  fixture_dir=$(mktemp -d)
  for key in pinned previous unpinned; do
    ssh-keygen -q -t ed25519 -N '' -C "${key}" -f "${fixture_dir}/${key}" </dev/null
  done
  public_key() { cut -d' ' -f1,2 "${fixture_dir}/$1.pub"; }
  pinned_signers="release-bot namespaces=\"git\" $(public_key pinned)"
  rotated_signers=$(printf 'release-bot namespaces="git",valid-after="20200101" %s\nrelease-bot namespaces="git",valid-after="20250101" %s' \
    "$(public_key previous)" "$(public_key pinned)")
  future_signers="release-bot namespaces=\"git\",valid-after=\"29990101\" $(public_key pinned)"

  # signed_object <key> <namespace> <payload-tag> <payload-commit> <payload-email> <api-email> <github-verified>
  # Called through command substitution, so each call names its own file.
  signed_object() {
    local payload_file
    payload_file=$(mktemp "${fixture_dir}/payload.XXXXXX")
    printf 'object %s\ntype commit\ntag %s\ntagger Release Bot <%s> 1700000000 +0000\n\n%s\n' "$4" "$3" "$5" "$3" >"${payload_file}"
    ssh-keygen -Y sign -f "${fixture_dir}/$1" -n "$2" "${payload_file}" </dev/null >/dev/null 2>&1
    jq -cn --rawfile payload "${payload_file}" --rawfile signature "${payload_file}.sig" \
      --arg email "$6" --argjson verified "$7" '{
        sha: "tag-object-sha",
        tag: "v1.2.3",
        object: {type: "commit", sha: "commit-sha"},
        tagger: {name: "Release Bot", email: $email},
        verification: {
          verified: $verified,
          reason: (if $verified then "valid" else "unknown_key" end),
          signature: $signature,
          payload: $payload
        }
      }'
  }
  set_pins() {
    creator_pin=$1
    tagger_email_pin=$2
    tag_signers_pin=$3
  }

  pinned_object=$(signed_object pinned git v1.2.3 commit-sha "${bot_email}" "${bot_email}" true)
  previous_object=$(signed_object previous git v1.2.3 commit-sha "${bot_email}" "${bot_email}" true)
  # A GitHub-verified tag from a key outside the pins, such as an
  # administrator's host key: accepted only by the transitional policy.
  unpinned_object=$(signed_object unpinned git v1.2.3 commit-sha "${bot_email}" "${bot_email}" true)
  team_rulesets=$(jq '.[0].bypass_actors=[{"actor_id":42,"actor_type":"Team","bypass_mode":"always"}]' <<<"${workflow_visible_rulesets}")
  team_audit_rulesets=${team_rulesets}

  set_pins 'OrganizationAdmin' '' ''
  expect_pass 'transitional policy keeps a GitHub-verified host-key tag' "${structural_rulesets}" "${valid_ref}" "${unpinned_object}"
  expect_red 'transitional policy with a visible creator team' "${team_rulesets}" "${valid_ref}" "${valid_object}"
  expect_audit_red 'transitional audit with a creator team' "${team_audit_rulesets}"

  set_pins 'Team:42' "${bot_email}" "${pinned_signers}"
  expect_pass 'pinned identity with actor fields omitted' "${structural_rulesets}" "${valid_ref}" "${pinned_object}"
  expect_pass 'pinned identity with the pinned creator team visible' "${team_rulesets}" "${valid_ref}" "${pinned_object}"
  expect_red 'pinned identity with the transitional creator visible' "${workflow_visible_rulesets}" "${valid_ref}" "${pinned_object}"
  expect_red 'pinned identity with another creator team visible' \
    "$(jq '.[0].bypass_actors[0].actor_id=43' <<<"${team_rulesets}")" "${valid_ref}" "${pinned_object}"
  expect_red 'GitHub-verified tag from an unpinned key' "${structural_rulesets}" "${valid_ref}" "${unpinned_object}"
  expect_red 'pinned key under another tagger email' "${structural_rulesets}" "${valid_ref}" \
    "$(signed_object pinned git v1.2.3 commit-sha "${other_email}" "${other_email}" true)"
  expect_red 'readback tagger differs from the signed payload' "${structural_rulesets}" "${valid_ref}" \
    "$(signed_object pinned git v1.2.3 commit-sha "${other_email}" "${bot_email}" true)"
  expect_red 'signature lifted from another tag' "${structural_rulesets}" "${valid_ref}" \
    "$(signed_object pinned git v1.2.2 commit-sha "${bot_email}" "${bot_email}" true)"
  expect_red 'signature lifted from another commit' "${structural_rulesets}" "${valid_ref}" \
    "$(signed_object pinned git v1.2.3 other-commit-sha "${bot_email}" "${bot_email}" true)"
  expect_red 'pinned key outside the git namespace' "${structural_rulesets}" "${valid_ref}" \
    "$(signed_object pinned file v1.2.3 commit-sha "${bot_email}" "${bot_email}" true)"
  expect_red 'payload altered after signing' "${structural_rulesets}" "${valid_ref}" \
    "$(jq -c '.verification.payload += "altered\n"' <<<"${pinned_object}")"
  expect_red 'signature payload absent from readback' "${structural_rulesets}" "${valid_ref}" \
    "$(jq -c 'del(.verification.signature, .verification.payload)' <<<"${pinned_object}")"
  expect_red 'pinned signature GitHub did not verify' "${structural_rulesets}" "${valid_ref}" \
    "$(signed_object pinned git v1.2.3 commit-sha "${bot_email}" "${bot_email}" false)"
  expect_audit_pass 'audit of the pinned creator team by an outside administrator' "${team_audit_rulesets}"
  expect_audit_pass 'audit of the pinned creator team by a team member' \
    "$(jq '.[0].current_user_can_bypass="always"' <<<"${team_audit_rulesets}")"
  expect_audit_red 'audit of the pinned team against the transitional creator' "${audit_rulesets}"
  expect_audit_red 'audit of the pinned team without auditor visibility' \
    "$(jq '.[0] |= del(.current_user_can_bypass)' <<<"${team_audit_rulesets}")"

  set_pins 'Team:42' "${bot_email}" "${rotated_signers}"
  expect_pass 'previous rotated key inside its window' "${structural_rulesets}" "${valid_ref}" "${previous_object}"
  expect_pass 'current key beside the previous one' "${structural_rulesets}" "${valid_ref}" "${pinned_object}"
  expect_red 'rotation pins still refuse an unpinned key' "${structural_rulesets}" "${valid_ref}" "${unpinned_object}"

  set_pins 'Team:42' "${bot_email}" "${future_signers}"
  expect_red 'pinned key before its valid-after date' "${structural_rulesets}" "${valid_ref}" "${pinned_object}"

  set_pins 'OrganizationAdmin' "${bot_email}" "${pinned_signers}"
  expect_pass 'signer pins before the creator pin moves' "${structural_rulesets}" "${valid_ref}" "${pinned_object}"
  expect_red 'signer pins refuse a host-key tag before the creator pin moves' "${structural_rulesets}" "${valid_ref}" "${unpinned_object}"

  set_pins 'Team:42' '' ''
  expect_red 'dedicated creator without tagger pins' "${structural_rulesets}" "${valid_ref}" "${pinned_object}"
  set_pins 'OrganizationAdmin' "${bot_email}" ''
  expect_red 'tagger email pin without signer pin' "${structural_rulesets}" "${valid_ref}" "${pinned_object}"
  set_pins 'OrganizationAdmin' '' "${pinned_signers}"
  expect_red 'signer pin without tagger email pin' "${structural_rulesets}" "${valid_ref}" "${pinned_object}"
  for malformed in 'Team:abc' 'Team:0' 'Team:' 'User:5' 'organizationadmin'; do
    set_pins "${malformed}" "${bot_email}" "${pinned_signers}"
    expect_red "malformed creator pin ${malformed}" "${structural_rulesets}" "${valid_ref}" "${pinned_object}"
  done

  set_pins 'OrganizationAdmin' '' ''
  rm -rf "${fixture_dir}"
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
  local workflow_visible_rulesets
  local valid_ref
  local valid_object
  local pass=0
  local fail=0

  # The fixtures below state their own pins; an operator's environment must
  # not change what the self-test proves.
  creator_pin='OrganizationAdmin'
  tagger_email_pin=''
  tag_signers_pin=''

  structural_creation='{"source":"example/release-repo","source_type":"Repository","target":"tag","enforcement":"active","conditions":{"ref_name":{"include":["refs/tags/v*"],"exclude":[]}},"rules":[{"type":"creation"}]}'
  structural_immutability='{"source":"example/release-repo","source_type":"Repository","target":"tag","enforcement":"active","conditions":{"ref_name":{"include":["refs/tags/v*"],"exclude":[]}},"rules":[{"type":"deletion"},{"type":"update"},{"type":"non_fast_forward"}]}'
  structural_rulesets=$(jq -cn --argjson creation "${structural_creation}" --argjson immutability "${structural_immutability}" '[ $creation, $immutability ]')
  audit_creation=$(jq -c '. + {bypass_actors:[{actor_id:null,actor_type:"OrganizationAdmin",bypass_mode:"always"}],current_user_can_bypass:"always"}' <<<"${structural_creation}")
  audit_immutability=$(jq -c '. + {bypass_actors:[],current_user_can_bypass:"never"}' <<<"${structural_immutability}")
  audit_rulesets=$(jq -cn --argjson creation "${audit_creation}" --argjson immutability "${audit_immutability}" '[ $creation, $immutability ]')
  workflow_visible_rulesets=$(jq '.[0].current_user_can_bypass="never"' <<<"${audit_rulesets}")
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
  expect_pass 'visible exact actor policy for workflow identity' "${workflow_visible_rulesets}" "${valid_ref}" "${valid_object}"
  expect_pass 'visible exact actor policy with alternate rule order' \
    "$(jq '.[1].rules |= reverse' <<<"${workflow_visible_rulesets}")" \
    "${valid_ref}" "${valid_object}"
  expect_red 'visible wrong creation actor' \
    "$(jq '.[0].bypass_actors=[{"actor_id":5,"actor_type":"RepositoryRole","bypass_mode":"always"}]' <<<"${workflow_visible_rulesets}")" \
    "${valid_ref}" "${valid_object}"
  expect_red 'visible extra creation actor' \
    "$(jq '.[0].bypass_actors += [{"actor_id":5,"actor_type":"RepositoryRole","bypass_mode":"always"}]' <<<"${workflow_visible_rulesets}")" \
    "${valid_ref}" "${valid_object}"
  expect_red 'visible immutability bypass' \
    "$(jq '.[1].bypass_actors=[{"actor_id":null,"actor_type":"OrganizationAdmin","bypass_mode":"always"}]' <<<"${workflow_visible_rulesets}")" \
    "${valid_ref}" "${valid_object}"
  expect_red 'visible immutability current user bypass' \
    "$(jq '.[1].current_user_can_bypass="always"' <<<"${workflow_visible_rulesets}")" \
    "${valid_ref}" "${valid_object}"
  expect_red 'visible creation current user bypass' \
    "$(jq '.[0].current_user_can_bypass="always"' <<<"${workflow_visible_rulesets}")" \
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

  identity_self_test

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
validate_identity_pins || die 'release identity pins are misconfigured; refusing to build or publish'

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
