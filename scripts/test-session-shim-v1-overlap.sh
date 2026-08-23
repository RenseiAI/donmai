#!/usr/bin/env bash
set -euo pipefail

overlap_tmp=$(mktemp -d "/tmp/donmai-shim-overlap.XXXXXX")
new_binary="$overlap_tmp/new-controller"
new_shim_binary="$overlap_tmp/new-shim"
old_pids=()
cleanup() {
  for old_pid in "${old_pids[@]}"; do
    kill "$old_pid" 2>/dev/null || true
    wait "$old_pid" 2>/dev/null || true
  done
  rm -rf "$overlap_tmp"
}
trap cleanup EXIT

GOWORK=off go build -o "$new_binary" ./scripts/testdata/session-shim-overlap/newcontroller
GOWORK=off go build -o "$new_shim_binary" ./scripts/testdata/session-shim-overlap/newshim

run_overlap() {
  local tag=$1
  local released_sha=$2
  local selected_version=$3
  local suffix=$4
  local actual_sha
  actual_sha=$(git rev-parse "${tag}^{}")
  if [[ "$actual_sha" != "$released_sha" ]]; then
    echo "$tag resolved to $actual_sha, want $released_sha" >&2
    exit 1
  fi
  local old_tree="$overlap_tmp/old-$suffix"
  local registry="$overlap_tmp/registry-$suffix"
  local old_binary="$overlap_tmp/old-shim-$suffix"
  mkdir -p "$old_tree/cmd/session-shim-overlap" "$registry"
  git archive "$released_sha" | tar -x -C "$old_tree"
  cp scripts/testdata/session-shim-overlap/oldshim/main.go "$old_tree/cmd/session-shim-overlap/main.go"
  (cd "$old_tree" && GOWORK=off go build -o "$old_binary" ./cmd/session-shim-overlap)
  "$old_binary" "$registry" "org-$suffix" "session-$suffix" "$overlap_tmp/workarea-$suffix" &
  old_pids+=("$!")
  for _ in $(seq 1 400); do
    if [[ -n "$(find "$registry" -name '*.json' -print -quit)" ]]; then
      break
    fi
    sleep 0.025
  done
  "$new_binary" "$registry" "org-$suffix" "session-$suffix" "$overlap_tmp/workarea-$suffix" "$selected_version" "$released_sha"
}

run_reverse_overlap() {
  local tag=$1
  local released_sha=$2
  local suffix=$3
  local actual_sha
  actual_sha=$(git rev-parse "${tag}^{}")
  if [[ "$actual_sha" != "$released_sha" ]]; then
    echo "$tag resolved to $actual_sha, want $released_sha" >&2
    exit 1
  fi
  local old_tree="$overlap_tmp/reverse-old-$suffix"
  local registry="$overlap_tmp/reverse-registry-$suffix"
  local old_binary="$overlap_tmp/old-controller-$suffix"
  local workarea="$overlap_tmp/reverse-workarea-$suffix"
  mkdir -p "$old_tree/cmd/session-shim-overlap" "$registry"
  git archive "$released_sha" | tar -x -C "$old_tree"
  cp scripts/testdata/session-shim-overlap/oldcontroller/main.go "$old_tree/cmd/session-shim-overlap/main.go"
  (cd "$old_tree" && GOWORK=off go build -o "$old_binary" ./cmd/session-shim-overlap)
  "$new_shim_binary" "$registry" "org-reverse-$suffix" "session-reverse-$suffix" "$workarea" &
  old_pids+=("$!")
  for _ in $(seq 1 400); do
    if [[ -n "$(find "$registry" -name '*.json' -print -quit)" ]]; then
      break
    fi
    sleep 0.025
  done
  "$old_binary" "$registry" "org-reverse-$suffix" "session-reverse-$suffix" "$workarea" "$released_sha"
  if [[ -z "$(find "$registry" -name '*.ack' -print -quit)" ]]; then
    echo "new shim did not publish the durable ACK sidecar" >&2
    exit 1
  fi
  # A second controller built entirely from released v0.68.2 must ignore the
  # unrecognized non-.json sidecar, scan the frozen Record, and adopt normally.
  "$old_binary" "$registry" "org-reverse-$suffix" "session-reverse-$suffix" "$workarea" "$released_sha"
}

run_overlap v0.68.1 cd71337a87aea7cf0e1e877da3816d06f717e778 1 v0681
run_overlap v0.68.2 65d80580fabdb6a469436965e9e17990cda23716 2 v0682
run_reverse_overlap v0.68.2 65d80580fabdb6a469436965e9e17990cda23716 v0682
