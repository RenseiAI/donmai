#!/usr/bin/env bash
# Decide whether a release run may skip its remote pre-publish gates.
#
# Two keys, both required:
#   1. the fast-lane switch: the FAST_LANE environment variable (the workflow
#      passes the Actions variable of the same name) equals exactly `on`; and
#   2. the attestation: the latest `local-verify` commit status on the tag's
#      exact commit has state `success`.
#
# Anything else, including a missing switch, a missing or non-success status,
# or any readback error, decides `skip_remote_gates=false`, which is the fully
# gated path. This script never fails a release on its own: it only ever
# narrows the answer back to "run the gates". Tag authority, signing,
# notarization, provenance, and every publisher are outside its reach.
#
# The attestation is a record, not a proof: any writer on the repository can
# post a commit status. The trust anchor is the credential that landed the
# commit; this script only reads what that path recorded.

set -euo pipefail

readonly attestation_context='local-verify'

usage() {
  printf 'usage: %s <owner/repository> <tag> <output-file>\n' "$0" >&2
  printf '       %s <owner/repository> <tag> <output-file> --json-file <tag-ref> <tag-object> <statuses>\n' "$0" >&2
  printf '       %s --self-test\n' "$0" >&2
}

# decide <switch> <tag> <tag-ref> <tag-object> <statuses>
# Prints `skip_remote_gates=`, `commit=`, and `reason=` lines, then a
# `summary=` line with the attestation detail (never used as an output value).
decide() {
  local switch=$1
  local tag=$2
  local tag_ref=$3
  local tag_object=$4
  local statuses=$5
  local commit=''
  local latest

  if [[ "${switch}" != on ]]; then
    printf 'skip_remote_gates=false\ncommit=\nreason=fast-lane switch is off\n'
    return
  fi

  if jq -e --arg ref "refs/tags/${tag}" '
       type == "object" and .ref == $ref and .object.type == "tag"
     ' >/dev/null 2>&1 <<<"${tag_ref}" &&
     jq -e --arg tag "${tag}" --arg object_sha "$(jq -r '.object.sha // ""' <<<"${tag_ref}")" '
       type == "object" and .sha == $object_sha and .tag == $tag
       and .object.type == "commit"
       and (.object.sha | type == "string" and test("^[0-9a-f]{40}$"))
     ' >/dev/null 2>&1 <<<"${tag_object}"; then
    commit=$(jq -r '.object.sha' <<<"${tag_object}")
  else
    printf 'skip_remote_gates=false\ncommit=\nreason=could not resolve the commit of annotated tag %s\n' "${tag}"
    return
  fi

  if ! jq -e 'type == "array"' >/dev/null 2>&1 <<<"${statuses}"; then
    printf 'skip_remote_gates=false\ncommit=%s\nreason=commit status readback failed\n' "${commit}"
    return
  fi
  # The statuses API lists newest first; the first entry for the context is
  # its current state.
  latest=$(jq -c --arg context "${attestation_context}" \
    'map(select(type == "object" and .context == $context)) | first // empty' <<<"${statuses}")
  if [[ -z "${latest}" ]]; then
    printf 'skip_remote_gates=false\ncommit=%s\nreason=no %s status on the tagged commit\n' "${commit}" "${attestation_context}"
    return
  fi
  if [[ "$(jq -r '.state // ""' <<<"${latest}")" != success ]]; then
    printf 'skip_remote_gates=false\ncommit=%s\nreason=%s is %s on the tagged commit\n' \
      "${commit}" "${attestation_context}" "$(jq -r '.state // "missing"' <<<"${latest}")"
    return
  fi

  printf 'skip_remote_gates=true\ncommit=%s\nreason=fast-lane switch is on and %s succeeded on the tagged commit\n' \
    "${commit}" "${attestation_context}"
  # One line, table-safe: the description is free text from the status writer.
  jq -r '"summary=" + ([
      (.description // ""),
      ("posted by " + (.creator.login // "unknown")),
      ("at " + (.updated_at // .created_at // "unknown"))
    ] | join(" / ") | gsub("[\r\n|`]"; " "))' <<<"${latest}"
}

write_outputs() {
  local decision=$1
  local output_file=$2
  local skip
  local commit
  local reason
  local summary

  skip=$(sed -n 's/^skip_remote_gates=//p' <<<"${decision}")
  commit=$(sed -n 's/^commit=//p' <<<"${decision}")
  reason=$(sed -n 's/^reason=//p' <<<"${decision}")
  summary=$(sed -n 's/^summary=//p' <<<"${decision}")

  {
    printf 'skip_remote_gates=%s\n' "${skip}"
    printf 'commit=%s\n' "${commit}"
  } >>"${output_file}"

  printf 'release fast-lane: skip_remote_gates=%s (%s)\n' "${skip}" "${reason}"
  if [[ "${skip}" == true ]]; then
    printf '::notice::Fast lane: remote pre-publish gates skipped for %s (%s)\n' "${commit}" "${summary}"
  fi
  if [[ -n "${GITHUB_STEP_SUMMARY:-}" ]]; then
    {
      printf '### Release fast lane\n\n'
      printf '| field | value |\n| --- | --- |\n'
      printf '| remote gates | %s |\n' "$([[ "${skip}" == true ]] && echo 'skipped (attested)' || echo 'required')"
      printf '| reason | %s |\n' "${reason}"
      printf '| commit | %s |\n' "\`${commit:-unresolved}\`"
      if [[ -n "${summary}" ]]; then
        printf '| %s | %s |\n' "${attestation_context}" "${summary}"
      fi
    } >>"${GITHUB_STEP_SUMMARY}"
  fi
}

self_test() {
  local tag='v1.2.3'
  local commit='0123456789abcdef0123456789abcdef01234567'
  local ref
  local object
  local attested
  local pass=0
  local fail=0

  ref='{"ref":"refs/tags/v1.2.3","object":{"type":"tag","sha":"tag-object-sha"}}'
  object="{\"sha\":\"tag-object-sha\",\"tag\":\"v1.2.3\",\"object\":{\"type\":\"commit\",\"sha\":\"${commit}\"}}"
  attested='[{"context":"local-verify","state":"success","description":"host a / fmt lint test build / 9m","creator":{"login":"operator"},"updated_at":"2026-01-01T00:00:00Z"},{"context":"local-verify","state":"pending"}]'

  expect() {
    local name=$1 want=$2 switch=$3 tag_ref=$4 tag_object=$5 statuses=$6
    local got
    got=$(decide "${switch}" "${tag}" "${tag_ref}" "${tag_object}" "${statuses}" | sed -n 's/^skip_remote_gates=//p')
    if [[ "${got}" == "${want}" ]]; then
      pass=$((pass + 1))
    else
      printf 'self-test FAIL: %s: skip_remote_gates=%s, want %s\n' "${name}" "${got}" "${want}" >&2
      fail=$((fail + 1))
    fi
  }

  expect 'switch on and attested' true on "${ref}" "${object}" "${attested}"
  expect 'switch unset' false '' "${ref}" "${object}" "${attested}"
  expect 'switch off' false off "${ref}" "${object}" "${attested}"
  expect 'switch spelled differently' false ON "${ref}" "${object}" "${attested}"
  expect 'switch true is not on' false true "${ref}" "${object}" "${attested}"
  expect 'no statuses' false on "${ref}" "${object}" '[]'
  expect 'only other contexts succeed' false on "${ref}" "${object}" \
    '[{"context":"ci/test","state":"success"},{"context":"local-verify-extra","state":"success"}]'
  expect 'latest attestation failed after an older success' false on "${ref}" "${object}" \
    '[{"context":"local-verify","state":"failure"},{"context":"local-verify","state":"success"}]'
  expect 'latest attestation pending' false on "${ref}" "${object}" '[{"context":"local-verify","state":"pending"}]'
  expect 'latest attestation errored' false on "${ref}" "${object}" '[{"context":"local-verify","state":"error"}]'
  expect 'newest success after older failure' true on "${ref}" "${object}" \
    '[{"context":"ci/test","state":"failure"},{"context":"local-verify","state":"success"},{"context":"local-verify","state":"failure"}]'
  expect 'status readback failed' false on "${ref}" "${object}" 'not json'
  expect 'status readback not a list' false on "${ref}" "${object}" '{"message":"Not Found"}'
  expect 'lightweight tag' false on \
    "{\"ref\":\"refs/tags/v1.2.3\",\"object\":{\"type\":\"commit\",\"sha\":\"${commit}\"}}" '{}' "${attested}"
  expect 'tag ref for another tag' false on \
    '{"ref":"refs/tags/v1.2.4","object":{"type":"tag","sha":"tag-object-sha"}}' "${object}" "${attested}"
  expect 'tag object mismatch' false on "${ref}" \
    "{\"sha\":\"other-object-sha\",\"tag\":\"v1.2.3\",\"object\":{\"type\":\"commit\",\"sha\":\"${commit}\"}}" "${attested}"
  expect 'tag object names another tag' false on "${ref}" \
    "{\"sha\":\"tag-object-sha\",\"tag\":\"v1.2.4\",\"object\":{\"type\":\"commit\",\"sha\":\"${commit}\"}}" "${attested}"
  expect 'tag ref readback failed' false on '' "${object}" "${attested}"

  local output_file
  local summary_file
  output_file=$(mktemp)
  summary_file=$(mktemp)
  GITHUB_STEP_SUMMARY="${summary_file}" write_outputs \
    "$(decide on "${tag}" "${ref}" "${object}" '[{"context":"local-verify","state":"success","description":"a|b`c"}]')" \
    "${output_file}" >/dev/null
  if grep -Fqx 'skip_remote_gates=true' "${output_file}" &&
     grep -Fqx "commit=${commit}" "${output_file}" &&
     [[ "$(wc -l <"${output_file}" | tr -d '[:space:]')" == 2 ]] &&
     grep -Fq '| local-verify | a b c / posted by unknown / at unknown |' "${summary_file}"; then
    pass=$((pass + 1))
  else
    printf 'self-test FAIL: outputs or summary are malformed\n' >&2
    fail=$((fail + 1))
  fi
  rm -f "${output_file}" "${summary_file}"

  printf 'release-fast-lane self-test: %s passed, %s failed\n' "${pass}" "${fail}"
  [[ "${fail}" -eq 0 ]]
}

command -v jq >/dev/null 2>&1 || {
  printf '::error::release-fast-lane: jq is required\n' >&2
  exit 1
}

if [[ "${1:-}" == '--self-test' ]]; then
  self_test
  exit
fi

repository=${1:-}
tag=${2:-}
output_file=${3:-}
[[ "${repository}" =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ && -n "${tag}" && -n "${output_file}" ]] || {
  usage
  exit 2
}

if [[ "${4:-}" == '--json-file' ]]; then
  [[ $# -eq 7 ]] || {
    usage
    exit 2
  }
  tag_ref=$(<"$5")
  tag_object=$(<"$6")
  statuses=$(<"$7")
elif [[ $# -eq 3 ]]; then
  tag_ref=''
  tag_object=''
  statuses=''
  # Only the switch-on path reads anything; switch-off makes no API call.
  if [[ "${FAST_LANE:-}" == on ]]; then
    tag_ref=$(gh api --header 'Accept: application/vnd.github+json' \
      "repos/${repository}/git/ref/tags/${tag}" 2>/dev/null) || tag_ref=''
    tag_object_sha=$(jq -r '.object.sha // empty' <<<"${tag_ref:-null}" 2>/dev/null || true)
    if [[ -n "${tag_object_sha}" ]]; then
      tag_object=$(gh api --header 'Accept: application/vnd.github+json' \
        "repos/${repository}/git/tags/${tag_object_sha}" 2>/dev/null) || tag_object=''
    fi
    commit=$(jq -r '.object.sha // empty' <<<"${tag_object:-null}" 2>/dev/null || true)
    if [[ "${commit}" =~ ^[0-9a-f]{40}$ ]]; then
      statuses=$(gh api --paginate --slurp --header 'Accept: application/vnd.github+json' \
        "repos/${repository}/commits/${commit}/statuses?per_page=100" 2>/dev/null | jq -c 'add // []') || statuses=''
    fi
  fi
else
  usage
  exit 2
fi

write_outputs "$(decide "${FAST_LANE:-}" "${tag}" "${tag_ref}" "${tag_object}" "${statuses}")" "${output_file}"
