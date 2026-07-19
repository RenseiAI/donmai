#!/usr/bin/env bash
set -euo pipefail

readonly numeric_identifier='(0|[1-9][0-9]*)'
readonly nonnumeric_identifier='[0-9A-Za-z-]*[A-Za-z-][0-9A-Za-z-]*'
readonly prerelease_identifier="(${numeric_identifier}|${nonnumeric_identifier})"
readonly release_tag_pattern="^v${numeric_identifier}\.${numeric_identifier}\.${numeric_identifier}(-${prerelease_identifier}(\.${prerelease_identifier})*)?$"

usage() {
  printf 'usage: %s --validate <tag>\n' "$0" >&2
  printf '       %s --verify <tag> <push|workflow_dispatch> <github-output> <github-env>\n' "$0" >&2
  exit 2
}

validate_release_tag() {
  local tag=$1

  if [[ ! "${tag}" =~ ${release_tag_pattern} ]]; then
    printf 'Release tag must be vMAJOR.MINOR.PATCH with an optional well-formed SemVer prerelease: %s\n' "${tag}" >&2
    return 1
  fi
}

verify_release_tag() {
  local tag=$1
  local event_name=$2
  local output_file=$3
  local env_file=$4
  local branch_ref
  local tag_commit
  local head_commit
  local make_latest
  local e2b_additional_tags

  validate_release_tag "${tag}"

  case "${event_name}" in
    push)
      make_latest=true
      e2b_additional_tags=default
      ;;
    workflow_dispatch)
      make_latest=false
      e2b_additional_tags=
      ;;
    *)
      printf 'Unsupported release event: %s\n' "${event_name}" >&2
      return 1
      ;;
  esac

  if ! git show-ref --verify --quiet "refs/tags/${tag}"; then
    printf 'Release ref must be an existing tag: %s\n' "${tag}" >&2
    return 1
  fi

  if branch_ref=$(git symbolic-ref --quiet HEAD); then
    printf 'Release checkout must be detached at %s, not branch %s\n' "${tag}" "${branch_ref}" >&2
    return 1
  fi

  tag_commit=$(git rev-parse --verify "refs/tags/${tag}^{commit}")
  head_commit=$(git rev-parse --verify 'HEAD^{commit}')
  if [[ "${tag_commit}" != "${head_commit}" ]]; then
    printf 'Checked-out commit %s does not match %s (%s)\n' "${head_commit}" "${tag}" "${tag_commit}" >&2
    return 1
  fi

  {
    printf 'tag=%s\n' "${tag}"
    printf 'commit=%s\n' "${head_commit}"
    printf 'goreleaser_make_latest=%s\n' "${make_latest}"
    printf 'e2b_template_ref=donmai-worker:%s\n' "${tag}"
    printf 'e2b_additional_tags=%s\n' "${e2b_additional_tags}"
    printf 'image_tags<<EOF\n'
    printf 'ghcr.io/renseiai/donmai-worker:%s\n' "${tag}"
    if [[ "${event_name}" == push ]]; then
      printf 'ghcr.io/renseiai/donmai-worker:latest\n'
    fi
    printf 'EOF\n'
  } >> "${output_file}"

  {
    printf 'GORELEASER_CURRENT_TAG=%s\n' "${tag}"
    printf 'GORELEASER_MAKE_LATEST=%s\n' "${make_latest}"
  } >> "${env_file}"

  printf 'Verified %s at detached commit %s\n' "${tag}" "${head_commit}"
}

case ${1:-} in
  --validate)
    [[ $# -eq 2 ]] || usage
    validate_release_tag "$2"
    ;;
  --verify)
    [[ $# -eq 5 ]] || usage
    verify_release_tag "$2" "$3" "$4" "$5"
    ;;
  *)
    usage
    ;;
esac
