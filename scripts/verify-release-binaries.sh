#!/usr/bin/env bash
# Inspect every release binary with `go version -m` before upload.
#
# The expected artifact set is derived from the GoReleaser config (builds:
# build id x binary name x goos x goarch), never hardcoded, so a new platform
# is covered automatically. Each binary must exist, must be a Go binary with module
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

# List expected binaries as "<binary> <id> <os> <arch>" lines, derived from
# the GoReleaser config. Fails closed when the config cannot be parsed.
# GoReleaser names each build directory after the build id
# (dist/<id>_<goos>_<goarch>*/<binary>), not after the binary name, so both
# are emitted for directory discovery.
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
  id = build['id'] || binary
  goos = build['goos'] || ['linux']
  goarch = build['goarch'] || ['amd64']
  abort "verify-release-binaries: RED: build entry without binary name" unless binary.is_a?(String) && !binary.empty?
  abort "verify-release-binaries: RED: build entry without id" unless id.is_a?(String) && !id.empty?
  goos.each do |os|
    goarch.each do |arch|
      puts "#{binary} #{id} #{os} #{arch}"
    end
  end
end
RUBY
}

locate_binary() {
  local name=$1 id=$2 os=$3 arch=$4
  local candidate
  # GoReleaser build layout: dist/<id>_<os>_<arch>*/<binary>.
  for candidate in "${dist}/${id}_${os}_${arch}"*/"${name}" "${dist}/${id}_${os}_${arch}/${name}"; do
    if [[ -f "${candidate}" ]]; then
      printf '%s' "${candidate}"
      return 0
    fi
  done
  return 1
}

# Hash one file and print its lowercase hex SHA256 digest. Fails closed when
# the hashing pipeline fails or the digest is not 64 hex characters. Both
# hash branches (shasum, sha256sum) check the pipeline status explicitly so a
# failing hasher cannot leave an empty digest, and validate the digest so a
# malformed hasher cannot pass. Called inside command substitution (which
# disables implicit errexit), so every failure is an explicit return 1.
hash_file() {
  local file=$1 digest
  if command -v shasum >/dev/null 2>&1; then
    if ! digest=$(shasum -a 256 "${file}" | awk '{print $1}'); then
      printf 'verify-release-binaries: RED: digest command failed for %s\n' "${file}" >&2
      return 1
    fi
  else
    if ! digest=$(sha256sum "${file}" | awk '{print $1}'); then
      printf 'verify-release-binaries: RED: digest command failed for %s\n' "${file}" >&2
      return 1
    fi
  fi
  if [[ ! "${digest}" =~ ^[0-9a-f]{64}$ ]]; then
    printf 'verify-release-binaries: RED: malformed digest %q for %s\n' "${digest}" "${file}" >&2
    return 1
  fi
  printf '%s' "${digest}"
}

verify_one() {
  local name=$1 id=$2 os=$3 arch=$4 floor=$5
  local path go_line go_version main_version sha
  if ! path=$(locate_binary "${name}" "${id}" "${os}" "${arch}"); then
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
  if ! sha=$(hash_file "${path}"); then
    return 1
  fi
  printf 'binary=%s os=%s arch=%s path=%s go=%s sha256=%s version=%s\n' \
    "${name}" "${os}" "${arch}" "${path}" "${go_version}" "${sha}" "${main_version:-unknown}"
  # GoReleaser strips the tag's leading `v` for `.Version`, which is what the
  # ldflags stamp. Normalize both sides so tag `v1.2.3` matches stamp `1.2.3`.
  if [[ -n "${tag}" && -n "${main_version}" ]]; then
    local want=${tag#v} have=${main_version#v}
    if [[ "${have}" != "${want}" ]]; then
      printf 'verify-release-binaries: RED: %s stamps version %q, want tag %q\n' "${path}" "${main_version}" "${tag}" >&2
      return 1
    fi
  fi
}

verify_dist() {
  local floor=$1
  local failures=0 count=0
  local name id os arch line
  # Capture the producer first: a config parse failure after emitting rows
  # must fail closed instead of verifying only the emitted prefix.
  local expected
  if ! expected=$(expected_binaries); then
    return 1
  fi
  while read -r name id os arch; do
    [[ -n "${name}" ]] || continue
    count=$((count + 1))
    if line=$(verify_one "${name}" "${id}" "${os}" "${arch}" "${floor}"); then
      printf '%s\n' "${line}"
      if [[ -n "${output_file}" ]]; then
        printf '%s\n' "${line}" >>"${output_file}"
      fi
    else
      failures=$((failures + 1))
    fi
  done <<<"${expected}"
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
    if ! digest=$(hash_file "${archive}"); then
      return 1
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
  mkdir -p "${work}/src" "${work}/dist/selftest-linux_linux_amd64_v1" "${work}/dist/selftest-darwin_darwin_arm64_v8.0"
  cat >"${work}/src/main.go" <<'GO'
package main
var version = "dev"
func main() {}
GO
  cat >"${work}/src/go.mod" <<'GO'
module example.com/selftest

go 1.26.10
GO
  # Stamp without the leading `v`, exactly like GoReleaser `{{.Version}}` (it strips the tag `v`).
  (cd "${work}/src" && GOWORK=off GOTOOLCHAIN=local go build -ldflags="-X main.version=1.2.3" -o "${work}/dist/selftest-linux_linux_amd64_v1/donmai" . )
  (cd "${work}/src" && GOWORK=off GOTOOLCHAIN=local go build -ldflags="-X main.version=1.2.3" -o "${work}/dist/selftest-darwin_darwin_arm64_v8.0/donmai" . )
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
  # Patch boundary: the security-relevant patch must refuse, not only major.
  if version_at_least "1.27.3" "1.27.4" >/dev/null 2>&1; then fail 'patch floor refuses'; else pass 'patch floor refuses'; fi
  if version_at_least "1.27.3" "1.27.3" >/dev/null; then pass 'patch floor passes'; else fail 'patch floor passes'; fi
  if (cd "${work}" && bash "${self_path}" --config goreleaser-trim.yaml --dist dist --tag v9.9.9 --floor "${floor}" >/dev/null 2>&1); then fail 'version mismatch refuses'; else pass 'version mismatch refuses'; fi
  # A config whose later entry omits `binary` must fail closed instead of
  # verifying only the emitted prefix.
  cat >"${work}/goreleaser-bad.yaml" <<'YAML'
version: 2
builds:
  - id: selftest-linux
    main: ./src
    binary: donmai
    goos: [linux]
    goarch: [amd64]
  - id: selftest-bad
    main: ./src
    goos: [linux]
    goarch: [amd64]
YAML
  if (cd "${work}" && bash "${self_path}" --config goreleaser-bad.yaml --dist dist --tag v1.2.3 --floor "${floor}" >/dev/null 2>&1); then fail 'missing binary name refuses'; else pass 'missing binary name refuses'; fi
  # A dist of good binaries with no archives is not shippable: the
  # empty-archive refusal must fail closed when the archives vanish.
  mkdir -p "${work}/hidden-archives"
  mv "${work}"/dist/*.tar.gz "${work}/hidden-archives/"
  if (cd "${work}" && bash "${self_path}" --config goreleaser-trim.yaml --dist dist --tag v1.2.3 --floor "${floor}" >/dev/null 2>&1); then fail 'missing archives refuses'; else pass 'missing archives refuses'; fi
  mv "${work}"/hidden-archives/*.tar.gz "${work}/dist/"
  mv "${work}/dist/selftest-linux_linux_amd64_v1/donmai" "${work}/dist/selftest-linux_linux_amd64_v1/donmai.bak"
  if (cd "${work}" && bash "${self_path}" --config goreleaser-trim.yaml --dist dist --tag v1.2.3 --floor "${floor}" >/dev/null 2>&1); then fail 'missing binary refuses'; else pass 'missing binary refuses'; fi
  mv "${work}/dist/selftest-linux_linux_amd64_v1/donmai.bak" "${work}/dist/selftest-linux_linux_amd64_v1/donmai"
  printf '#!/bin/sh\nexit 0\n' >"${work}/dist/selftest-linux_linux_amd64_v1/donmai-sh"
  mv "${work}/dist/selftest-linux_linux_amd64_v1/donmai" "${work}/dist/selftest-linux_linux_amd64_v1/donmai.real"
  cp "${work}/dist/selftest-linux_linux_amd64_v1/donmai-sh" "${work}/dist/selftest-linux_linux_amd64_v1/donmai"
  if (cd "${work}" && bash "${self_path}" --config goreleaser-trim.yaml --dist dist --tag v1.2.3 --floor "${floor}" >/dev/null 2>&1); then fail 'non-Go binary refuses'; else pass 'non-Go binary refuses'; fi
  mv "${work}/dist/selftest-linux_linux_amd64_v1/donmai.real" "${work}/dist/selftest-linux_linux_amd64_v1/donmai"
  # A failing hasher must fail closed instead of accepting an empty digest.
  mkdir -p "${work}/fakebin-fail"
  printf '#!/bin/sh\nexit 1\n' >"${work}/fakebin-fail/shasum"
  printf '#!/bin/sh\nexit 1\n' >"${work}/fakebin-fail/sha256sum"
  chmod +x "${work}/fakebin-fail/shasum" "${work}/fakebin-fail/sha256sum"
  if (cd "${work}" && PATH="${work}/fakebin-fail:${PATH}" bash "${self_path}" --config goreleaser-trim.yaml --dist dist --tag v1.2.3 --floor "${floor}" >/dev/null 2>&1); then fail 'digest error refuses'; else pass 'digest error refuses'; fi
  # A malformed hasher output must fail closed instead of passing.
  mkdir -p "${work}/fakebin-malformed"
  printf '#!/bin/sh\necho "not-a-digest  %s"\n' "x" >"${work}/fakebin-malformed/shasum"
  printf '#!/bin/sh\necho "not-a-digest  %s"\n' "x" >"${work}/fakebin-malformed/sha256sum"
  chmod +x "${work}/fakebin-malformed/shasum" "${work}/fakebin-malformed/sha256sum"
  if (cd "${work}" && PATH="${work}/fakebin-malformed:${PATH}" bash "${self_path}" --config goreleaser-trim.yaml --dist dist --tag v1.2.3 --floor "${floor}" >/dev/null 2>&1); then fail 'digest malformed refuses'; else pass 'digest malformed refuses'; fi

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
