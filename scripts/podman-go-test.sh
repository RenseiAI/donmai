#!/usr/bin/env bash
# podman-go-test.sh — run this module's Go tests inside a Linux podman
# container (the fast lane: `make test-podman`, and the test gate of
# `make ship` and `make release`).
#
# Why a container: CI runs the suite on Linux, and on a macOS host with a live
# daemon the daemon install/uninstall tests can boot out the developer's own
# launchd service (see AGENTS.md, Gotchas). Inside the container there is no
# launchd and no live daemon to reach, so the whole suite runs the way CI runs
# it and never touches this host's service.
#
# The worktree and the repository's git common directory are mounted at their
# host paths, so tests that read git history (tags, the changelog baseline) see
# the same repository they would see on the host. Module and build caches live
# in a named podman volume and survive between runs.
#
# Usage: scripts/podman-go-test.sh [go test arguments]   (default: -race ./...)
#
# The image is built once per Go version from the official golang image plus
# the fixture tools the script tests need (jq, ssh-keygen, ruby) and reused.

set -euo pipefail

die() {
  printf 'podman-go-test: %s\n' "$*" >&2
  exit 1
}

command -v podman >/dev/null 2>&1 || die "podman is required (brew install podman && podman machine start)"
command -v git >/dev/null 2>&1 || die "git is required"

root="$(git rev-parse --show-toplevel)" || die "run inside the repository"
common="$(cd "$(git rev-parse --git-common-dir)" && pwd -P)" || die "cannot resolve the git common directory"
root="$(cd "${root}" && pwd -P)"

# The toolchain line wins over the go line, as it does for setup-go and the go
# command itself.
go_version="$(awk '$1 == "toolchain" { sub(/^go/, "", $2); print $2; exit }' "${root}/go.mod")"
if [[ -z "${go_version}" ]]; then
  go_version="$(awk '$1 == "go" { print $2; exit }' "${root}/go.mod")"
fi
[[ "${go_version}" =~ ^[0-9]+\.[0-9]+(\.[0-9]+)?$ ]] || die "cannot read the Go version from go.mod (got '${go_version}')"

image="localhost/donmai-go-test:go${go_version}"
cache_volume="donmai-go-test-cache"

if ! podman image exists "${image}"; then
  # Removed right after the build: the exec below would skip an EXIT trap.
  context="$(mktemp -d)"
  printf 'podman-go-test: building %s (once per Go version)\n' "${image}"
  build_status=0
  podman build --quiet --tag "${image}" --file - "${context}" <<CONTAINERFILE >/dev/null || build_status=$?
FROM docker.io/library/golang:${go_version}-bookworm
RUN apt-get update \\
 && apt-get install -y --no-install-recommends jq openssh-client python3 ruby \\
 && rm -rf /var/lib/apt/lists/*
CONTAINERFILE
  rm -rf -- "${context}"
  [[ "${build_status}" -eq 0 ]] || die "podman build of ${image} failed (exit ${build_status})"
fi

if [[ $# -eq 0 ]]; then
  set -- -race ./...
fi

# Run as a non-root user, as CI does: a root test process passes permission
# checks CI enforces (a read-only directory is writable to root). A podman
# machine is rootful by default, so --userns=keep-id would still be root there.
uid="$(id -u)"
gid="$(id -g)"
if [[ "${uid}" == 0 ]]; then
  uid=1000
  gid=1000
fi

printf 'podman-go-test: go test %s (Linux container, go%s)\n' "$*" "${go_version}"
# --rm enables Podman's volatile overlay optimization, which omits fsync.
# Archive and journal tests need real filesystem sync semantics. Retain the
# container until the test exits, then remove only the ID recorded by this run.
container_receipt="$(mktemp -d)"
container_client_pid=''
container_creating=1
cleanup() {
  local status=$? container_id cleanup_failed=0
  trap - EXIT INT TERM
  # Let an in-flight create finish writing its receipt. It cannot start the
  # test workload; killing its client could abandon a server-side creation.
  if [[ -n "${container_client_pid}" && "${container_creating}" -eq 1 ]]; then
    wait "${container_client_pid}" 2>/dev/null || true
  fi
  if [[ -s "${container_receipt}/cid" ]]; then
    container_id="$(cat "${container_receipt}/cid")"
    if [[ ! "${container_id}" =~ ^[0-9a-f]{64}$ ]]; then
      printf 'podman-go-test: invalid container ID receipt\n' >&2
      [[ "${status}" -ne 0 ]] || status=1
    elif ! podman rm --force "${container_id}" >/dev/null; then
      printf 'podman-go-test: cleanup failed for container %s\n' "${container_id}" >&2
      cleanup_failed=1
      [[ "${status}" -ne 0 ]] || status=1
    fi
  fi
  if [[ -n "${container_client_pid}" ]]; then
    if [[ "${cleanup_failed}" -ne 0 ]]; then
      kill -TERM "${container_client_pid}" 2>/dev/null || true
    fi
    wait "${container_client_pid}" 2>/dev/null || true
  fi
  rm -rf -- "${container_receipt}"
  exit "${status}"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
# --init: a real PID 1 reaps orphaned grandchildren, as on a CI runner; with go
# test as PID 1 a killed orphan stays a zombie and process-group tests fail.
podman create --cidfile "${container_receipt}/cid" --init --user "${uid}:${gid}" \
  --volume "${root}:${root}" \
  --volume "${common}:${common}" \
  --volume "${cache_volume}:/cache:U" \
  --workdir "${root}" \
  --env HOME=/cache/home \
  --env GOCACHE=/cache/build \
  --env GOMODCACHE=/cache/mod \
  --env GOPATH=/cache/gopath \
  --env GOWORK=off \
  --env GOTOOLCHAIN=local \
  --env GIT_CONFIG_COUNT=1 \
  --env GIT_CONFIG_KEY_0=safe.directory \
  --env GIT_CONFIG_VALUE_0='*' \
  "${image}" \
  sh -c 'mkdir -p "$HOME" && exec go test "$@"' go-test "$@" >/dev/null &
container_client_pid=$!
wait "${container_client_pid}"
container_id="$(cat "${container_receipt}/cid")"
[[ "${container_id}" =~ ^[0-9a-f]{64}$ ]] || die 'invalid container ID receipt'
container_creating=0
podman start --attach "${container_id}" &
container_client_pid=$!
wait "${container_client_pid}"
