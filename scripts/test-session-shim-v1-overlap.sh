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
  new_shim_pid="$!"
  for _ in $(seq 1 400); do
    if [[ -n "$(find "$registry" -name '*.json' -print -quit)" ]]; then
      break
    fi
    sleep 0.025
  done
  # The Linux start-time unit changed from raw /proc ticks to Unix
  # nanoseconds, so a released controller (raw-tick comparison) cannot match
  # a new-format record. It must leave the record stale — fail closed, kill
  # nothing — and upgrades stay controller-first. Other platforms never
  # changed units, so adoption still succeeds there.
  if [[ "$(uname -s)" == "Linux" ]]; then
    reverse_out="$overlap_tmp/reverse-$suffix.out"
    if "$old_binary" "$registry" "org-reverse-$suffix" "session-reverse-$suffix" "$workarea" "$released_sha" >"$reverse_out" 2>&1; then
      echo "released controller unexpectedly adopted a new-format record on Linux" >&2
      cat "$reverse_out" >&2
      exit 1
    fi
    if ! grep -q "adopted=0" "$reverse_out"; then
      echo "released controller failed without the pinned adopted=0 stale refusal" >&2
      cat "$reverse_out" >&2
      exit 1
    fi
    if [[ -n "$(find "$registry" -name '*.ack' -print -quit)" ]]; then
      echo "selected-v2 overlap unexpectedly published a v3-only ACK sidecar" >&2
      exit 1
    fi
    if [[ -z "$(find "$registry" -name '*.json' -print -quit)" ]]; then
      echo "released controller removed a record it refused to adopt" >&2
      exit 1
    fi
    if ! kill -0 "$new_shim_pid" 2>/dev/null; then
      echo "released controller killed a shim it refused to adopt" >&2
      exit 1
    fi
    # A released v0.68.2 scanner ignores unknown non-.json entries: the corrupt
    # sidecar-shaped sentinel must leave the outcome unchanged (same stale
    # refusal, no crash on the unknown file).
    printf '%s' 'corrupt-v3-only-sidecar' > "$registry/overlap-corrupt.ack"
    if "$old_binary" "$registry" "org-reverse-$suffix" "session-reverse-$suffix" "$workarea" "$released_sha" >"$reverse_out" 2>&1; then
      echo "released controller unexpectedly adopted a new-format record on Linux" >&2
      cat "$reverse_out" >&2
      exit 1
    fi
    if ! grep -q "adopted=0" "$reverse_out"; then
      echo "released controller failed without the pinned adopted=0 stale refusal" >&2
      cat "$reverse_out" >&2
      exit 1
    fi
    echo "PINNED-REVERSE-OVERLAP documented-break old=$released_sha selected=2 new_format_record=stale_not_adopted"
    return 0
  fi
  "$old_binary" "$registry" "org-reverse-$suffix" "session-reverse-$suffix" "$workarea" "$released_sha"
  if [[ -n "$(find "$registry" -name '*.ack' -print -quit)" ]]; then
    echo "selected-v2 overlap unexpectedly published a v3-only ACK sidecar" >&2
    exit 1
  fi
  # A released v0.68.2 scanner ignores unknown non-.json entries. This corrupt
  # sidecar-shaped sentinel also proves the reverse-overlap scan remains frozen.
  printf '%s' 'corrupt-v3-only-sidecar' > "$registry/overlap-corrupt.ack"
  "$old_binary" "$registry" "org-reverse-$suffix" "session-reverse-$suffix" "$workarea" "$released_sha"
}

run_overlap v0.68.1 cd71337a87aea7cf0e1e877da3816d06f717e778 1 v0681
run_overlap v0.68.2 65d80580fabdb6a469436965e9e17990cda23716 2 v0682
run_reverse_overlap v0.68.2 65d80580fabdb6a469436965e9e17990cda23716 v0682
