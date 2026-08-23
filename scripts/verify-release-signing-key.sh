#!/usr/bin/env bash
# Verify the local Git SSH signing identity before a release tag is created.
# This script is read-only: it reads Git config and the active GitHub account's
# public SSH signing keys. It never prints private-key bytes and never creates,
# updates, or pushes a tag.

set -euo pipefail

verification_temp_dir=''

usage() {
  printf 'usage: %s [--verify-tag <tag>]\n' "$0" >&2
}

die() {
  printf 'release-signing-key: RED: %s\n' "$*" >&2
  exit 1
}

require_command() {
  command -v "$1" >/dev/null 2>&1 || die "$1 is required"
}

# Print one canonical OpenSSH public key (type + base64 body, no comment).
# Reject newlines before parsing so a config value cannot add an allowed-signers
# entry when the canonical key is written to the temporary trust file.
canonicalize_public_key() {
  local candidate=$1
  local key_type
  local key_body
  local ignored

  [[ -n "${candidate}" && "${candidate}" != *$'\n'* && "${candidate}" != *$'\r'* ]] || return 1
  IFS=$' \t' read -r key_type key_body ignored <<<"${candidate}"
  : "${ignored:-}"
  [[ "${key_type}" =~ ^(ssh-|ecdsa-|sk-) ]] || return 1
  [[ "${key_type}" =~ ^[A-Za-z0-9@._+-]+$ ]] || return 1
  [[ "${key_body}" =~ ^[A-Za-z0-9+/=]+$ ]] || return 1
  printf '%s %s\n' "${key_type}" "${key_body}" |
    ssh-keygen -lf - -E sha256 >/dev/null 2>&1 || return 1
  printf '%s %s\n' "${key_type}" "${key_body}"
}

resolve_key_path() {
  local configured_path=$1

  case "${configured_path}" in
    \~) [[ -n "${HOME:-}" ]] && printf '%s\n' "${HOME}" ;;
    \~/*) [[ -n "${HOME:-}" ]] && printf '%s/%s\n' "${HOME}" "${configured_path#\~/}" ;;
    \~*) return 1 ;;
    *) printf '%s\n' "${configured_path}" ;;
  esac
}

resolve_configured_public_key() {
  local configured=$1
  local key_path
  local candidate

  case "${configured}" in
    key::*)
      canonicalize_public_key "${configured#key::}" ||
        die 'user.signingkey contains an invalid literal SSH public key'
      return
      ;;
    ssh-*|ecdsa-*|sk-*)
      canonicalize_public_key "${configured}" ||
        die 'user.signingkey contains an invalid raw SSH public key'
      return
      ;;
  esac

  key_path=$(resolve_key_path "${configured}") ||
    die 'user.signingkey uses an unsupported ~user path; use an absolute path, relative path, or ~/path'
  [[ -f "${key_path}" && -r "${key_path}" ]] ||
    die 'the user.signingkey path is not a readable regular file'

  # A configured path may name either a public key or a private key. Prefer a
  # public key file (the configured file itself, then its .pub sidecar). Only
  # ask ssh-keygen to derive public material as a final, non-interactive
  # fallback. Its stdout is public-key data; stderr and private bytes stay
  # suppressed.
  IFS= read -r candidate <"${key_path}" || true
  if canonicalize_public_key "${candidate}"; then
    return
  fi
  if [[ -f "${key_path}.pub" && -r "${key_path}.pub" ]]; then
    IFS= read -r candidate <"${key_path}.pub" || true
    if canonicalize_public_key "${candidate}"; then
      return
    fi
  fi
  candidate=$(ssh-keygen -y -f "${key_path}" </dev/null 2>/dev/null) ||
    die 'could not derive a public key from user.signingkey; provide a readable public key or .pub sidecar'
  canonicalize_public_key "${candidate}" ||
    die 'ssh-keygen returned an invalid public key for user.signingkey'
}

public_key_fingerprint() {
  local public_key=$1
  local output
  local fingerprint

  output=$(printf '%s\n' "${public_key}" | ssh-keygen -lf - -E sha256 2>/dev/null) || return 1
  fingerprint=$(awk 'NR == 1 { print $2 }' <<<"${output}")
  [[ "${fingerprint}" == SHA256:* ]] || return 1
  printf '%s\n' "${fingerprint}"
}

verify_local_tag() {
  local tag=$1
  local login=$2
  local public_key=$3
  local allowed_signers

  git check-ref-format "refs/tags/${tag}" >/dev/null 2>&1 ||
    die "invalid tag name: ${tag}"
  git rev-parse --verify --quiet "refs/tags/${tag}^{tag}" >/dev/null ||
    die "refs/tags/${tag} is absent or is not an annotated tag"

  umask 077
  verification_temp_dir=$(mktemp -d) || die 'could not create a temporary allowed-signers directory'
  allowed_signers="${verification_temp_dir}/allowed_signers"
  trap 'rm -rf "${verification_temp_dir}"' EXIT
  printf '%s %s\n' "${login}" "${public_key}" >"${allowed_signers}"

  if ! git -c gpg.format=ssh \
    -c gpg.ssh.allowedSignersFile="${allowed_signers}" \
    verify-tag "refs/tags/${tag}" >/dev/null 2>&1; then
    die "refs/tags/${tag} is not signed by the registered configured SSH key"
  fi
  printf 'release-signing-key: tag GREEN: refs/tags/%s is trusted for GitHub account %s\n' \
    "${tag}" "${login}"
}

verify_tag=''
case "${1:-}" in
  '') ;;
  --verify-tag)
    [[ $# -eq 2 && -n "${2:-}" ]] || { usage; exit 2; }
    verify_tag=$2
    ;;
  *) usage; exit 2 ;;
esac

require_command git
require_command gh
require_command jq
require_command ssh-keygen

git rev-parse --is-inside-work-tree >/dev/null 2>&1 ||
  die 'run this preflight inside the release repository'

signing_format=$(git config --get gpg.format || true)
[[ "${signing_format}" == ssh ]] ||
  die 'gpg.format must be exactly ssh for release tags'

configured_signing_key=$(git config --get user.signingkey || true)
[[ -n "${configured_signing_key}" ]] ||
  die 'user.signingkey must name the release SSH signing public or private key'
configured_public_key=$(resolve_configured_public_key "${configured_signing_key}")
configured_fingerprint=$(public_key_fingerprint "${configured_public_key}") ||
  die 'could not fingerprint the configured SSH signing public key'

user_json=$(gh api --hostname github.com \
  --header 'Accept: application/vnd.github+json' \
  user 2>/dev/null) ||
  die 'could not resolve the active github.com account with gh'
login=$(jq -er '.login | select(type == "string" and length > 0)' <<<"${user_json}" 2>/dev/null) ||
  die 'GitHub user readback did not contain a login'
[[ "${login}" =~ ^[A-Za-z0-9-]+$ ]] ||
  die 'GitHub user readback contained an invalid login'

key_pages=$(gh api --hostname github.com --paginate --slurp \
  --header 'Accept: application/vnd.github+json' \
  "users/${login}/ssh_signing_keys?per_page=100" 2>/dev/null) ||
  die "could not read SSH signing keys for GitHub account ${login}"
jq -e 'type == "array" and all(.[]; type == "array")' >/dev/null 2>&1 <<<"${key_pages}" ||
  die 'GitHub SSH signing-key readback was not a paginated JSON array'
github_keys=$(jq -c 'add // []' <<<"${key_pages}") ||
  die 'could not flatten the GitHub SSH signing-key readback'
jq -e 'type == "array" and all(.[]; type == "object" and (.key | type == "string" and length > 0))' \
  >/dev/null 2>&1 <<<"${github_keys}" ||
  die 'GitHub SSH signing-key readback contained an invalid key record'

matched=false
github_key_count=$(jq 'length' <<<"${github_keys}")
index=0
while [[ "${index}" -lt "${github_key_count}" ]]; do
  github_key=$(jq -r --argjson index "${index}" '.[ $index ].key' <<<"${github_keys}")
  github_public_key=$(canonicalize_public_key "${github_key}") ||
    die 'GitHub SSH signing-key readback contained an invalid public key'
  github_fingerprint=$(public_key_fingerprint "${github_public_key}") ||
    die 'could not fingerprint a GitHub SSH signing key'
  if [[ "${github_fingerprint}" == "${configured_fingerprint}" ]]; then
    matched=true
    break
  fi
  index=$((index + 1))
done

[[ "${matched}" == true ]] ||
  die "configured SSH signing key ${configured_fingerprint} is not registered as an SSH signing key for GitHub account ${login}"

printf 'release-signing-key: GREEN: Git SSH signing key %s is registered to GitHub account %s\n' \
  "${configured_fingerprint}" "${login}"

if [[ -n "${verify_tag}" ]]; then
  verify_local_tag "${verify_tag}" "${login}" "${configured_public_key}"
fi
