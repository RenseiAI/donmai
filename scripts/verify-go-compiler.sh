#!/usr/bin/env bash
# Verify the actual Go compiler meets the floor declared in go.mod.
#
# Independent root: the check observes the real local compiler with
# GOTOOLCHAIN=local, never an auto-downloaded toolchain, so a stale go.mod
# or a silently switched toolchain cannot pass. Fails closed on missing,
# malformed, prerelease, or below-floor compilers.
#
# Usage:
#   scripts/verify-go-compiler.sh [--go-mod <path>] [--floor <x.y.z>]
#       [--version-output "<go version output>"]
#   scripts/verify-go-compiler.sh --self-test
#
# Exit 0 when the compiler is a stable release >= floor, non-zero otherwise.

set -euo pipefail

floor_override=''
go_mod='go.mod'
version_override=''
version_given=false
self_test=false

while [[ $# -gt 0 ]]; do
  case "$1" in
    --go-mod) go_mod="$2"; shift 2 ;;
    --floor) floor_override="$2"; shift 2 ;;
    --version-output) version_override="$2"; version_given=true; shift 2 ;;
    --self-test) self_test=true; shift ;;
    -h|--help)
      sed -n '1,20p' "$0"
      exit 0
      ;;
    *)
      printf 'usage: %s [--go-mod <path>] [--floor <x.y.z>] [--version-output "<go version output>"] [--self-test]\n' "$0" >&2
      exit 2
      ;;
  esac
done

floor_from_go_mod() {
  local mod_file=$1
  local line
  line=$(grep -E '^go [0-9]+\.[0-9]+\.[0-9]+$' "${mod_file}" | head -1 || true)
  if [[ -z "${line}" ]]; then
    printf 'verify-go-compiler: RED: cannot read floor from %s (want a `go x.y.z` line)\n' "${mod_file}" >&2
    return 1
  fi
  printf '%s' "${line#go }"
}

# Prints "<major> <minor> <patch>" for a stable version, fails otherwise.
parse_stable() {
  local version=$1
  if [[ "${version}" =~ ^([0-9]+)\.([0-9]+)\.([0-9]+)$ ]]; then
    # Reject leading-zero components (01, 02) as malformed SemVer.
    # Copy out first: the inner [[ =~ ]] below overwrites BASH_REMATCH.
    local major="${BASH_REMATCH[1]}" minor="${BASH_REMATCH[2]}" patch="${BASH_REMATCH[3]}"
    local i
    for i in "${major}" "${minor}" "${patch}"; do
      if [[ "${i}" =~ ^0[0-9]+$ ]]; then
        return 1
      fi
    done
    printf '%s %s %s' "${major}" "${minor}" "${patch}"
    return 0
  fi
  return 1
}

check_compiler() {
  local floor=$1
  local version_output=$2
  local raw floor_parts actual_parts
  local f_major f_minor f_patch a_major a_minor a_patch

  if ! floor_parts=$(parse_stable "${floor}"); then
    printf 'verify-go-compiler: RED: malformed floor %q (want x.y.z stable)\n' "${floor}" >&2
    return 1
  fi
  read -r f_major f_minor f_patch <<<"${floor_parts}"

  # Strict: `go version go<x.y.z> <os>/<arch>` with no prerelease suffix.
  # Anything else (missing go line, rc/beta suffix, extra fields) fails.
  if [[ ! "${version_output}" =~ ^go\ version\ go([0-9]+\.[0-9]+\.[0-9]+)\ ([a-z0-9]+/[a-z0-9]+)$ ]]; then
    printf 'verify-go-compiler: RED: malformed compiler output %q\n' "${version_output}" >&2
    return 1
  fi
  raw="${BASH_REMATCH[1]}"
  if ! actual_parts=$(parse_stable "${raw}"); then
    printf 'verify-go-compiler: RED: compiler version %q is not a stable release\n' "${raw}" >&2
    return 1
  fi
  read -r a_major a_minor a_patch <<<"${actual_parts}"

  if (( a_major != f_major )); then
    if (( a_major < f_major )); then
      printf 'verify-go-compiler: RED: go%s is older than floor go%s\n' "${raw}" "${floor}" >&2
      return 1
    fi
    printf 'Verified Go compiler go%s >= floor go%s\n' "${raw}" "${floor}"
    return 0
  fi
  if (( a_minor < f_minor )) || { (( a_minor == f_minor )) && (( a_patch < f_patch )); }; then
    printf 'verify-go-compiler: RED: go%s is older than floor go%s\n' "${raw}" "${floor}" >&2
    return 1
  fi
  printf 'Verified Go compiler go%s >= floor go%s\n' "${raw}" "${floor}"
}

run_self_test() {
  local failures=0
  pass() { printf '  PASS: %s\n' "$1"; }
  fail() { printf '  FAIL: %s\n' "$1" >&2; failures=$((failures + 1)); }

  if check_compiler '1.26.10' 'go version go1.26.10 darwin/arm64' >/dev/null; then pass 'floor passes'; else fail 'floor passes'; fi
  if check_compiler '1.26.10' 'go version go1.27.3 darwin/arm64' >/dev/null; then pass 'newer stable passes'; else fail 'newer stable passes'; fi
  if check_compiler '1.26.10' 'go version go1.26.9 darwin/arm64' >/dev/null; then fail 'old fails'; else pass 'old fails'; fi
  if check_compiler '1.26.10' '' >/dev/null 2>&1; then fail 'missing fails'; else pass 'missing fails'; fi
  if check_compiler '1.26.10' 'go version go1.26 darwin/arm64' >/dev/null 2>&1; then fail 'malformed fails'; else pass 'malformed fails'; fi
  if check_compiler '1.26.10' 'go version go1.26.10rc1 darwin/arm64' >/dev/null 2>&1; then fail 'prerelease fails'; else pass 'prerelease fails'; fi
  if check_compiler '1.26.10' 'go version go1.27.0-beta.1 linux/amd64' >/dev/null 2>&1; then fail 'beta fails'; else pass 'beta fails'; fi

  if (( failures > 0 )); then
    printf 'verify-go-compiler self-test: %d failure(s)\n' "${failures}" >&2
    return 1
  fi
  printf 'verify-go-compiler self-test: PASS\n'
}

if [[ "${self_test}" == true ]]; then
  run_self_test
  exit 0
fi

floor="${floor_override:-}"
if [[ -z "${floor}" ]]; then
  floor=$(floor_from_go_mod "${go_mod}")
fi

output="${version_override}"
if [[ "${version_given}" != true ]]; then
  if ! command -v go >/dev/null 2>&1; then
    printf 'verify-go-compiler: RED: go toolchain is missing from PATH\n' >&2
    exit 1
  fi
  # Independent root: never auto-download a toolchain to satisfy the check.
  output=$(GOTOOLCHAIN=local go version)
fi

check_compiler "${floor}" "${output}"
