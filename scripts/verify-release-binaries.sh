#!/usr/bin/env bash
# Inspect every release binary with `go version -m` before upload.
#
# The expected artifact set is derived from the GoReleaser config (builds:
# binary name x goos x goarch), never hardcoded, so a new platform is covered
# automatically. Each binary must exist, must be a Go binary with module
# metadata, and must report a stable compiler >= the go.mod floor. The script
# retains the SHA256 digest, Go version, and main.version for every binary.
#
# Usage:
#   scripts/verify-release-binaries.sh [--config <goreleaser.yaml>]
#       [--dist <dir>] [--tag <vX.Y.Z>] [--floor <x.y.z>] [--output <file>]
#   scripts/verify-release-binaries.sh --self-test
#
# Exit 0 when every derived binary verifies, non-zero otherwise.

set -euo pipefail

config='.goreleaser.yaml'
dist='dist'
tag=''
floor_override=''
output_file=''
self_test=false

while [[ $# -gt 0 ]]; do
  case "$1" in
    --config) config="$2"; shift 2 ;;
    --dist) dist="$2"; shift 2 ;;
    --tag) tag="$2"; shift 2 ;;
    --floor) floor_override="$2"; shift 2 ;;
    --output) output_file="$2"; shift 2 ;;
    --self-test) self_test=true; shift ;;
    -h|--help)
      sed -n '1,20p' "$0"
      exit 0
      ;;
    *)
      printf 'usage: %s [--config <goreleaser.yaml>] [--dist <dir>] [--tag <vX.Y.Z>] [--floor <x.y.z>] [--output <file>] [--self-test]\n' "$0" >&2
      exit 2
      ;;
  esac
done

floor_from_go_mod() {
  grep -E '^go [0-9]+\.[0-9]+\.[0-9]+$' go.mod | head -1 | cut -d' ' -f2
}

# Compare dotted stable versions. Returns 0 when $1 >= $2.
version_at_least() {
  local actual=$1 floor=$2
  local a_major a_minor a_patch f_major f_minor f_patch
  IFS=. read -r a_major a_minor a_patch <<<"${actual}"
  IFS=. read -r f_major f_minor f_patch <<<"${floor}"
  (( a_major > f_major )) && return 0
  (( a_major < f_major )) && return 1
  (( a_minor > f_minor )) && return 0
  (( a_minor < f_minor )) && return 1
  (( a_patch >= f_patch ))
}

# List expected binaries as "<binary> <os> <arch>" lines, derived from the
# GoReleaser config. Fails closed when the config cannot be parsed.
expected_binaries() {
  ruby -ryaml - "$config" <<'RUBY'
config_file = ARGV.fetch(0)
begin
  config = YAML.safe_load(File.read(config_file), permitted_classes: [], permitted_symbols: [], aliases: false)
rescue Psych::Exception, SystemCallError => error
  abort "verify-release-binaries: RED: cannot parse #{config_file}: #{error.message}"
end
builds = config['builds']
abort "verify-release-binaries: RED: #{config_file} declares no builds" unless builds.is_a?(Array) && !builds.empty?
builds.each do |build|
  binary = build['binary']
  goos = build['goos'] || ['linux']
  goarch = build['goarch'] || ['amd64']
  abort "verify-release-binaries: RED: build entry without binary name" unless binary.is_a?(String) && !binary.empty?
  goos.each do |os|
    goarch.each do |arch|
      puts "#{binary} #{os} #{arch}"
    end
  end
end
RUBY
}

locate_binary() {
  local name=$1 os=$2 arch=$3
  local candidate
  # GoReleaser build layout: dist/<name>_<os>_<arch>*/<name>.
  for candidate in "${dist}/${name}_${os}_${arch}"*/"${name}" "${dist}/${name}_${os}_${arch}/${name}"; do
    if [[ -f "${candidate}" ]]; then
      printf '%s' "${candidate}"
      return 0
    fi
  done
  return 1
}

verify_one() {
  local name=$1 os=$2 arch=$3 floor=$4
  local path go_line go_version main_version sha
  if ! path=$(locate_binary "${name}" "${os}" "${arch}"); then
    printf 'verify-release-binaries: RED: missing binary for %s %s/%s under %s\n' "${name}" "${os}" "${arch}" "${dist}" >&2
    return 1
  fi
  local meta
  if ! meta=$(go version -m "${path}" 2>&1); then
    printf 'verify-release-binaries: RED: %s has no Go build metadata (not a Go binary?)\n' "${path}" >&2
    return 1
  fi
  go_line=$(head -1 <<<"${meta}")
  if [[ ! "${go_line}" =~ :\ go([0-9]+\.[0-9]+\.[0-9]+)$ ]]; then
    printf 'verify-release-binaries: RED: %s reports malformed Go version %q\n' "${path}" "${go_line}" >&2
    return 1
  fi
  go_version="${BASH_REMATCH[1]}"
  if ! version_at_least "${go_version}" "${floor}"; then
    printf 'verify-release-binaries: RED: %s built with go%s, older than floor go%s\n' "${path}" "${go_version}" "${floor}" >&2
    return 1
  fi
  main_version=$(grep -E 'main\.version=' <<<"${meta}" | grep -Eo 'main\.version=[^ "\\]+' | head -1 || true)
  main_version=${main_version#main.version=}
  if command -v shasum >/dev/null 2>&1; then
    sha=$(shasum -a 256 "${path}" | awk '{print $1}')
  else
    sha=$(sha256sum "${path}" | awk '{print $1}')
  fi
  printf 'binary=%s os=%s arch=%s path=%s go=%s sha256=%s version=%s\n' \
    "${name}" "${os}" "${arch}" "${path}" "${go_version}" "${sha}" "${main_version:-unknown}"
  if [[ -n "${tag}" && -n "${main_version}" && "${main_version}" != "${tag}" ]]; then
    printf 'verify-release-binaries: RED: %s stamps version %q, want tag %q\n' "${path}" "${main_version}" "${tag}" >&2
    return 1
  fi
}

verify_dist() {
  local floor=$1
  local failures=0 count=0
  local name os arch line
  while read -r name os arch; do
    [[ -n "${name}" ]] || continue
    count=$((count + 1))
    if line=$(verify_one "${name}" "${os}" "${arch}" "${floor}"); then
      printf '%s\n' "${line}"
      if [[ -n "${output_file}" ]]; then
        printf '%s\n' "${line}" >>"${output_file}"
      fi
    else
      failures=$((failures + 1))
    fi
  done < <(expected_binaries)
  if (( count == 0 )); then
    printf 'verify-release-binaries: RED: config derived zero binaries\n' >&2
    return 1
  fi
  # Every published archive must exist and checksum cleanly.
  local archive
  shopt -s nullglob
  local archives=("${dist}"/*.tar.gz)
  shopt -u nullglob
  if (( ${#archives[@]} == 0 )); then
    printf 'verify-release-binaries: RED: no archives under %s\n' "${dist}" >&2
    return 1
  fi
  for archive in "${archives[@]}"; do
    local digest
    if command -v shasum >/dev/null 2>&1; then
      digest=$(shasum -a 256 "${archive}" | awk '{print $1}')
    else
      digest=$(sha256sum "${archive}" | awk '{print $1}')
    fi
    printf 'archive=%s sha256=%s\n' "$(basename "${archive}")" "${digest}"
    if [[ -n "${output_file}" ]]; then
      printf 'archive=%s sha256=%s\n' "$(basename "${archive}")" "${digest}" >>"${output_file}"
    fi
  done
  if (( failures > 0 )); then
    printf 'verify-release-binaries: RED: %d of %d binaries failed\n' "${failures}" "${count}" >&2
    return 1
  fi
  printf 'Verified %d binaries and %d archives (floor go%s)\n' "${count}" "${#archives[@]}" "${floor}"
}

run_self_test() {
  local self_path
  self_path=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/$(basename "${BASH_SOURCE[0]}")
  local failures=0
  pass() { printf '  PASS: %s\n' "$1"; }
  fail() { printf '  FAIL: %s\n' "$1" >&2; failures=$((failures + 1)); }

  local work
  work=$(mktemp -d)
  trap 'rm -rf "${work}"' RETURN
  mkdir -p "${work}/src" "${work}/dist/donmai_linux_amd64_v8.0" "${work}/dist/donmai_darwin_arm64_v8.0"
  cat >"${work}/src/main.go" <<'GO'
package main
var version = "dev"
func main() {}
GO
  cat >"${work}/src/go.mod" <<'GO'
module example.com/selftest

go 1.26.10
GO
  (cd "${work}/src" && GOWORK=off GOTOOLCHAIN=local go build -ldflags="-X main.version=v1.2.3" -o "${work}/dist/donmai_linux_amd64_v8.0/donmai" . )
  (cd "${work}/src" && GOWORK=off GOTOOLCHAIN=local go build -ldflags="-X main.version=v1.2.3" -o "${work}/dist/donmai_darwin_arm64_v8.0/donmai" . )
  printf 'payload' >"${work}/dist/donmai_1.2.3_linux_amd64.tar.gz"
  printf 'payload' >"${work}/dist/donmai_1.2.3_darwin_arm64.tar.gz"
  cat >"${work}/goreleaser.yaml" <<'YAML'
version: 2
builds:
  - id: selftest
    main: ./src
    binary: donmai
    goos: [linux, darwin]
    goarch: [amd64, arm64]
YAML
  # Trim to the two binaries actually built: linux/amd64 and darwin/arm64.
  cat >"${work}/goreleaser-trim.yaml" <<'YAML'
version: 2
builds:
  - id: selftest-linux
    main: ./src
    binary: donmai
    goos: [linux]
    goarch: [amd64]
  - id: selftest-darwin
    main: ./src
    binary: donmai
    goos: [darwin]
    goarch: [arm64]
YAML

  local floor
  floor=$(floor_from_go_mod)
  if (cd "${work}" && bash "${self_path}" --config goreleaser-trim.yaml --dist dist --tag v1.2.3 --floor "${floor}" >/dev/null); then pass 'real binaries pass'; else fail 'real binaries pass'; fi
  if (cd "${work}" && bash "${self_path}" --config goreleaser-trim.yaml --dist dist --tag v1.2.3 --floor 99.0.0 >/dev/null 2>&1); then fail 'old metadata refuses'; else pass 'old metadata refuses'; fi
  mv "${work}/dist/donmai_linux_amd64_v8.0/donmai" "${work}/dist/donmai_linux_amd64_v8.0/donmai.bak"
  if (cd "${work}" && bash "${self_path}" --config goreleaser-trim.yaml --dist dist --tag v1.2.3 --floor "${floor}" >/dev/null 2>&1); then fail 'missing binary refuses'; else pass 'missing binary refuses'; fi
  mv "${work}/dist/donmai_linux_amd64_v8.0/donmai.bak" "${work}/dist/donmai_linux_amd64_v8.0/donmai"
  printf '#!/bin/sh\nexit 0\n' >"${work}/dist/donmai_linux_amd64_v8.0/donmai-sh"
  mv "${work}/dist/donmai_linux_amd64_v8.0/donmai" "${work}/dist/donmai_linux_amd64_v8.0/donmai.real"
  cp "${work}/dist/donmai_linux_amd64_v8.0/donmai-sh" "${work}/dist/donmai_linux_amd64_v8.0/donmai"
  if (cd "${work}" && bash "${self_path}" --config goreleaser-trim.yaml --dist dist --tag v1.2.3 --floor "${floor}" >/dev/null 2>&1); then fail 'non-Go binary refuses'; else pass 'non-Go binary refuses'; fi
  mv "${work}/dist/donmai_linux_amd64_v8.0/donmai.real" "${work}/dist/donmai_linux_amd64_v8.0/donmai"

  if (( failures > 0 )); then
    printf 'verify-release-binaries self-test: %d failure(s)\n' "${failures}" >&2
    return 1
  fi
  printf 'verify-release-binaries self-test: PASS\n'
}

if [[ "${self_test}" == true ]]; then
  run_self_test
  exit 0
fi

floor="${floor_override:-}"
if [[ -z "${floor}" ]]; then
  floor=$(floor_from_go_mod)
  if [[ -z "${floor}" ]]; then
    printf 'verify-release-binaries: RED: cannot read Go floor from go.mod\n' >&2
    exit 1
  fi
fi
if [[ -n "${output_file}" ]]; then
  : >"${output_file}"
fi
verify_dist "${floor}"
