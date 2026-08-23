#!/usr/bin/env bash
set -euo pipefail

released_sha=cd71337a87aea7cf0e1e877da3816d06f717e778
actual_sha=$(git rev-parse 'v0.68.1^{}')
if [[ "$actual_sha" != "$released_sha" ]]; then
  echo "v0.68.1 resolved to $actual_sha, want $released_sha" >&2
  exit 1
fi

overlap_tmp=$(mktemp -d "/tmp/donmai-shim-overlap.XXXXXX")
old_tree="$overlap_tmp/old"
registry="$overlap_tmp/registry"
old_binary="$overlap_tmp/old-shim"
new_binary="$overlap_tmp/new-controller"
old_pid=""
cleanup() {
  if [[ -n "$old_pid" ]]; then
    kill "$old_pid" 2>/dev/null || true
    wait "$old_pid" 2>/dev/null || true
  fi
  rm -rf "$overlap_tmp"
}
trap cleanup EXIT

mkdir -p "$old_tree/cmd/session-shim-overlap" "$registry"
git archive "$released_sha" | tar -x -C "$old_tree"
cp scripts/testdata/session-shim-overlap/oldshim/main.go "$old_tree/cmd/session-shim-overlap/main.go"

(cd "$old_tree" && GOWORK=off go build -o "$old_binary" ./cmd/session-shim-overlap)
GOWORK=off go build -o "$new_binary" ./scripts/testdata/session-shim-overlap/newcontroller

"$old_binary" "$registry" org-v0681 session-v0681 "$overlap_tmp/workarea" &
old_pid=$!

for _ in $(seq 1 400); do
  if [[ -n "$(find "$registry" -name '*.json' -print -quit)" ]]; then
    break
  fi
  sleep 0.025
done

"$new_binary" "$registry" org-v0681 session-v0681 "$overlap_tmp/workarea"
